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

	o.stopAll()
	if o.selfRestart != nil {
		o.selfRestart()
		return
	}
	os.Exit(0)
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
	client := &http.Client{Timeout: 180 * time.Second}
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
