#!/bin/bash
# One-time install of feedbridge on the Pi: INSTALL.md steps 2-4.
# Run from your own account: sudo deploy/setup.sh
# Safe to run again: existing config, secret and URL are kept.
set -euo pipefail

CONF_DIR=/etc/feedbridge
DATA_DIR=/var/lib/feedbridge
CONFIG=$CONF_DIR/config.toml
SECRET=$CONF_DIR/webhook.secret
URL=$CONF_DIR/webhook.url

if [ "$(id -u)" -ne 0 ] || [ -z "${SUDO_USER:-}" ] || [ "$SUDO_USER" = root ]; then
	echo "Run this from your own account with sudo: sudo deploy/setup.sh" >&2
	exit 1
fi
REPO=$(cd "$(dirname "$0")/.." && pwd)
GO=$(getent passwd "$SUDO_USER" | cut -d: -f6)/.local/go/bin/go
as_user() { sudo -H -u "$SUDO_USER" "$@"; }
# An empty file, mode 0600, owned by feedbridge; writing with > keeps both.
new_file() { install -m 0600 -o feedbridge -g feedbridge /dev/null "$1"; }

echo "== Building feedbridge"
[ -x "$GO" ] || { echo "Go not found at $GO" >&2; exit 1; }
cd "$REPO"
VERSION=1.0.0+$(as_user git rev-parse --short HEAD)
as_user env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO" build \
	-ldflags "-X main.version=$VERSION" -o "$REPO/feedbridge" ./cmd/feedbridge
install -m 755 "$REPO/feedbridge" /usr/local/bin/feedbridge
echo "Installed /usr/local/bin/feedbridge ($VERSION)"

echo "== User and directories"
if ! id feedbridge >/dev/null 2>&1; then
	useradd --system --no-create-home --shell /usr/sbin/nologin feedbridge
fi
install -d -m 0700 -o feedbridge -g feedbridge "$DATA_DIR"
install -d -m 0750 -o root -g feedbridge "$CONF_DIR"

echo "== Config"
if [ ! -s "$CONFIG" ]; then
	# Groups stay empty until start.sh writes the two JIDs from --list-groups.
	new_file "$CONFIG"
	sed 's/^groups = .*/groups = []/' "$REPO/deploy/config.example.toml" >"$CONFIG"
	echo "Wrote $CONFIG"
else
	echo "Keeping existing $CONFIG"
fi

echo "== Webhook secret"
if [ ! -s "$SECRET" ]; then
	new_file "$SECRET"
	openssl rand -hex 32 >"$SECRET"
	echo
	echo "At script.google.com, create a project and paste in appscript/Code.js from this repo."
	echo "Then Project Settings > Script properties, add FEED_SECRET with this value:"
	echo
	echo "    $(cat "$SECRET")"
	echo
	echo "Then Deploy > New deployment > Web app (Execute as: Me, Who has access: Anyone)."
else
	echo "Keeping existing secret (show it with: sudo cat $SECRET)"
fi

echo "== Webhook URL"
if [ ! -s "$URL" ]; then
	while :; do
		read -r -p "Paste the web app URL (ends in /exec): " u
		case "$u" in
		https://script.google.com/*/exec) break ;;
		*) echo "That should start https://script.google.com/ and end /exec." ;;
		esac
	done
	new_file "$URL"
	printf '%s\n' "$u" >"$URL"
	echo "Saved $URL"
else
	echo "Keeping existing $URL"
fi

echo "== systemd"
install -m 644 "$REPO/deploy/feedbridge.service" "$REPO/deploy/feedbridge.timer" /etc/systemd/system/
systemctl daemon-reload
# The Pi 5 has no RTC battery: without this, After=time-sync.target doesn't wait for real time.
systemctl enable systemd-time-wait-sync.service
if ! systemctl -q is-enabled NetworkManager-wait-online.service 2>/dev/null &&
	! systemctl -q is-enabled systemd-networkd-wait-online.service 2>/dev/null; then
	echo "Warning: no wait-online service is enabled, so network-online.target won't wait for the network." >&2
fi

echo "== Checking the webhook"
reply=$(curl -sL --max-time 60 -H 'Content-Type: application/json' \
	-d '{"secret":"wrong","id":"setup-check","date":"x","text":"x"}' "$(cat "$URL")" || true)
if [ "$reply" = forbidden ]; then
	echo "Apps Script answered 'forbidden' to a wrong secret, as it should."
else
	echo "Warning: expected 'forbidden' from Apps Script, got something else. Check the deployment." >&2
fi

cat <<EOF

Done. Next:
  1. sudo -u feedbridge feedbridge --link          (scan the QR in the monitoring account)
  2. sudo -u feedbridge feedbridge --list-groups   (find the two group JIDs)
  3. sudo deploy/start.sh <JID1> <JID2>
EOF
