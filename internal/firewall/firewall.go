// Package firewall 让落地 agent 按面板下发的喂入机列表收敛服务端口访问规则：
// 面板算出"谁（接入机 IP）可以访问哪些端口（backend）"，agent 幂等对齐 firewalld
// rich rules——多入口增减自动同步，替代手工 `firewall-cmd --add-rich-rule`。
//
// 只管理符合本包形状（family=ipv4 + source address + port + protocol）的规则，
// 管理员手工添加的其他规则不动；firewalld 不存在时跳过（fail-safe）。
package firewall

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

type PortRule struct {
	Port  int    `json:"port"`
	Proto string `json:"proto"` // tcp | udp | both
}

type AllowRule struct {
	Source string     `json:"source"`
	Ports  []PortRule `json:"ports"`
}

type Spec struct {
	Managed bool        `json:"managed"`
	Allow   []AllowRule `json:"allow,omitempty"`
}

// 单条规则的规范文本（既是下发目标也是比对键）
func ruleText(source string, port int, proto string) string {
	// firewalld 规范形式（--list-rich-rules 返回带引号的值）——生成与比对必须同构，
	// 否则每周期都"新增"且旧规则永不识别为可管理（2026-10-11 实测教训）
	return fmt.Sprintf(`rule family="ipv4" source address="%s" port port="%d" protocol="%s" accept`, source, port, proto)
}

// desiredRules 展开 spec → 规则文本集合（both 拆 tcp+udp）
func desiredRules(s Spec) map[string]bool {
	out := map[string]bool{}
	for _, a := range s.Allow {
		for _, p := range a.Ports {
			protos := []string{p.Proto}
			if p.Proto == "both" || p.Proto == "" {
				protos = []string{"tcp", "udp"}
			}
			for _, pr := range protos {
				out[ruleText(a.Source, p.Port, pr)] = true
			}
		}
	}
	return out
}

var ruleRe = regexp.MustCompile(`^rule family="ipv4" source address="([^"]+)" port port="(\d+)" protocol="(tcp|udp)" accept$`)
var ruleReLegacy = regexp.MustCompile(`^rule family=ipv4 source address=(\S+) port port=(\d+) protocol=(tcp|udp) accept$`)

// managedShape 识别"我们形状"的现存规则（不管谁加的，形状一致即纳入管理——
// 与面板期望不一致的会被收敛掉，保证唯一事实源）
func parseManaged(rules []string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range rules {
		r := strings.TrimSpace(raw)
		if m := ruleRe.FindStringSubmatch(r); m != nil {
			out[ruleText(m[1], atoi(m[2]), m[3])] = true // 规范化为引号形式
		} else if m := ruleReLegacy.FindStringSubmatch(r); m != nil {
			out[ruleText(m[1], atoi(m[2]), m[3])] = true
		}
	}
	return out
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func firewalld(args ...string) (string, error) {
	ctx := exec.Command("firewall-cmd", args...)
	out, err := ctx.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Apply 幂等收敛：期望集合 vs 现存（permanent）形状规则，增删差异后 reload。
func Apply(s Spec) error {
	if !s.Managed {
		return nil
	}
	if _, err := firewalld("--state"); err != nil {
		nlog.Core().Debug("firewall: firewalld not active, skip")
		return nil
	}

	listOut, err := firewalld("--permanent", "--list-rich-rules")
	if err != nil {
		return fmt.Errorf("list rich rules: %w", err)
	}
	current := parseManaged(strings.Split(listOut, "\n"))
	desired := desiredRules(s)

	var toAdd, toRemove []string
	for r := range desired {
		if !current[r] {
			toAdd = append(toAdd, r)
		}
	}
	for r := range current {
		if !desired[r] {
			toRemove = append(toRemove, r)
		}
	}
	if len(toAdd) == 0 && len(toRemove) == 0 {
		return nil
	}

	sort.Strings(toAdd)
	sort.Strings(toRemove)
	for _, r := range toRemove {
		if _, err := firewalld("--permanent", "--remove-rich-rule="+r); err != nil {
			nlog.Core().Warn("firewall: remove rule failed", "rule", r, "error", err)
		}
	}
	for _, r := range toAdd {
		if _, err := firewalld("--permanent", "--add-rich-rule="+r); err != nil {
			nlog.Core().Error("firewall: add rule failed", "rule", r, "error", err)
			return fmt.Errorf("add rule %q: %w", r, err)
		}
	}
	if _, err := firewalld("--reload"); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	nlog.Core().Info("firewall: rules reconciled", "added", len(toAdd), "removed", len(toRemove))
	return nil
}

func diff(desired, current map[string]bool) (toAdd, toRemove []string) {
	for r := range desired {
		if !current[r] {
			toAdd = append(toAdd, r)
		}
	}
	for r := range current {
		if !desired[r] {
			toRemove = append(toRemove, r)
		}
	}
	return
}
