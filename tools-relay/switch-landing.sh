#!/usr/bin/env bash
# switch-landing.sh — 接入机一键换落地（用户无感知）
#
# 用法（接入机）：./switch-landing.sh 198.44.54.109
#
# 行为：改写 /etc/realm/*.toml 全部 remote → 探测新落地可达 → 重启全部 realm 实例
#       → 失败自动回滚。适用于 dual/tcp/udp 多实例布局。
set -euo pipefail

NEW="${1:?用法: $0 <新落地IP>}"
ETC_DIR="/etc/xboard-node/realm"  # 与 agent/relay-setup.sh 同路径

ls "${ETC_DIR}"/*.toml >/dev/null 2>&1 || { echo "ERROR: 无 realm 配置，先跑 relay-setup.sh"; exit 1; }

OLD=$(grep -hoP 'remote = "\K[0-9.]+' "${ETC_DIR}"/*.toml | head -1)
echo "换落地: ${OLD} → ${NEW}"

echo "=== [1/3] 探测新落地（先测后切） ==="
PORTS=$(grep -hoP 'remote = "[0-9.]+:\K[0-9]+' "${ETC_DIR}"/*.toml | sort -u)
for p in $PORTS; do
  timeout 4 bash -c "echo > /dev/tcp/${NEW}/${p}" 2>/dev/null && echo "  ${NEW}:${p}/tcp ✓" \
    || echo "  ${NEW}:${p}/tcp ✗（落地未放行？继续切换但请检查）"
done

echo "=== [2/3] 备份 → 改写 → 重启 ==="
BAK="${ETC_DIR}/backup-$(date +%m%d%H%M)"
mkdir -p "$BAK"; cp "${ETC_DIR}"/*.toml "$BAK/"
sed -i "s/remote = \"[0-9.]*:/remote = \"${NEW}:/g" "${ETC_DIR}"/*.toml
INSTANCES=$(systemctl list-units 'realm-relay@*' --no-legend --plain | awk '{print $1}' | sed 's/realm-relay@//;s/\.service//')
[ -n "$INSTANCES" ] || { echo "ERROR: 无运行实例"; exit 1; }
BAD=0
for i in $INSTANCES; do
  systemctl restart "realm-relay@${i}" 2>/dev/null || BAD=1
done
sleep 1
for i in $INSTANCES; do systemctl is-active --quiet "realm-relay@${i}" || BAD=1; done
if [ $BAD -eq 1 ]; then
  echo "实例异常，回滚 → ${BAK}"
  cp "${BAK}"/*.toml "${ETC_DIR}/"
  for i in $INSTANCES; do systemctl restart "realm-relay@${i}" 2>/dev/null || true; done
  journalctl -u 'realm-relay@*' -n 15 --no-pager; exit 1
fi
echo "  全部实例已重启 ✓（备份: ${BAK}）"

echo "=== [3/3] 完成 ==="
echo "用户侧无需任何动作（订阅入口未变）。旧落地 ${OLD} 保留观察 24h 后再处置。"
echo "接入 agent 后建议改用面板 [保存并下发] 切换落地（自动映射内核端口）。"
