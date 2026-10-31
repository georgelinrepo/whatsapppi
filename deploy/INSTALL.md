# Installing feedbridge on the Pi

These steps cover spec §One-time setup 4–7. Steps 1–3 (SIM, second WhatsApp
account, joining the groups) come first; step 8 is the one-week soak test
(acceptance test 23), before relying on the feed.

## Quick path

Two scripts do the `sudo` work in the sections below. Read them first.

```sh
sudo deploy/setup.sh                          # sections 2-4; prints the secret for Apps Script, asks for the /exec URL
sudo -u feedbridge feedbridge --link          # section 5: scan the QR in the monitoring account
sudo -u feedbridge feedbridge --list-groups   # find the group JIDs
sudo deploy/start.sh <JID1> <JID2>            # writes the JIDs, enables the timer, runs once
#   or: sudo deploy/start.sh --all            # every joined group instead (see section 7)
```

Then do section 7 on the phone. The sections below are the same steps by hand.

## 1. Apps Script (setup step 4)

Create the script from the spec's Appendix and deploy it as a web app
(Execute as: Me, Who has access: Anyone). Generate the shared secret with:

```sh
openssl rand -hex 32
```

Set it as the script property `FEED_SECRET`, and keep the `/exec` URL and the
secret for step 3 below.

## 2. Build and install the binary (setup step 5)

Go is at `~/.local/go/bin` on this Pi.

```sh
export PATH=$HOME/.local/go/bin:$PATH
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=1.0.0" -o feedbridge ./cmd/feedbridge
sudo install -m 755 feedbridge /usr/local/bin/feedbridge
```

## 3. User, directories, config and secrets (setup step 5)

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin feedbridge
# Must exist before the first run, or the unit fails on ReadWritePaths.
sudo install -d -m 0700 -o feedbridge -g feedbridge /var/lib/feedbridge
sudo install -d -m 0750 -o root -g feedbridge /etc/feedbridge

sudo install -m 0600 -o feedbridge -g feedbridge deploy/config.example.toml /etc/feedbridge/config.toml
# Paste the /exec URL and the secret from step 1 (one line each).
sudo install -m 0600 -o feedbridge -g feedbridge /dev/null /etc/feedbridge/webhook.url
sudo install -m 0600 -o feedbridge -g feedbridge /dev/null /etc/feedbridge/webhook.secret
sudoedit /etc/feedbridge/webhook.url
sudoedit /etc/feedbridge/webhook.secret
```

`--link` and `--list-groups` load and validate the whole config, so
`webhook.url` and `webhook.secret` must already be filled in before step 5.
`whatsapp.groups` may be empty (`groups = []`) until after `--list-groups`;
only `--run` refuses an empty list, unless `all_groups = true`.

## 4. systemd units (setup step 5)

```sh
sudo install -m 644 deploy/feedbridge.service deploy/feedbridge.timer /etc/systemd/system/
sudo systemctl daemon-reload
```

The Pi 5's RTC has no battery, so a run at boot must wait for real time.
`After=time-sync.target` only waits if this service is enabled:

```sh
sudo systemctl enable systemd-time-wait-sync.service
```

Check that something makes `network-online.target` mean "online" (one of
these should print `enabled`):

```sh
systemctl is-enabled NetworkManager-wait-online.service
systemctl is-enabled systemd-networkd-wait-online.service
```

## 5. Link (setup step 6)

```sh
sudo -u feedbridge feedbridge --link
```

Scan the QR code in the monitoring account: Linked devices > Link a device.
WhatsApp drops history from before a first link; this is accepted.

```sh
sudo -u feedbridge feedbridge --list-groups
```

Copy the group JIDs into `whatsapp.groups` in `/etc/feedbridge/config.toml`
(`sudoedit`), replacing the placeholders. Or, to take every group the
monitoring number is in, set `groups = []` and `all_groups = true`, and list
any groups to leave out in `exclude_groups`.

## 6. Start (setup step 7)

```sh
sudo systemctl enable --now feedbridge.timer
sudo systemctl start feedbridge
journalctl -u feedbridge
```

Confirm a WA-FEED email arrives and the health footer says `whatsapp: synced`.

## 7. On the phone

Mute and archive the watched groups in the monitoring account and never open
them there: WhatsApp sends read receipts in groups even with read receipts off.

With `all_groups = true`, also set Settings > Privacy > Groups to
**My contacts** in the monitoring account. It has no saved contacts, so
nobody can add it to a group directly: each add becomes an invite you accept
on the phone, and accepting it is what puts the group in the feed.
