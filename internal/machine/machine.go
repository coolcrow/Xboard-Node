// Package machine implements the machine-mode orchestrator that dynamically
// discovers nodes from the panel's machine API and manages their lifecycles.
package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/monitor"
	"github.com/cedar2025/xboard-node/internal/buildinfo"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/firewall"
	"github.com/cedar2025/xboard-node/internal/relay"
	"github.com/cedar2025/xboard-node/internal/service"
)

// nodeHandle tracks a running node service.
type nodeHandle struct {
	cancel  context.CancelFunc
	done    chan struct{}
	mailbox *controlplane.NodeMailbox
	service *service.Service
}

// nodeState 跟踪节点服务健康度：心跳上报（load_status.nodes）+ 崩溃重试退避共用。
type nodeState struct {
	Status      string // "running" | "starting" | "crashed"
	Err         string    // 最近一次崩溃原因（截断）
	Consecutive int       // 连续崩溃次数（运行超过复位阈值则重新计数）
	LastCrash   time.Time
	RanFor      time.Duration // 上次完整运行的时长
}

const (
	// nodeRunConfirmDelay：崩溃后的重试要稳定存活超过该时长才报 "running"，
	// 避免重试启动与再次崩溃之间的亚秒窗口被心跳抽样成 running → 面板误发恢复通知
	nodeRunConfirmDelay = 10 * time.Second
)

const (
	nodeRetryBaseDelay = 30 * time.Second
	nodeRetryMaxDelay  = 5 * time.Minute
	// 单次运行超过该时长后崩溃 → 视为"稳定过"，连续计数重新从 1 开始
	nodeRunResetThreshold = 2 * time.Minute
)

// nodeRetryDelay 返回第 n 次连续崩溃后重启该节点前需要等待的退避窗口。
// rediscover 每 pull 周期（默认 60s）会尝试补起缺失节点，本退避决定"是否放行"，
// 实际重试间隔 = max(pull 周期, 退避窗口)。
func nodeRetryDelay(consecutive int) time.Duration {
	switch {
	case consecutive <= 1:
		return nodeRetryBaseDelay
	case consecutive == 2:
		return 60 * time.Second
	case consecutive == 3:
		return 120 * time.Second
	case consecutive == 4:
		return 240 * time.Second
	default:
		return nodeRetryMaxDelay
	}
}

// Orchestrator manages all nodes bound to a panel machine. It:
//   - discovers nodes via GET /machine/nodes
//   - starts / stops Service instances as nodes are added / removed
//   - maintains a shared WS connection that demuxes events by node_id
//   - reports machine-level load via POST /machine/status
type Orchestrator struct {
	cfg    *config.Config
	client *panel.Client // machine-level client (no node_id)

	mu    sync.Mutex
	nodes map[int]*nodeHandle // node_id → handle（注意：崩溃路径必须清理，否则 startNode 视为已运行永不重启）
	// nodeStates 与 nodes 同一把锁；节点被主动移除时一并清理
	nodeStates map[int]*nodeState

	// Per-node mailbox keyed by node_id. Shared WS events are aggregated here
	// and each node service drains the latest state when ready.
	eventsMu  sync.RWMutex
	mailboxes map[int]*controlplane.NodeMailbox
	statuses  map[int]chan<- controlplane.StatusChange

	// Shared WS client (nil when WS is disabled).
	ws       *panel.WSClient
	wsCancel context.CancelFunc

	// runCtx is stored from Run() so that onWSEvent can trigger rediscover
	// for sync.nodes events without blocking the main loop.
	runCtx context.Context

	pullInterval time.Duration
	pushInterval time.Duration

	// selfRestart, when set, terminates the agent process so the service
	// manager (systemd Restart=always) re-launches it — used by
	// control.restart without node_id.
	selfRestart func()

	// upgradeStatus 携带 control.upgrade 的进度/结果，随机器心跳上报面板。
	upgradeStatus atomic.Value
	// upgradeMu 串行化 selfUpgrade（并发指令直接拒绝）。
	upgradeMu sync.Mutex
	// healthyDisarm 由 main 注入：升级看门狗的健康解除函数（首次成功上报心跳时调用）。
	healthyDisarm func()
}

