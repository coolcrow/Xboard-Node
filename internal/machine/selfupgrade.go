package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cedar2025/xboard-node/internal/buildinfo"
	"github.com/cedar2025/xboard-node/internal/distribution"
	"github.com/cedar2025/xboard-node/internal/nlog"
)

// selfUpgrade 执行面板下发的 agent 自升级：
// 下载 → SHA256 强制校验 → 试执行 → 停内核 → 原子换核 → 退出交给 systemd 拉起新版本。
// 任何一步失败都不动在跑的二进制（agent 继续以旧版本服务），状态经
// upgradeStatus 上报到机器心跳，面板按钮侧可见进度与失败原因。
func (o *Orchestrator) selfUpgrade(version, shaAMD64, shaARM64 string) {
	if !o.upgradeMu.TryLock() {
		o.upgradeStatus.Store("upgrade rejected: another upgrade in progress")
		nlog.Core().Warn("control.upgrade skipped: upgrade already running", "version", version)
		return
	}
	defer o.upgradeMu.Unlock()
	o.upgradeStatus.Store("upgrading: " + version)
	defer func() {
		if r := recover(); r != nil {
			o.upgradeStatus.Store(fmt.Sprintf("upgrade failed: panic %v", r))
		}
	}()

	sha := shaForArch(shaAMD64, shaARM64)
	if version == "" || sha == "" {
		o.upgradeStatus.Store("upgrade rejected: version or sha256 for this arch missing")
		nlog.Core().Warn("control.upgrade rejected: no pinned sha256 for this arch",
			"version", version, "arch", runtime.GOARCH)
		return
	}
	if !validSHA256(sha) {
		o.upgradeStatus.Store("upgrade rejected: invalid sha256 format")
		nlog.Core().Warn("control.upgrade rejected: malformed sha256", "sha256", sha)
		return
	}

	if o.cfg.Machine != nil {
		distribution.SetBase(o.cfg.Machine.UpgradeDownloadBase)
	}
	artifact := fmt.Sprintf("xboard-node-%s-%s", runtime.GOOS, runtime.GOARCH)
	url := distribution.ArtifactURL(artifact, version)
	nlog.Core().Info("agent self-upgrade starting", "version", version, "url", url, "arch", runtime.GOARCH)

	live, err := os.Executable()
	if err != nil {
		o.upgradeStatus.Store("upgrade failed: resolve executable: " + err.Error())
		return
	}
	live, err = filepath.EvalSymlinks(live)
	if err != nil {
		live = filepath.Clean(live)
	}
	dir := filepath.Dir(live)
	staged := filepath.Join(dir, ".xboard-node.upgrade")

	o.upgradeStatus.Store("upgrading: downloading")
	if err := download(url, staged); err != nil {
		o.upgradeStatus.Store("upgrade failed: download: " + err.Error())
		nlog.Core().Error("self-upgrade download failed", "error", err)
		return
	}

	o.upgradeStatus.Store("upgrading: verifying sha256")
	actual, err := fileSHA256(staged)
	if err != nil || actual != sha {
		os.Remove(staged)
		o.upgradeStatus.Store("upgrade failed: sha256 mismatch")
		nlog.Core().Error("self-upgrade sha256 mismatch", "want", sha, "got", actual)
		return
	}

	o.upgradeStatus.Store("upgrading: validating binary")
	if err := os.Chmod(staged, 0o755); err != nil {
		os.Remove(staged)
		o.upgradeStatus.Store("upgrade failed: chmod: " + err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	out, err := exec.CommandContext(ctx, staged, "-v").CombinedOutput()
	cancel()
	if err != nil {
		os.Remove(staged)
		o.upgradeStatus.Store("upgrade failed: binary validation: " + strings.TrimSpace(string(out)))
		nlog.Core().Error("self-upgrade binary failed -v check", "error", err, "output", string(out))
		return
	}

	// 从这里开始进入不可逆段：先停内核，再换核，退出交给 supervisor。
	o.upgradeStatus.Store("upgrading: swapping binary")
	backup := filepath.Join(dir, fmt.Sprintf(".xboard-node.pre-upgrade.%d", time.Now().Unix()))
	if err := os.Rename(live, backup); err != nil {
		o.upgradeStatus.Store("upgrade failed: swap: " + err.Error())
		nlog.Core().Error("self-upgrade swap failed", "error", err)
		return
	}
	if err := os.Rename(staged, live); err != nil {
		// 换核失败立即回滚
		os.Rename(backup, live)
		o.upgradeStatus.Store("upgrade failed: swap staged: " + err.Error())
		return
	}
	nlog.Core().Info("agent binary swapped, stopping kernels and exiting for supervisor",
		"from", buildinfo.Version, "to", version, "backup", backup)

	// 换核成功后写 pending 标记：新进程启动时据此布防看门狗——若在窗口期内未达到
	// 健康信号（MarkHealthy），自动恢复备份二进制并退出，systemd 拉回旧版本。
	// 标记携带升级前健康的节点集：解除条件 = 心跳成功 + 这些节点全部恢复运行，
	// 覆盖"新二进制通过 -v 校验但节点服务全灭"的残余风险（2026-10-24 事故形态）。
	if err := writeUpgradeMarker(dir, backup, o.healthyNodeIDs()); err != nil {
		nlog.Core().Warn("upgrade marker write failed; new binary starts without watchdog", "error", err)
	}

	o.stopAll()
	if o.selfRestart != nil {
		o.selfRestart()
		return
	}
	os.Exit(0)
}

// ─── 启动看门狗（P0 回滚）────────────────────────────────────────────

const upgradeMarkerName = ".xboard-node.upgrade-pending"
const nodeHealthStateName = ".node-health.json"

// 窗口要覆盖：首跳心跳（push_interval，默认 60s）+ 至少一轮节点崩溃重试，
// 240s 内期望节点未全部恢复即回退。
const upgradeWatchdogTimeout = 240 * time.Second

// upgradeMarker 换核 pending 标记；Healthy 为升级前处于 running 的节点集，
// 新版本的看门狗解除条件 = 心跳成功 + 该集合全部恢复运行。
// install.sh 升级路径写空 Healthy（它不知道节点状态），由 agent 从
// nodeHealthStateName 的升级前快照继承。
type upgradeMarker struct {
	Backup  string `json:"backup"`
	Healthy []int  `json:"healthy,omitempty"`
}

// nodeHealthState 心跳成功后落盘的节点健康快照；跨二进制交换持久，
// 供升级后的新进程还原"升级前哪些节点在跑"。
type nodeHealthState struct {
	Healthy  []int64 `json:"healthy"`
	UnixTime int64   `json:"unix_time"`
}

func writeUpgradeMarker(dir, backupPath string, healthy []int) error {
	b, err := json.Marshal(upgradeMarker{Backup: backupPath, Healthy: healthy})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, upgradeMarkerName), b, 0o600)
}

