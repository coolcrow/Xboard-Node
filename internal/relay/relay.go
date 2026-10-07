// Package relay 让 agent 像管理代理内核一样管理 realm 中转：
// 面板下发 Spec（sync.relay 推送 / machine/nodes 基线拉取）→
// 渲染 /etc/realm/*.toml → 供给二进制（sha256 固定）→ 管理 systemd 实例。
//
// 文件格式与 tools-relay/relay-setup.sh 完全兼容：手动脚本与 agent 可互相接手。
package relay

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

const (
	realmVer     = "2.9.6"
	realmSHA256  = "6a1733bfc51e3d19743757ca08f4c81f36afe7972eab0607bef264fc99064208"
	realmURL     = "https://github.com/zhboner/realm/releases/download/v" + realmVer + "/realm-x86_64-unknown-linux-musl.tar.gz"
	binPath      = "/etc/xboard-node/bin/realm" // agent 沙箱可写区，与主二进制同住
	etcDir       = "/etc/xboard-node/realm"
	unitPath     = "/etc/systemd/system/realm-relay@.service"
	downloadTOSE = 120 * time.Second
)

// Spec 是面板下发的 relay 规格（JSON 与面板 ServerMachine::relaySpec() 对齐）。
type Spec struct {
	Enabled          bool   `json:"enabled"`
	LandingHost      string `json:"landing_host"`
	Ports            string `json:"ports"`
	LandingMachineID int    `json:"landing_machine_id"`
}

// InstanceStatus 是单个 systemd 实例的状态。
type InstanceStatus struct {
	Name   string `json:"name"`   // dual / tcp / udp
	Active bool   `json:"active"` // systemd active 状态
}

// Status 是上报面板的整体状态。
type Status struct {
	Managed   bool             `json:"managed"`             // 本机存在 relay 配置
	Binary    bool             `json:"binary"`              // realm 二进制就绪
	Version   string           `json:"version,omitempty"`   // realm --version 输出
	Instances []InstanceStatus `json:"instances,omitempty"` // 各协议实例
	Error     string           `json:"error,omitempty"`     // 最近一次 Apply 错误
	AppliedAt int64            `json:"applied_at,omitempty"`
}

var lastError string

// buckets 把端口字符串按协议分桶。语法与 relay-setup.sh 一致：
// "443, 8443/tcp, 53/udp"（无后缀 = 双协议）。
func buckets(raw string) (dual, tcp, udp []int, err error) {
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		var portStr, proto string
		if i := strings.IndexByte(item, '/'); i >= 0 {
			portStr, proto = item[:i], strings.ToLower(item[i+1:])
		} else {
			portStr, proto = item, "dual"
		}
		port, perr := strconv.Atoi(portStr)
		if perr != nil || port < 1 || port > 65535 {
			return nil, nil, nil, fmt.Errorf("invalid port %q", item)
		}
		switch proto {
		case "dual":
			dual = append(dual, port)
		case "tcp":
			tcp = append(tcp, port)
		case "udp":
			udp = append(udp, port)
		default:
			return nil, nil, nil, fmt.Errorf("invalid proto %q (want tcp/udp or none)", item)
		}
	}
	if len(dual)+len(tcp)+len(udp) == 0 {
		return nil, nil, nil, fmt.Errorf("no ports")
	}
	return dual, tcp, udp, nil
}

// render 生成单个 toml 文件内容（与 relay-setup.sh gen_conf 输出逐字节兼容）。
func render(landing string, noTCP, useUDP bool, ports []int) string {
	var b strings.Builder
	b.WriteString("[network]\n")
	b.WriteString(fmt.Sprintf("no_tcp = %v\nuse_udp = %v\n", noTCP, useUDP))
	b.WriteString("\n# managed by xboard-node agent (sync.relay)\n")
	for _, p := range ports {
		b.WriteString(fmt.Sprintf("[[endpoints]]\nlisten = \"0.0.0.0:%d\"\nremote = \"%s:%d\"\n", p, landing, p))
	}
	return b.String()
}

// files 返回需要落盘的 toml 集合（只含非空桶）。
func files(s Spec) (map[string]string, error) {
	dual, tcp, udp, err := buckets(s.Ports)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if len(dual) > 0 {
		out["dual"] = render(s.LandingHost, false, true, dual)
	}
	if len(tcp) > 0 {
		out["tcp"] = render(s.LandingHost, false, false, tcp)
	}
	if len(udp) > 0 {
		out["udp"] = render(s.LandingHost, true, true, udp)
	}
	return out, nil
}