// New creates a machine orchestrator from the given config.
func New(cfg *config.Config) *Orchestrator {
	panelCfg := config.PanelConfig{
		URL:       cfg.Panel.URL,
		Token:     cfg.Machine.Token,
		MachineID: cfg.Machine.MachineID,
	}
	return &Orchestrator{
		cfg:        cfg,
		client:     panel.NewClient(panelCfg),
		nodes:      make(map[int]*nodeHandle),
		nodeStates: make(map[int]*nodeState),
		mailboxes:  make(map[int]*controlplane.NodeMailbox),
		statuses:   make(map[int]chan<- controlplane.StatusChange),
	}
}

// Run is the main loop. It blocks until ctx is cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.runCtx = ctx
	nodesResp, err := o.client.GetMachineNodes()
	if err != nil {
		return fmt.Errorf("initial node discovery: %w", err)
	}

	o.applyIntervals(nodesResp.BaseConfig)
	nlog.Core().Info(fmt.Sprintf("machine %d: discovered %d nodes",
		o.cfg.Machine.MachineID, len(nodesResp.Nodes)))

	// relay 基线：断线期间面板可能已改配置，启动即对齐
	if nodesResp.Relay != nil {
		nlog.Go("machine.relayBaseline", func() { o.applyRelay(*nodesResp.Relay) })
	}

	// Start machine-level WS as early as possible so sync.nodes can reach an
	// empty machine before the first node is attached.
	o.tryStartWS(ctx)

	// Start initial nodes.
	for _, n := range nodesResp.Nodes {
		o.startNode(ctx, n)
	}

	discoveryTicker := time.NewTicker(o.pullInterval)
	statusTicker := time.NewTicker(o.pushInterval)
	defer discoveryTicker.Stop()
	defer statusTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			o.stopAll()
			return nil

		case <-discoveryTicker.C:
			o.rediscover(ctx)

		case <-statusTicker.C:
			o.reportMachineStatus()
		}
	}
}

// ─── Node lifecycle ──────────────────────────────────────────────────────

