#!/usr/bin/env bash
# relay-setup.sh — AIBolt 中转层部署（realm，按协议分实例）
#
# 用法（中转机 root）：
#   ./relay-setup.sh --landing 198.44.54.109 --ports 443,18443          # 全双协议（生产常规）
#   ./relay-setup.sh --landing 198.44.54.109 --ports 443/udp,18443/tcp  # 单协议（端口冲突场景，
#                                                                       # 如 CVM 模拟：TCP443 被 nginx 占）
#   --bin /path/to/realm  可选，用本地二进制跳过下载
#
# 原理：realm 的 [network] 协议开关是全局的 → 按协议桶生成独立配置
#   /etc/realm/dual.toml（TCP+UDP）/ tcp.toml / udp.toml + systemd 模板单元
#   realm-relay@{dual,tcp,udp}。多实例共存，换落地用 switch-landing.sh。
#
# realm v2.9.6 musl（sha256 固定）；TCP+UDP 转发 2026-10-03 本地实测通过。
set -euo pipefail

REALM_VER="2.9.6"
REALM_SHA256="6a1733bfc51e3d19743757ca08f4c81f36afe7972eab0607bef264fc99064208"
REALM_URL="https://github.com/zhboner/realm/releases/download/v${REALM_VER}/realm-x86_64-unknown-linux-musl.tar.gz"
ETC_DIR="/etc/realm"
BIN_PATH="/usr/local/bin/realm"

LANDING=""; PORTS=""; LOCAL_BIN=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --landing) LANDING="$2"; shift 2 ;;
    --ports)   PORTS="$2"; shift 2 ;;
    --bin)     LOCAL_BIN="$2"; shift 2 ;;
    *) echo "unknown arg: $1"; exit 1 ;;
  esac
done
[ -n "$LANDING" ] || { echo "ERROR: --landing <落地IP> 必填"; exit 1; }
[ -n "$PORTS" ] || { echo "ERROR: --ports 必填（如 443,18443 或 443/udp,18443/tcp）"; exit 1; }

echo "=== [1/4] 安装 realm v${REALM_VER} ==="
if [ -n "$LOCAL_BIN" ]; then
  install -m 755 "$LOCAL_BIN" "$BIN_PATH"
else
  TMP=$(mktemp -d)
  curl -fsSL "$REALM_URL" -o "$TMP/realm.tgz"
  tar xzf "$TMP/realm.tgz" -C "$TMP"
  ACTUAL=$(sha256sum "$TMP/realm" | awk '{print $1}')
  [ "$ACTUAL" = "$REALM_SHA256" ] || { echo "ERROR: sha256 mismatch: $ACTUAL"; exit 1; }
  install -m 755 "$TMP/realm" "$BIN_PATH"; rm -rf "$TMP"
fi
"$BIN_PATH" --version | head -1

echo "=== [2/4] 端口分桶 → ${ETC_DIR}/*.toml ==="
mkdir -p "$ETC_DIR"; chmod 700 "$ETC_DIR"
DUAL=""; TCPONLY=""; UDPONLY=""
IFS=',' read -ra PARR <<< "$PORTS"
for p in "${PARR[@]}"; do
  PORT="${p%%/*}"; PROTO="${p##*/}"
  [[ "$PORT" =~ ^[0-9]+$ ]] || { echo "ERROR: 非法端口 $p"; exit 1; }
  case "$PROTO" in
    tcp)      TCPONLY+="${TCPONLY:+ }$PORT" ;;
    udp)      UDPONLY+="${UDPONLY:+ }$PORT" ;;
    "$PORT")  DUAL+="${DUAL:+ }$PORT" ;;
    *)        echo "ERROR: 非法协议 $p（支持 /tcp /udp 或省略=双协议）"; exit 1 ;;
  esac
done

gen_conf() { # $1=文件名 $2=no_tcp $3=use_udp $4=端口列表
  local endpoints=""
  local port
  for port in $4; do
    endpoints+="${endpoints:+$'\n'}[[endpoints]]
listen = \"0.0.0.0:${port}\"
remote = \"${LANDING}:${port}\""
  done
  cat > "${ETC_DIR}/$1" <<EOF
[network]
no_tcp = $2
use_udp = $3

# relay-setup.sh 生成（--landing ${LANDING}）；换落地用 switch-landing.sh
${endpoints}
EOF
  chmod 600 "${ETC_DIR}/$1"
  echo "--- ${ETC_DIR}/$1 ---"; cat "${ETC_DIR}/$1"
}

ACTIVE=""
[ -n "$DUAL" ]    && { gen_conf dual.toml false true  "$DUAL";    ACTIVE+=" dual"; }
[ -n "$TCPONLY" ] && { gen_conf tcp.toml false false "$TCPONLY"; ACTIVE+=" tcp"; }
[ -n "$UDPONLY" ] && { gen_conf udp.toml true  true  "$UDPONLY"; ACTIVE+=" udp"; }
[ -z "$ACTIVE" ] && { echo "ERROR: 无有效端口"; exit 1; }

# 端口重复检测（跨协议桶：同一端口出现在两个桶→绑定冲突）
ALL_PORTS=$(echo $DUAL $TCPONLY $UDPONLY | tr ' ' '\n' | sort)
DUPES=$(echo "$ALL_PORTS" | uniq -d)
[ -n "$DUPES" ] && { echo "ERROR: 端口重复分配: ${DUPES}（每个端口只能属于一个协议桶）"; exit 1; }

echo "=== [3/4] systemd 模板单元 + 启动 ==="
cat > /etc/systemd/system/realm-relay@.service <<EOF
[Unit]
Description=AIBolt relay (realm %i)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN_PATH} -c ${ETC_DIR}/%i.toml
Restart=always
RestartSec=3
LimitNOFILE=1048576
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=${ETC_DIR}
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
RestrictSUIDSGID=true
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
for i in $ACTIVE; do systemctl enable --now "realm-relay@${i}"; done

echo "=== [4/4] 验证 ==="
sleep 1
FAIL=0
for port in $DUAL $TCPONLY; do
  ss -tln | grep -q ":${port} " && echo "  TCP ${port}: listening ✓" || { echo "  TCP ${port}: ✗"; FAIL=1; }
done
for port in $DUAL $UDPONLY; do
  ss -uln | grep -q ":${port} " && echo "  UDP ${port}: listening ✓" || { echo "  UDP ${port}: ✗"; FAIL=1; }
done
[ $FAIL -eq 0 ] || { journalctl -u 'realm-relay@*' -n 15 --no-pager; exit 1; }
echo ""
echo "完成：入口端口 → 落地 ${LANDING}，实例:${ACTIVE}"
echo "落地侧需放行中转 IP 的对应端口（TCP+UDP）。换落地：./switch-landing.sh <新IP>"