func unitContent() string {
	return `[Unit]
Description=AIBolt relay (realm %i)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + binPath + ` -c ` + etcDir + `/%i.toml
Restart=always
RestartSec=3
LimitNOFILE=1048576
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=` + etcDir + `
ProtectHome=true

[Install]
WantedBy=multi-user.target
`
}

func systemctl(args ...string) (string, error) {
	ctx := exec.Command("systemctl", args...)
	out, err := ctx.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// EnsureBinary 确保 realm 二进制存在且 sha256 匹配；缺失则下载（tar.gz 内单二进制）。
func EnsureBinary() error {
	if actual, err := fileSHA(binPath); err == nil && actual == realmSHA256 {
		return nil
	}
	nlog.Core().Info("relay: downloading realm", "url", realmURL)

	tmp, err := os.MkdirTemp("", "realm-dl-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	tgz := filepath.Join(tmp, "realm.tgz")
	if err := download(realmURL, tgz, downloadTOSE); err != nil {
		return fmt.Errorf("download realm: %w", err)
	}
	if err := extractRealm(tgz, filepath.Join(tmp, "realm")); err != nil {
		return err
	}
	actual, err := fileSHA(filepath.Join(tmp, "realm"))
	if err != nil || actual != realmSHA256 {
		return fmt.Errorf("realm sha256 mismatch: want %s got %s", realmSHA256, actual)
	}
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		return err
	}
	if err := copyMode(filepath.Join(tmp, "realm"), binPath, 0o755); err != nil {
		return err
	}
	nlog.Core().Info("relay: realm binary provisioned", "version", realmVer)
	return nil
}

// ensureUnit 写入 systemd 模板单元（幂等，变化时 daemon-reload）。
func ensureUnit() error {
	want := unitContent()
	if cur, err := os.ReadFile(unitPath); err == nil && string(cur) == want {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(want), 0o644); err != nil {
		return err
	}
	if _, err := systemctl("daemon-reload"); err != nil {
		nlog.Core().Warn("relay: daemon-reload failed", "error", err)
	}
	return nil
}

// Apply 是唯一入口：spec 未启用 → 拆除；启用 → 全量对齐（幂等）。
func Apply(s Spec) error {
	if !s.Enabled || s.LandingHost == "" || strings.TrimSpace(s.Ports) == "" {
		err := Disable()
		setErr(err, s)
		return err
	}

	want, err := files(s)
	if err != nil {
		setErr(err, s)
		return err
	}
	if err := os.MkdirAll(etcDir, 0o700); err != nil {
		setErr(err, s)
		return err
	}
	if err := EnsureBinary(); err != nil {
		setErr(err, s)
		return err
	}
	if err := ensureUnit(); err != nil {
		setErr(err, s)
		return err
	}

	changed := false
	// 对齐三个协议桶：有配置→写盘+确保运行；无配置→停用+清理
	for _, name := range []string{"dual", "tcp", "udp"} {
		unit := "realm-relay@" + name
		confPath := filepath.Join(etcDir, name+".toml")

		content, has := want[name]
		if !has {
			_, _ = systemctl("stop", unit)
			_, _ = systemctl("disable", unit)
			os.Remove(confPath)
			continue
		}

		if cur, rerr := os.ReadFile(confPath); rerr != nil || string(cur) != content {
			if err := os.WriteFile(confPath, []byte(content), 0o600); err != nil {
				setErr(err, s)
				return err
			}
			changed = true
		}
		if changed {
			if _, err := systemctl("enable", unit); err != nil {
				nlog.Core().Warn("relay: enable failed", "unit", unit, "error", err)
			}
			if _, err := systemctl("restart", unit); err != nil {
				setErr(fmt.Errorf("restart %s: %w", unit, err), s)
				return err
			}
			nlog.Core().Info("relay: instance applied", "unit", unit, "landing", s.LandingHost, "ports", s.Ports)
		}
	}
	if !changed {
		nlog.Core().Debug("relay: config unchanged, no restart")
	}
	lastError = ""
	return nil
}

// Disable 停用全部 relay 实例并移除配置（保留二进制与单元模板）。
func Disable() error {
	for _, name := range []string{"dual", "tcp", "udp"} {
		unit := "realm-relay@" + name
		_, _ = systemctl("stop", unit)
		_, _ = systemctl("disable", unit)
		os.Remove(filepath.Join(etcDir, name+".toml"))
	}
	return nil
}

// Collect 采集当前状态（状态上报用），绝不返回 error——状态缺失不能影响心跳。
func Collect() Status {
	st := Status{Instances: []InstanceStatus{}}

	if _, err := os.Stat(filepath.Join(etcDir)); err == nil {
		for _, name := range []string{"dual", "tcp", "udp"} {
			if _, err := os.Stat(filepath.Join(etcDir, name+".toml")); err == nil {
				st.Managed = true
				active := false
				if out, err := systemctl("is-active", "realm-relay@"+name); err == nil && out == "active" {
					active = true
				}
				st.Instances = append(st.Instances, InstanceStatus{Name: name, Active: active})
			}
		}
	}

	if _, err := os.Stat(binPath); err == nil {
		st.Binary = true
		ctx := exec.Command(binPath, "--version")
		if out, err := ctx.CombinedOutput(); err == nil {
			if f := strings.Fields(strings.TrimSpace(string(out))); len(f) >= 2 {
				st.Version = f[0] + " " + f[1] // "Realm 2.9.6"
			} else if len(f) == 1 {
				st.Version = f[0]
			}
		}
	}
	st.Error = lastError
	return st
}

func setErr(err error, s Spec) {
	if err != nil {
		lastError = err.Error()
		nlog.Core().Error("relay: apply failed", "error", err, "landing", s.LandingHost, "ports", s.Ports)
	}
}

func fileSHA(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// extractRealm 从 tar.gz 中抽出名为 realm 的单文件（纯 Go，宿主机无需 tar）。
func extractRealm(tgz, dest string) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("realm gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("realm binary not found in archive")
		}
		if err != nil {
			return fmt.Errorf("realm tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "realm" {
			continue
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
}

// copyMode 复制文件并设置权限（替代外部 install 命令）。
func copyMode(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
