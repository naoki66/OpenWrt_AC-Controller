#!/bin/sh
# 11k 邻居列表的真机验证。
#
# 默认只读，不会改动设备上的任何状态；加 --write 才会执行 rrm_nr_set。
#
#   ./scripts/verify-11k.sh            # 只读：看每个 BSS 的自身报告与当前列表
#   ./scripts/verify-11k.sh --write    # 额外做一次可逆写入自验证
#
# --write 做的是「把空列表再设成空列表」：它验证 rrm_nr_set 这条路通，
# 但不改变任何实际内容（本来就空）。当前列表非空的 BSS 会被跳过，不会被覆盖。
# rrm_nr_set 只改 hostapd 内存，不写 /etc/config，`wifi reload` 或重启即清除。
#
# 环境变量：AC_AP_HOST（默认 10.0.30.251）、AC_AP_USER（默认 root）。
set -eu

host=${AC_AP_HOST:-10.0.30.251}
user=${AC_AP_USER:-root}
write=0
for arg in "$@"; do
	case "$arg" in
	--write) write=1 ;;
	-h | --help)
		sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "未知参数：$arg（支持 --write）" >&2
		exit 2
		;;
	esac
done

run() { ssh -o ConnectTimeout=8 -o BatchMode=yes "$user@$host" "$1"; }
say() { printf '\n== %s\n' "$1"; }

# hostapd 输出的是带缩进的 JSON（"list":[ 换行 制表符 ]），先压平再判断，
# 否则「有没有条目」会被空白差异误判。
flat() { printf '%s' "$1" | tr -d ' \t\n\r'; }
is_empty_list() {
	case "$(flat "$1")" in
	*'"list":[]'*) return 0 ;;
	*) return 1 ;;
	esac
}

say "主机 $user@$host"
run 'ubus call system board | sed -n "s/.*\"model\": \"\([^\"]*\)\".*/model: \1/p"; uname -r'

say "hostapd BSS 列表"
bss_list=$(run 'ubus list | sed -n "s/^hostapd\.//p"')
if [ -z "$bss_list" ]; then
	echo "没有 hostapd.* 对象，无线可能没起来" >&2
	exit 1
fi
printf '%s\n' "$bss_list"

say "每个 BSS 的自身邻居报告（控制器采集的正是这个三元组）"
for bss in $bss_list; do
	printf '\n--- %s\n' "$bss"
	run "ubus call hostapd.$bss rrm_nr_get_own" || echo "  rrm_nr_get_own 不可用"
done

say "每个 BSS 当前的邻居列表"
empty_count=0
total=0
for bss in $bss_list; do
	total=$((total + 1))
	cur=$(run "ubus call hostapd.$bss rrm_nr_list" || true)
	if is_empty_list "$cur"; then
		empty_count=$((empty_count + 1))
	fi
	printf '%-22s %s\n' "$bss" "$(flat "$cur")"
done
printf '\n空列表 %d / %d\n' "$empty_count" "$total"

if [ "$write" -eq 0 ]; then
	say "只读结束"
	cat <<'EOF'
未做任何写入。要验证 rrm_nr_set 这条路通，加 --write：

    ./scripts/verify-11k.sh --write

它只把「本来就空」的列表再设成空列表，不改内容、不写配置，reload 即清除。
EOF
	exit 0
fi

say "写入自验证（空 → 空，可逆）"
set_ok=0
skipped=0
for bss in $bss_list; do
	cur=$(run "ubus call hostapd.$bss rrm_nr_list" || true)
	if ! is_empty_list "$cur"; then
		printf '%-22s 跳过（当前列表非空，不在真机上覆盖）\n' "$bss"
		skipped=$((skipped + 1))
		continue
	fi
	if run "ubus call hostapd.$bss rrm_nr_set '{\"list\":[]}'" >/dev/null 2>&1; then
		set_ok=$((set_ok + 1))
	else
		printf '%-22s rrm_nr_set 失败\n' "$bss"
	fi
done
printf '\n设置成功 %d 个，跳过 %d 个\n' "$set_ok" "$skipped"

say "复核：接口仍在，且列表仍为空"
for bss in $bss_list; do
	up=$(run "ubus call hostapd.$bss get_status" 2>/dev/null | sed -n 's/.*"ssid": "\([^"]*\)".*/\1/p' | head -1)
	cur=$(run "ubus call hostapd.$bss rrm_nr_list" || true)
	printf '%-22s ssid=%-12s %s\n' "$bss" "${up:-?}" "$(flat "$cur")"
done

say "结论"
cat <<'EOF'
rrm_nr_set 可用 = 这条链路成立。接下来要做的是双 AP 的完整验证：

  1. 两台 AP 都被控制器采纳（agent 已在跑）；
  2. 对两台各点一次「重新下发」（新采纳的第一次包不含邻居列表，见
     docs/ac-11k-neighbors.md §6 限制 2）；
  3. 回到本脚本的只读模式，确认 rrm_nr_list 里出现了对方的 BSSID；
  4. 手机在两者之间走动，观察是否触发 FT 重关联。

若第 3 步仍为空，检查 agent 日志里的 "11k neighbour list set"、
以及控制器侧该 AP 是否真的上报了 neighbors（hello 的 neighbors 字段）。
EOF