// persistNodeHealth 原子落盘健康节点快照（写在二进制同目录，属可写的 INSTALL_ROOT）。
func persistNodeHealth(ids []int) {
	live, err := os.Executable()
	if err != nil {
		return
	}
	path := filepath.Join(filepath.Dir(live), nodeHealthStateName)
	st := nodeHealthState{UnixTime: time.Now().Unix()}
	for _, id := range ids {
		st.Healthy = append(st.Healthy, int64(id))
	}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// readNodeHealthSnapshot 读取升级前快照（新进程启动早期调用，此时文件仍是旧进程内容）。
func readNodeHealthSnapshot(dir string) []int {
	raw, err := os.ReadFile(filepath.Join(dir, nodeHealthStateName))
	if err != nil {
		return nil
	}
	var st nodeHealthState
	if json.Unmarshal(raw, &st) != nil {
		return nil
	}
	ids := make([]int, 0, len(st.Healthy))
	for _, id := range st.Healthy {
		ids = append(ids, int(id))
	}
	return ids
}

// ArmUpgradeWatchdog 在进程启动早期调用：若存在换核 pending 标记，布防看门狗。
// 返回的 disarm 函数在达到健康信号时调用（幂等）。
var (
	watchdogArmed  atomic.Bool
	watchdogDisarm func()
	watchdogInitMu sync.Mutex
	// watchdogExpected 记录标记里的健康节点集（未布防时为 nil）
	watchdogExpected []int
)

// watchdogExpectedHealthy 返回升级看门狗的期望健康节点集；空集 = 仅凭心跳解除。
func watchdogExpectedHealthy() []int {
	return watchdogExpected
}

// fallbackMarkerBackup 解析旧版纯路径标记（新 JSON 标记之前的格式）
func fallbackMarkerBackup(raw []byte) string {
	return strings.TrimSpace(string(raw))
}

func ArmUpgradeWatchdog() (disarm func()) {
	watchdogInitMu.Lock()
	defer watchdogInitMu.Unlock()
	// 幂等：同一进程重复调用返回同一解除函数（历史上 main 与 runWithReload
	// 各布防一次，泄漏的第二个看门狗在健康解除后开火，把成功升级误回滚）
	if watchdogArmed.Load() {
		return watchdogDisarm
	}
	watchdogExpected = nil
	live, err := os.Executable()
	if err != nil {
		watchdogDisarm = func() {}
		return watchdogDisarm
	}
	dir := filepath.Dir(live)
	markerPath := filepath.Join(dir, upgradeMarkerName)
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		watchdogDisarm = func() {}
		return watchdogDisarm // 无标记 = 常规重启，无需布防
	}
	var marker upgradeMarker
	if json.Unmarshal(raw, &marker) == nil && marker.Backup != "" {
		watchdogExpected = marker.Healthy
	} else {
		// 旧版标记仅含备份路径（跨版本升级在途）——退化为仅凭心跳解除
		marker.Backup = fallbackMarkerBackup(raw)
	}
	if marker.Backup == "" {
		watchdogDisarm = func() {}
		return watchdogDisarm
	}
	if len(watchdogExpected) == 0 {
		// install.sh 升级路径写的标记不带 Healthy：从升级前的健康快照继承期望集
		watchdogExpected = readNodeHealthSnapshot(dir)
	}
	watchdogArmed.Store(true)
	backupPath := marker.Backup
	nlog.Core().Warn("upgrade watchdog armed: restore backup if not healthy in "+upgradeWatchdogTimeout.String(),
		"backup", backupPath, "expect_healthy_nodes", len(watchdogExpected))

	done := make(chan struct{})
	var once sync.Once
	go func() {
		nlog.Go("upgrade.watchdog", func() {
			select {
			case <-done:
				return
			case <-time.After(upgradeWatchdogTimeout):
				// 火焰路径复核标记：标记已被移除 = 健康路径已解除（可能是另一实例），
				// 此时不做任何回滚/退出，避免幽灵看门狗破坏成功的升级
				if _, err := os.Stat(markerPath); os.IsNotExist(err) {
					nlog.Core().Warn("upgrade watchdog fire skipped: marker already cleared (healthy)")
					return
				}
				nlog.Core().Error("upgrade watchdog fired: new binary not healthy, rolling back", "backup", backupPath)
				if err := os.Rename(backupPath, live); err != nil {
					nlog.Core().Error("upgrade watchdog rollback rename failed — manual recovery required", "error", err)
				}
				os.Remove(markerPath)
				os.Exit(1) // systemd Restart=always 拉回旧版本
			}
		})
	}()
	watchdogDisarm = func() {
		once.Do(func() {
			close(done)
			os.Remove(markerPath)
			nlog.Core().Info("upgrade watchdog disarmed: agent healthy on new version")
		})
	}
	return watchdogDisarm
}

func shaForArch(amd64, arm64 string) string {
	switch runtime.GOARCH {
	case "arm64":
		return arm64
	default:
		return amd64
	}
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func download(url, dest string) error {
	// 慢链路（跨境/回环公网）下载 60MB+ 二进制可能超过 3 分钟；10 分钟上限兼顾慢速与最终失败
	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}
