# AIBolt 中转层工具（Phase 2）

落地保护架构：用户 → 中转入口（本套脚本，三网优化机）→ 落地（廉价大流量机）。
落地 IP 不暴露给 GFW，被墙率趋近零；换落地对用户无感知。

```
用户 ──▶ hk1.aibolt.tech（中转机，realm L4 转发）
            ├─ 443/tcp+udp ──▶ 落地A 443（hysteria2）
            └─ 18443/tcp    ──▶ 落地A 18443（trojan）
```

## 首次部署（中转机，root）

```bash
scp tools/relay/*.sh root@<中转机>:/root/
ssh root@<中转机> './relay-setup.sh --landing <落地IP> --ports 443,18443'
```

- realm v2.9.6 musl，sha256 固定校验（2026-10-03 本地实测 TCP+UDP 转发通过）
- 每端口自动双协议（hy2 需要 UDP；trojan 的 tcp-only 端口多转 udp 无害）
- systemd 沙箱单元（ProtectSystem=strict + 仅 CAP_NET_BIND_SERVICE）

## 换落地（10 秒，用户无感）

```bash
./switch-landing.sh <新落地IP>     # 先探可达 → 改写 → 重启 → 自动备份可回滚
```

## 前置检查清单

1. **中转机**：三网优化（港 BGP/泛联类或云轻量），带宽 ≥ 预估峰值
   （50 用户晚高峰并发 ~15 × 8Mbps ≈ 120Mbps，轻量 30Mbps 不够）
2. **落地机**：放行中转 IP 的对应端口 TCP+UDP（agent 防火墙/云安全组都要开）
3. **DNS**：入口域名 A 记录 → 中转 IP，TTL=300
4. **面板**：节点 server 字段填入口域名（勿填落地 IP——那会绕过中转）

## 带宽/经济模型

| 层 | 规格 | 成本量级 |
|---|---|---|
| 中转 | 港三网优化 100-200Mbps | ¥150-400/月（泛联类按带宽） |
| 落地 | 廉价大流量 1-4TB/月 | $14-60/年（RackNerd/CloudCone 档） |

对比直连 GIA 扛流量：GIA $0.14/GB 级 vs 中转落地 <$0.01/GB 级——
用户量 >50 后中转架构成本优势一个数量级。

## 排障

- `journalctl -u 'realm-relay@*' -f`：转发日志
- 用户不通先分层：中转端口监听（ss）→ 中转→落地探测（switch 脚本里的 /dev/tcp 探测）→ 落地 agent 心跳
- 回滚：`cp /etc/realm/backup-*/\*.toml /etc/realm/ && systemctl restart realm-relay@{dual,tcp,udp}`
