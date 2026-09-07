package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// 覆盖“新二进制通过 -v 校验但在真实启动路径上崩溃”的残余风险。
	if err := writeUpgradeMarker(dir, backup); err != nil {
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
const upgradeWatchdogTimeout = 120 * time.Second

func writeUpgradeMarker(dir, backupPath string) error {
	return os.WriteFile(filepath.Join(dir, upgradeMarkerName), []byte(backupPath), 0o600)
}

// ArmUpgradeWatchdog 在进程启动早期调用：若存在换核 pending 标记，布防看门狗。
// 返回的 disarm 函数在达到健康信号时调用（幂等）。
var (
	watchdogArmed   atomic.Bool
	watchdogDisarm  func()
	watchdogInitMu  sync.Mutex
)

func ArmUpgradeWatchdog() (disarm func()) {
	watchdogInitMu.Lock()
	defer watchdogInitMu.Unlock()
	// 幂等：同一进程重复调用返回同一解除函数（历史上 main 与 runWithReload
	// 各布防一次，泄漏的第二个看门狗在健康解除后开火，把成功升级误回滚）
	if watchdogArmed.Load() {
		return watchdogDisarm
	}
	live, err := os.Executable()
	if err != nil {
		watchdogDisarm = func() {}
		return watchdogDisarm
	}
	dir := filepath.Dir(live)
	markerPath := filepath.Join(dir, upgradeMarkerName)
	backup, err := os.ReadFile(markerPath)
	if err != nil {
		watchdogDisarm = func() {}
		return watchdogDisarm // 无标记 = 常规重启，无需布防
	}
	watchdogArmed.Store(true)
	backupPath := strings.TrimSpace(string(backup))
	nlog.Core().Warn("upgrade watchdog armed: restore backup if not healthy in "+upgradeWatchdogTimeout.String(), "backup", backupPath)

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
