# xboard-node

Node backend for [Xboard](https://github.com/cedar2025/Xboard). Supports `sing-box` / `xray-core` dual kernels.

> **Disclaimer**: This project is for educational and learning purposes only.

## Features

- Protocols: V2Ray family, Trojan, Shadowsocks, Hysteria2, TUIC, AnyTLS
- Sync: WebSocket push + REST polling dual channel
- User controls: speed limit, device limit, alive-IP tracking, hot update
- Deploy modes: node mode, machine mode, standalone mode
- Multi-instance: single process binding multiple panels / nodes

## Install

### Docker

```bash
docker run -d --restart=always --network=host \
  -e apiHost=https://panel.com -e apiKey=TOKEN -e nodeID=1 \
  ghcr.io/cedar2025/xboard-node:latest
```

### Docker Compose

```bash
git clone -b compose --depth 1 https://github.com/cedar2025/xboard-node.git
cd xboard-node
vim config/config.yml   # set panel.url / token / node_id
docker compose up -d
```

### Installer (Linux systemd)

```bash
# Node mode
curl -fsSL https://raw.githubusercontent.com/coolcrow/Xboard-Node/main/install.sh | \
  sudo bash -s -- --mode node --panel https://panel.example.com --token TOKEN --node-id 1

# Machine mode
curl -fsSL https://raw.githubusercontent.com/coolcrow/Xboard-Node/main/install.sh | \
  sudo bash -s -- --mode machine --panel https://panel.example.com --token TOKEN --machine-id 1

# 中国大陆节点（agent-dist 镜像加速 + 指定版本）
curl -fsSL https://raw.githubusercontent.com/coolcrow/Xboard-Node/main/install.sh | \
  sudo bash -s -- --mode machine --panel https://panel.example.com --token TOKEN --machine-id 1 \
       --mirror https://panel.example.com/agent-dist --version v1.0.6

## Relay / Landing Setup (Optional)

For relay-protected landing architecture (users connect to a relay entry, landing IPs stay hidden from GFW):

```bash
# On relay server (e.g., HK with China-optimized routing)
./tools-relay/relay-setup.sh --landing <landing_ip> --ports 443,18443

# Single-protocol (when TCP 443 is occupied by nginx on the relay box)
./tools-relay/relay-setup.sh --landing <landing_ip> --ports 443/udp,18443/tcp

# Switch landing (10 seconds, users unaffected)
./tools-relay/switch-landing.sh <new_landing_ip>
```

See [tools-relay/README.md](./tools-relay/README.md) for architecture, bandwidth planning, and troubleshooting.

## xbctl

Run `xbctl` after installation for help. Common commands:

```bash
xbctl list                          # list all instances
xbctl status                        # running status
xbctl bind add-node --panel URL --token TOKEN --node-id 1
xbctl bind add-machine --panel URL --token TOKEN --machine-id 1
xbctl bind remove-node --panel URL --node-id 1
xbctl service restart
```

## Configuration

Legacy single-panel config is fully compatible. Appending bindings auto-migrates to `instances` format. See `config.yml.example`.

## Extensions

- Custom routes: [docs-custom-routes.md](docs-custom-routes.md)
- Custom outbounds: [docs-custom-outbounds.md](docs-custom-outbounds.md)
- DNS providers (ACME DNS-01): [docs-dns-providers.md](docs-dns-providers.md)

## License

- 本仓库源码：MPL-2.0（延续上游 [cedar2025/Xboard-Node](https://github.com/cedar2025/Xboard-Node) 的许可声明）
- 发布二进制：静态链接 [sing-box](https://github.com/SagerNet/sing-box)（GPL-3.0，本组织镜像 [coolcrow/sing-box](https://github.com/coolcrow/sing-box)），故二进制分发适用 GPL-3.0 条款；对应源码均在本仓库与上述镜像公开，满足源码可得性
