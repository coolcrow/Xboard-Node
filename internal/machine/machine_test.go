package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Panel:   config.PanelConfig{URL: "http://127.0.0.1:1", Token: "t"},
		Machine: &config.MachineConfig{MachineID: 1, Token: "t"},
	}
}

func TestNodeRetryDelaySequence(t *testing.T) {
	cases := []struct {
		consecutive int
		want        time.Duration
	}{
		{0, nodeRetryBaseDelay},
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, nodeRetryMaxDelay},
		{99, nodeRetryMaxDelay},
	}
	for _, c := range cases {
		if got := nodeRetryDelay(c.consecutive); got != c.want {
			t.Errorf("nodeRetryDelay(%d) = %v, want %v", c.consecutive, got, c.want)
		}
	}
}

func TestUpgradeMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := writeUpgradeMarker(dir, "/backup/path", []int{2, 3}); err != nil {
		t.Fatalf("writeUpgradeMarker: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, upgradeMarkerName))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m upgradeMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, raw)
	}
	if m.Backup != "/backup/path" {
		t.Errorf("Backup = %q", m.Backup)
	}
	if len(m.Healthy) != 2 || m.Healthy[0] != 2 || m.Healthy[1] != 3 {
		t.Errorf("Healthy = %v", m.Healthy)
	}
}

// 旧版标记（纯路径文本）跨版本升级时退化为仅凭心跳解除
func TestUpgradeMarkerLegacyPlainPath(t *testing.T) {
	var m upgradeMarker
	if err := json.Unmarshal([]byte("/plain/backup/path"), &m); err == nil && m.Backup != "" {
		t.Fatalf("legacy plain path should not parse as JSON marker")
	}
	// ArmUpgradeWatchdog 的回退分支：非 JSON → 视为纯路径
	legacy := "/plain/backup/path"
	parsed := fallbackMarkerBackup([]byte(legacy))
	if parsed != legacy {
		t.Errorf("fallbackMarkerBackup = %q, want %q", parsed, legacy)
	}
}

func TestAllExpectedNodesHealthy(t *testing.T) {
	o := New(testConfig())
	o.nodeStates[2] = &nodeState{Status: "running"}
	o.nodeStates[3] = &nodeState{Status: "crashed", Err: "boom"}

	watchdogExpected = []int{2}
	if !o.allExpectedNodesHealthy() {
		t.Error("expected healthy set {2} with node2 running should pass")
	}

	watchdogExpected = []int{2, 3}
	if o.allExpectedNodesHealthy() {
		t.Error("node3 crashed must block disarm")
	}

	watchdogExpected = nil
	if !o.allExpectedNodesHealthy() {
		t.Error("empty expected set (machine without nodes) should pass")
	}

	watchdogExpected = []int{99} // 标记里的节点未在本机出现（配置漂移）
	if o.allExpectedNodesHealthy() {
		t.Error("missing node in states must block disarm")
	}
	watchdogExpected = nil
}

func TestNodeStatusSnapshotShape(t *testing.T) {
	o := New(testConfig())
	crashAt := time.Now().Add(-10 * time.Second)
	o.nodeStates[2] = &nodeState{Status: "running"}
	o.nodeStates[3] = &nodeState{Status: "crashed", Err: "initial setup failed", Consecutive: 2, LastCrash: crashAt}

	snap := o.nodeStatusSnapshot()
	if snap == nil {
		t.Fatal("snapshot nil")
	}
	running, ok := snap["2"].(map[string]interface{})
	if !ok || running["status"] != "running" {
		t.Errorf("node2 snapshot = %v", snap["2"])
	}
	crashed, ok := snap["3"].(map[string]interface{})
	if !ok || crashed["status"] != "crashed" {
		t.Fatalf("node3 snapshot = %v", snap["3"])
	}
	if crashed["error"] != "initial setup failed" || crashed["consecutive"] != 2 {
		t.Errorf("node3 detail = %v", crashed)
	}
	if _, ok := crashed["retry_in"].(int); !ok {
		t.Errorf("retry_in missing: %v", crashed)
	}
	// 退避 60s（连续 2 次），已过 10s → 剩 ~50s
	if ri := crashed["retry_in"].(int); ri < 45 || ri > 55 {
		t.Errorf("retry_in = %d, want ~50", ri)
	}
}

func TestNodeStatusSnapshotEmpty(t *testing.T) {
	o := New(testConfig())
	if snap := o.nodeStatusSnapshot(); snap != nil {
		t.Errorf("empty states should yield nil, got %v", snap)
	}
}
