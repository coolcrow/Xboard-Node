# Third-Party Notices

本仓库源码以 MPL-2.0 授权（见 [LICENSE](LICENSE)）。发布二进制静态链接以下第三方组件，
随每个 Release 分发时适用各组件许可证；GPL-3.0 全文见 [LICENSE-GPLv3.txt](LICENSE-GPLv3.txt)。

| 组件 | 许可证 | 用途 | 源码获取 |
|---|---|---|---|
| github.com/coolcrow/sing-box（sagernet/sing-box fork） | GPL-3.0 | sing-box 内核（稳定用户 ID 补丁） | https://github.com/coolcrow/sing-box（镜像公开） |
| github.com/cedar2025/Xray-core（XTLS/Xray-core fork） | MIT | Xray 内核 | https://github.com/cedar2025/Xray-core |
| github.com/caddyserver/certmagic | Apache-2.0 | ACME 证书管理 |
| github.com/libdns/*（21 providers） | Apache-2.0/MIT（各异） | DNS-01 挑战服务商适配 |
| github.com/gorilla/websocket | BSD-2-Clause | 面板 WS 控制通道 |
| github.com/fsnotify/fsnotify | BSD-3-Clause | 配置热重载监听 |
| gopkg.in/yaml.v3 | Apache-2.0 | 配置解析 |
| github.com/shirou/gopsutil | MIT | 机器负载采集 |
| 其余传递依赖 | 见 go.mod/go.sum 各自仓库 | — |

完整依赖树以 `go.mod` / `go.sum` 为准；各组件版权声明随其源码仓库保留。