func (o *Orchestrator) startNode(ctx context.Context, mn panel.MachineNode) {
	o.mu.Lock()
	if _, exists := o.nodes[mn.ID]; exists {
		o.mu.Unlock()
		return
	}
	// 崩溃退避：距上次崩溃不足退避窗口则本轮跳过，等下一轮 rediscover 再试
	if st, ok := o.nodeStates[mn.ID]; ok && st.Status == "crashed" {
		wait := nodeRetryDelay(st.Consecutive)
		if since := time.Since(st.LastCrash); since < wait {
			o.mu.Unlock()
			nlog.Core().Debug("machine: node retry backoff, skip start",
				"node_id", mn.ID, "consecutive", st.Consecutive,
				"eligible_in", (wait - since).Round(time.Second))
			return
		}
	}

	nodeCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	mb := controlplane.NewNodeMailbox()
	o.nodes[mn.ID] = &nodeHandle{cancel: cancel, done: done, mailbox: mb}
	if prev := o.nodeStates[mn.ID]; prev == nil {
		o.nodeStates[mn.ID] = &nodeState{Status: "running"}
	} else if prev.Status == "crashed" {
		prev.Status = "starting"
	} else {
		prev.Status = "running"
	}
	o.mu.Unlock()

	o.eventsMu.Lock()
	o.mailboxes[mn.ID] = mb
	o.eventsMu.Unlock()

	nodeCfg := o.cfg.ExpandMachineNode(mn.ID, mn.Type)

	perNodeClient := o.client.ForNode(mn.ID)

	// Pre-fetch node config to detect transport-based kernel requirements.
	// If the transport (e.g. xhttp) is incompatible with the configured kernel
	// (e.g. singbox), auto-switch to the required kernel for this node.
	if cfgSnapshot, err := perNodeClient.GetConfig(); err == nil && cfgSnapshot != nil {
		if resolved := model.ResolveKernelForTransport(cfgSnapshot.Network, nodeCfg.Kernel.Type); resolved != nodeCfg.Kernel.Type {
			nlog.Core().Info(fmt.Sprintf("machine: auto-switching kernel for node %d (%s→%s, transport=%s)",
				mn.ID, nodeCfg.Kernel.Type, resolved, cfgSnapshot.Network))
			nodeCfg.Kernel.Type = resolved
		}
	}
	// Reset cached ETag so the subsequent GetConfig in Initial() gets a full response.
	perNodeClient.ResetConfigETag()

	var push controlplane.PushClient
	if o.ws != nil {
		push = &machineNodePush{
			nodeID: mn.ID,
			ws:     o.ws,
		}
	}

	// The registerFn is called by MachinePanelControlPlane.Initial() to expose
	// the node mailbox + status channel to the Service.
	nodeID := mn.ID
	registerFn := func(st chan<- controlplane.StatusChange) *controlplane.NodeMailbox {
		o.registerNode(nodeID, st)
		return mb
	}

	cp := controlplane.NewMachinePanelControlPlane(perNodeClient, nodeCfg.Kernel, push, registerFn)
	svc := service.NewWithControlPlane(nodeCfg, cp)

	o.mu.Lock()
	if h, ok := o.nodes[mn.ID]; ok {
		h.service = svc
	}
	o.mu.Unlock()

	nlog.Core().Info(fmt.Sprintf("machine: starting node %d (%s/%s)",
		mn.ID, mn.Type, mn.Name))

	nlog.Go("machine.nodeRun", func() {
		defer close(done)
		defer o.unregisterNode(mn.ID)
		started := time.Now()
		confirmRunning := time.AfterFunc(nodeRunConfirmDelay, func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			if st, ok := o.nodeStates[mn.ID]; ok && st.Status == "starting" {
				st.Status = "running"
			}
		})
		defer confirmRunning.Stop()
		err := svc.Run(nodeCtx)

		o.mu.Lock()
		// 崩溃路径清理僵尸句柄：不清则 startNode 视为"已运行"，rediscover 永远不会重启它
		// （2026-10-24 客户事故：节点服务退出后搁浅 11 天的直接根因）。
		// stopNode 已先行删除句柄，h.done == done 保证只删自己的。
		if h, ok := o.nodes[mn.ID]; ok && h.done == done {
			delete(o.nodes, mn.ID)
		}
		if err != nil && nodeCtx.Err() == nil {
			st := o.nodeStates[mn.ID]
			if st == nil {
				st = &nodeState{}
				o.nodeStates[mn.ID] = st
			}
			ranFor := time.Since(started)
			st.Status = "crashed"
			st.Err = err.Error()
			if len(st.Err) > 256 {
				st.Err = st.Err[:256]
			}
			st.LastCrash = time.Now()
			st.RanFor = ranFor
			if ranFor >= nodeRunResetThreshold {
				st.Consecutive = 1
			} else {
				st.Consecutive++
			}
			consecutive := st.Consecutive
			o.mu.Unlock()
			nlog.Core().Error("machine node exited with error",
				"node_id", mn.ID, "error", err,
				"consecutive_crashes", consecutive,
				"retry_eligible_in", nodeRetryDelay(consecutive).Round(time.Second))
			return
		}
		o.mu.Unlock()
		if err != nil {
			nlog.Core().Debug("machine node stopped", "node_id", mn.ID, "error", err)
		}
	})
}

func (o *Orchestrator) reloadNode(nodeID int) {
	o.mu.Lock()
	h, ok := o.nodes[nodeID]
	o.mu.Unlock()
	if !ok {
		nlog.Core().Warn("control.reload: unknown node", "node_id", nodeID)
		return
	}
	if h.service != nil {
		h.service.ForceReload()
	}
}

func (o *Orchestrator) restartNode(nodeID int) {
	o.mu.Lock()
	_, ok := o.nodes[nodeID]
	o.mu.Unlock()
	if !ok {
		nlog.Core().Warn("control.restart: unknown node", "node_id", nodeID)
		return
	}
	o.stopNode(nodeID)
	// rediscovery re-fetches the node list and starts everything missing,
	// which covers the stopped node.
	nlog.Go("machine.rediscover", func() { o.rediscover(o.runCtx) })
}

// SetSelfRestart wires the agent-level restart hook (called by main after
// orchestrator construction; nil = machine-level control.restart is a no-op).
// SetHealthyDisarm 注入看门狗解除函数（首次成功心跳上报时调用，幂等）。
// fn 为 nil 时（看门狗未布防——常规重启），仅用于登记「本次启动版本」到升级状态，
// 覆盖上一次升级失败时残留的 upgrade_status。
func (o *Orchestrator) SetHealthyDisarm(fn func()) {
	o.healthyDisarm = fn
	if fn != nil {
		o.upgradeStatus.Store("upgraded to " + buildinfo.Version)
	}
}

func (o *Orchestrator) SetSelfRestart(fn func()) {
	o.mu.Lock()
	o.selfRestart = fn
	o.mu.Unlock()
}

