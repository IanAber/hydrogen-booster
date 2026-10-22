#!/bin/bash
# Installs the hydrogen-booster systemd service (if not already present)
# and (re)starts it so the control program runs automatically, including
# across reboots. Run this as root (or with sudo) on the target device
# from the project root, after deploying dist/ARM64/hydrogen-booster.
if [ "$(id -u)" -ne 0 ]; then
    echo "Please run this as root or with sudo" >&2
    exit 2
fi

set -eu

BIN="hydrogen-booster"
SERVICE_USER="egadmin"
PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
SERVICE_NAME="hydrogen-booster.service"
SERVICE_SRC="$PROJECT_DIR/systemd/$SERVICE_NAME"
SERVICE_DST="/etc/systemd/system/$SERVICE_NAME"
CONFIG_NAME="config.json"
CONFIG_SRC="$PROJECT_DIR/$CONFIG_NAME"
CONFIG_DIR="/var/$BIN"
CONFIG_DST="$CONFIG_DIR/$CONFIG_NAME"
BIN_DST="/usr/bin/$BIN"
BINARY="$PROJECT_DIR/dist/ARM64/$BIN"
WEB_DST="/etc/$BIN/web"

if [ ! -x "$BINARY" ]; then
    echo "error: $BINARY not found or not executable - deploy the ARM64 build first" >&2
    exit 1
fi

if [ ! -f "$SERVICE_DST" ]; then
    echo "Installing $SERVICE_NAME to $SERVICE_DST"
    cp "$SERVICE_SRC" "$SERVICE_DST"
    systemctl daemon-reload
else
    echo "$SERVICE_DST already exists - leaving it as-is"
fi

# Persistent journal, so the service's log (faults, sensor failures,
# rejected commands) survives a reboot - otherwise the record of why the
# booster stopped is lost the moment someone power-cycles it. Recent
# Raspberry Pi OS images ship 40-rpi-volatile-storage.conf, which keeps
# the journal in RAM only; this drop-in sorts after it and overrides it.
#   MaxFileSec=1day  rotates the journal every 24 hours: the active file
#                    is archived and a new one started each day. Archived
#                    entries stay readable (journalctl, the admin
#                    screen's System Log) until SystemMaxUse is reached.
#   SystemMaxUse     caps the space the journal may take on the SD card -
#                    journald deletes the oldest archived files first.
# This file is owned by the installer, so it's rewritten whenever it
# differs from the settings below (journald restarted only then).
JOURNALD_DROPIN="/etc/systemd/journald.conf.d/60-$BIN-persistent.conf"
JOURNALD_CONF="# Installed by hydrogen-booster's install.sh - keep the journal across
# reboots so fault history is available via journalctl, rotating daily.
[Journal]
Storage=persistent
MaxFileSec=1day
SystemMaxUse=200M"
if [ ! -f "$JOURNALD_DROPIN" ] || [ "$(cat "$JOURNALD_DROPIN")" != "$JOURNALD_CONF" ]; then
    echo "Configuring persistent journal with daily rotation ($JOURNALD_DROPIN)"
    mkdir -p "$(dirname "$JOURNALD_DROPIN")"
    printf '%s\n' "$JOURNALD_CONF" > "$JOURNALD_DROPIN"
    mkdir -p /var/log/journal
    systemd-tmpfiles --create --prefix /var/log/journal
    systemctl restart systemd-journald
    journalctl --flush
else
    echo "$JOURNALD_DROPIN is up to date"
fi

# The admin screen's System Log page runs journalctl as the service
# user, which needs the systemd-journal group to read system journal
# entries. Takes effect when the service is restarted below.
if ! id -nG "$SERVICE_USER" | tr ' ' '\n' | grep -qx systemd-journal; then
    echo "Adding $SERVICE_USER to the systemd-journal group (for the System Log page)"
    usermod -aG systemd-journal "$SERVICE_USER"
fi

systemctl enable "$SERVICE_NAME"
systemctl stop "$SERVICE_NAME" 2>/dev/null || true

# Web UI static assets - always refreshed to match this build.
mkdir -p "/etc/$BIN"
rm -rf "$WEB_DST"
cp -r "$PROJECT_DIR/web" "$WEB_DST"

# Editable config - seeded once, never overwritten on later installs so
# calibration/tuning saved from the admin screen survives redeploys.
mkdir -p "$CONFIG_DIR"
if [ ! -f "$CONFIG_DST" ]; then
    cp "$CONFIG_SRC" "$CONFIG_DST"
fi
chown -R "$SERVICE_USER:$SERVICE_USER" "$CONFIG_DIR"

cp "$BINARY" "$BIN_DST"
chmod +x "$BIN_DST"

systemctl restart "$SERVICE_NAME"
systemctl --no-pager status "$SERVICE_NAME"
