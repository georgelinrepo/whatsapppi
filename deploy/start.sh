#!/bin/bash
# Put the watched groups in the config, enable the daily timer and run once now.
# Usage: sudo deploy/start.sh 120363...@g.us 120363...@g.us   (just these groups)
#        sudo deploy/start.sh --all                           (every joined group)
set -euo pipefail

CONFIG=/etc/feedbridge/config.toml

if [ "$(id -u)" -ne 0 ]; then
	echo "Run with sudo: sudo deploy/start.sh <JID1> <JID2> | --all" >&2
	exit 1
fi
if [ $# -lt 1 ]; then
	echo "Usage: sudo deploy/start.sh <JID1> [<JID2> ...] | --all  (JIDs from --list-groups)" >&2
	exit 2
fi
list=
all=false
if [ "$1" = "--all" ] && [ $# -eq 1 ]; then
	all=true
else
	for j in "$@"; do
		[[ "$j" =~ ^[0-9]+(-[0-9]+)?@g\.us$ ]] || { echo "Not a group JID: $j" >&2; exit 2; }
		list+="${list:+, }\"$j\""
	done
fi

# Rewrite in place (not sed -i), so the file keeps its owner and mode.
# exclude_groups only applies with all_groups = true, so a JID list comments it out.
keep_exclude=
[ "$all" = true ] || keep_exclude='s/^exclude_groups = /# &/'
new=$(sed -e '/^all_groups = /d' -e "$keep_exclude" \
	-e "s/^groups = .*/groups = [$list]\nall_groups = $all/" "$CONFIG")
printf '%s\n' "$new" >"$CONFIG"
grep -E '^(groups|all_groups|exclude_groups) = ' "$CONFIG"

systemctl enable --now feedbridge.timer
echo "Running once now (up to a few minutes while it waits for WhatsApp to sync)..."
if systemctl start feedbridge; then
	echo "Run finished. Check your email for WA-FEED; the footer should say 'whatsapp: synced'."
else
	echo "The run failed; see the log below." >&2
fi
journalctl -u feedbridge -n 20 --no-pager
systemctl list-timers feedbridge.timer --no-pager