func (o *Orchestrator) stopNode(nodeID int) {
	o.mu.Lock()
	h, ok := o.nodes[nodeID]
	if !ok {
		o.mu.Unlock()
		return
	}
	delete(o.nodes, nodeID)
	// 主动移除（面板删节点 / control.restart）同时清健康状态：restart 语义 = 退避归零
	delete(o.nodeStates, nodeID)
	o.mu.Unlock()

	o.eventsMu.Lock()
	delete(o.mailboxes, nodeID)
	o.eventsMu.Unlock()

	nlog.Core().Info(fmt.Sprintf("machine: stopping node %d", nodeID))
	h.cancel()
	<-h.done
}

func (o *Orchestrator) stopAll() {
	o.mu.Lock()
	handles := make(map[int]*nodeHandle, len(o.nodes))
	for id, h := range o.nodes {
		handles[id] = h
	}
	o.mu.Unlock()

	for id, h := range handles {
		nlog.Core().Info(fmt.Sprintf("machine: stopping node %d", id))
		h.cancel()
	}
	for _, h := range handles {
		<-h.done
	}

	if o.wsCancel != nil {
		o.wsCancel()
	}
}

// ─── Node discovery ──────────────────────────────────────────────────────

func (o *Orchestrator) rediscover(ctx context.Context) {
	nodesResp, err := o.client.GetMachineNodes()
	if err != nil {
		nlog.Core().Warn("machine node discovery failed", "error", err)
		return
	}

	if nodesResp.Relay != nil {
		o.applyRelay(*nodesResp.Relay) // 幂等：配置未变时 no-op
	}
	if nodesResp.Firewall != nil && nodesResp.Firewall.Managed {
		if err := firewall.Apply(*nodesResp.Firewall); err != nil {
			nlog.Core().Error("firewall: reconcile failed", "error", err)
		}
	}

	wanted := make(map[int]panel.MachineNode, len(nodesResp.Nodes))
	for _, n := range nodesResp.Nodes {
		wanted[n.ID] = n
	}

	o.mu.Lock()
	var toRemove []int
	for id := range o.nodes {
		if _, ok := wanted[id]; !ok {
			toRemove = append(toRemove, id)
		}
	}
	o.mu.Unlock()

	for _, id := range toRemove {
		o.stopNode(id)
	}

	for _, n := range nodesResp.Nodes {
		o.startNode(ctx, n) // no-op if already running
	}
}

// ─── Relay (realm) management ────────────────────────────────────────────

// applyRelay 将面板 relay spec 对齐到本机 realm（幂等；错误只记录不影响节点服务）。
func (o *Orchestrator) applyRelay(spec relay.Spec) {
	if !spec.Enabled {
		// 未启用也可能是"从未配置"——Collect 会如实上报，无需日志刷屏
		nlog.Core().Debug("relay: spec disabled, ensuring teardown")
	} else {
		nlog.Core().Info("relay: applying spec", "landing", spec.LandingHost, "ports", spec.Ports)
	}
	if err := relay.Apply(spec); err != nil {
		nlog.Core().Error("relay: apply failed", "error", err)
	}
}

// ─── Machine status reporting ────────────────────────────────────────────

func (o *Orchestrator) reportMachineStatus() {
	s := monitor.Collect()
	upgradeStatus, _ := o.upgradeStatus.Load().(string)
	nodes := o.nodeStatusSnapshot()
	if err := o.client.ReportMachineStatus(
		s.CPU,
		[2]uint64{s.MemTotal, s.MemUsed},
		[2]uint64{s.SwapTotal, s.SwapUsed},
		[2]uint64{s.DiskTotal, s.DiskUsed},
		s.NetInSpeed, s.NetOutSpeed,
		upgradeStatus,
		relay.Collect(),
		nodes,
	); err != nil {
		nlog.Core().Warn("machine status report failed", "error", err)
		return
	}
	persistNodeHealth(o.healthyNodeIDs())
	// 看门狗解除门槛：上报成功 + 升级前健康的节点全部恢复运行。
	// 只看心跳不看节点的旧判定让 2026-10-24 事故以"升级成功"收场（进程活着、内核全死）。
	if healthyDisarm := o.healthyDisarm; healthyDisarm != nil && o.allExpectedNodesHealthy() {
		healthyDisarm()
	}
}

