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