// nodeStatusSnapshot 生成心跳里的节点级状态（load_status.nodes），面板据此告警。
func (o *Orchestrator) nodeStatusSnapshot() map[string]interface{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.nodeStates) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(o.nodeStates))
	for id, st := range o.nodeStates {
		e := map[string]interface{}{"status": st.Status}
		if st.Status == "crashed" || st.Status == "starting" {
			// starting 保留最近一次崩溃上下文：面板据此区分"重试中"与"健康"
			if st.Err != "" {
				e["error"] = st.Err
			}
			e["consecutive"] = st.Consecutive
			if st.Status == "crashed" {
				remaining := nodeRetryDelay(st.Consecutive) - time.Since(st.LastCrash)
				if remaining < 0 {
					remaining = 0
				}
				e["retry_in"] = int(remaining.Seconds())
			}
		}
		out[strconv.Itoa(id)] = e
	}
	return out
}

// healthyNodeIDs 返回当前处于 running 状态的节点（升级换核前调用，写入看门狗标记）。
func (o *Orchestrator) healthyNodeIDs() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	ids := make([]int, 0, len(o.nodeStates))
	for id, st := range o.nodeStates {
		if st.Status == "running" {
			ids = append(ids, id)
		}
	}
	return ids
}

// allExpectedNodesHealthy：看门狗期望集（升级标记里的健康节点）全部 running 才放行解除。
// 期望集为空（常规重启 / 升级前本就无健康节点）→ 仅凭心跳解除。
func (o *Orchestrator) allExpectedNodesHealthy() bool {
	expected := watchdogExpectedHealthy()
	if len(expected) == 0 {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, id := range expected {
		st, ok := o.nodeStates[id]
		if !ok || st.Status != "running" {
			return false
		}
	}
	return true
}

// ─── WS mux ─────────────────────────────────────────────────────────────

func (o *Orchestrator) tryStartWS(ctx context.Context) {
	hs, err := o.client.Handshake()
	if err != nil {
		nlog.Core().Warn("machine ws handshake failed, REST only", "error", err)
		return
	}
	if !hs.WebSocket.Enabled || hs.WebSocket.WSURL == "" {
		nlog.Core().Info("machine: ws disabled by panel, REST only")
		return
	}

	wsCfg := panel.WSClientConfig{
		StatusInterval:   time.Duration(o.cfg.WS.StatusInterval) * time.Second,
		HandshakeTimeout: time.Duration(o.cfg.WS.HandshakeTimeout) * time.Second,
		BackoffInitial:   time.Duration(o.cfg.WS.BackoffInitial) * time.Second,
		BackoffMax:       time.Duration(o.cfg.WS.BackoffMax) * time.Second,
		MachineID:        o.cfg.Machine.MachineID,
	}

	o.ws = panel.NewWSClient(
		hs.WebSocket.WSURL,
		o.cfg.Machine.Token,
		0, // no single node_id
		wsCfg,
		o.onWSEvent,
		o.onWSStatus,
		nil, // per-node status is sent via machineNodePush
	)

	wsCtx, wsCancel := context.WithCancel(ctx)
	o.wsCancel = wsCancel
	nlog.Go("machine.wsRun", func() { o.ws.Run(wsCtx) })

	nlog.Core().Info("machine: ws mux started")
}

// onWSEvent routes a WS event to the correct node's channel.
// sync.nodes is a machine-level event that triggers immediate rediscovery.
func (o *Orchestrator) onWSEvent(event panel.WSEvent) {
	// sync.nodes is a machine-level event, not per-node
	if event.Type == panel.WSEventSyncNodes {
		nlog.Core().Info("machine received sync.nodes, triggering immediate rediscovery")
		nlog.Go("machine.rediscover", func() { o.rediscover(o.runCtx) })
		return
	}

	// sync.relay: realm 转发配置变更（即时应用）
	if event.Type == panel.WSEventSyncRelay && event.Relay != nil {
		nlog.Core().Info("machine received sync.relay", "enabled", event.Relay.Enabled,
			"landing", event.Relay.LandingHost, "ports", event.Relay.Ports)
		nlog.Go("machine.relayApply", func() { o.applyRelay(*event.Relay) })
		return
	}

	// control.* events are machine-level directives from the panel
	if event.Type == panel.WSEventControlReload {
		if event.NodeID > 0 {
			nlog.Core().Info("machine received control.reload", "node_id", event.NodeID)
			nlog.Go("machine.reloadNode", func() { o.reloadNode(event.NodeID) })
		} else {
			nlog.Core().Info("machine received control.reload (all nodes)")
			nlog.Go("machine.rediscover", func() { o.rediscover(o.runCtx) })
		}
		return
	}
	if event.Type == panel.WSEventControlRestart {
		if event.NodeID > 0 {
			nlog.Core().Info("machine received control.restart", "node_id", event.NodeID)
			nlog.Go("machine.restartNode", func() { o.restartNode(event.NodeID) })
		} else if o.selfRestart != nil {
			nlog.Core().Info("machine received control.restart (agent), exiting for supervisor restart")
			nlog.Go("machine.selfRestart", func() {
				o.stopAll()
				o.selfRestart()
			})
		}
		return
	}
	if event.Type == panel.WSEventControlUpgrade {
		nlog.Core().Info("machine received control.upgrade", "version", event.Version)
		nlog.Go("machine.selfUpgrade", func() {
			o.selfUpgrade(event.Version, event.SHA256AMD64, event.SHA256ARM64)
		})
		return
	}

	nodeID := event.NodeID
	if nodeID == 0 {
		nlog.Core().Debug("machine ws event missing node_id, dropping", "type", event.Type)
		return
	}

	translated, err := controlplane.TranslateWSEvent(event, o.cfg.Kernel)
	if err != nil {
		nlog.Core().Warn("machine ws event translation failed",
			"type", event.Type, "node_id", nodeID, "error", err)
		return
	}

	o.eventsMu.RLock()
	mailbox, ok := o.mailboxes[nodeID]
	o.eventsMu.RUnlock()
	if !ok {
		nlog.Core().Debug("machine ws event for unknown node", "node_id", nodeID, "type", event.Type)
		return
	}
	mailbox.Apply(translated)
}

// onWSStatus broadcasts WS connectivity changes to all registered nodes.
func (o *Orchestrator) onWSStatus(status panel.WSStatusChange) {
	change := controlplane.StatusChange{Connected: status.Connected}
	o.eventsMu.RLock()
	defer o.eventsMu.RUnlock()
	for _, ch := range o.statuses {
		select {
		case ch <- change:
		default:
		}
	}
}

func (o *Orchestrator) registerNode(nodeID int, st chan<- controlplane.StatusChange) {
	o.eventsMu.Lock()
	o.statuses[nodeID] = st
	o.eventsMu.Unlock()
}

func (o *Orchestrator) unregisterNode(nodeID int) {
	o.eventsMu.Lock()
	delete(o.mailboxes, nodeID)
	delete(o.statuses, nodeID)
	o.eventsMu.Unlock()
}

func (o *Orchestrator) applyIntervals(bc panel.MachineBaseConfig) {
	o.pullInterval = time.Duration(bc.PullInterval) * time.Second
	if o.pullInterval < 30*time.Second {
		o.pullInterval = 60 * time.Second
	}
	o.pushInterval = time.Duration(bc.PushInterval) * time.Second
	if o.pushInterval < 10*time.Second {
		o.pushInterval = 60 * time.Second
	}
	// 心跳必须能在升级看门狗窗口（240s）内稳定打满两拍，
	// 否则健康升级会在首拍到来前被看门狗误回退
	if o.pushInterval > 120*time.Second {
		o.pushInterval = 120 * time.Second
	}
}

// ─── Virtual PushClient ─────────────────────────────────────────────────

// machineNodePush implements controlplane.PushClient for a single node
// backed by the shared machine WS connection. Events are routed by the
// WS mux directly to the Service's channels; this adapter only provides
// connectivity status and send capabilities.
type machineNodePush struct {
	nodeID int
	ws     *panel.WSClient
}

func (p *machineNodePush) Run(ctx context.Context) {
	// The shared WS mux pushes events into our channels; we just wait.
	<-ctx.Done()
}

func (p *machineNodePush) IsConnected() bool {
	return p.ws != nil && p.ws.IsConnected()
}

func (p *machineNodePush) SendDeviceReport(devices map[int][]string) {
	if p.ws == nil {
		return
	}
	payload := map[string]interface{}{
		"node_id": p.nodeID,
	}
	// Flatten into the standard format with node_id wrapper.
	strDevices := make(map[string][]string, len(devices))
	for uid, ips := range devices {
		strDevices[fmt.Sprintf("%d", uid)] = ips
	}
	payload["devices"] = strDevices
	data, _ := json.Marshal(payload)
	p.ws.SendRaw(panel.WSEventReportDevices, data)
}
