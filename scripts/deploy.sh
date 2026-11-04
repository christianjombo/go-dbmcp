#!/usr/bin/env bash
#
# Build and ship mcp-proxy to a droplet. Run from your workstation, from the
# repository root.
#
#   scripts/deploy.sh root@203.0.113.10
#   scripts/deploy.sh root@203.0.113.10 --arch arm64     # for an ARM droplet
#
# Assumes scripts/bootstrap-droplet.sh has already been run on the target.
#
# The upload lands on a temporary path and is moved into place only after it
# arrives intact, so an interrupted transfer cannot leave a truncated binary
# where systemd will try to execute it.

set -euo pipefail

TARGET="${1:-}"
ARCH=amd64

shift || true
while [ $# -gt 0 ]; do
  case "$1" in
    --arch) ARCH="${2:?--arch needs a value}"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$TARGET" ]; then
  cat >&2 <<'USAGE'
usage: scripts/deploy.sh user@host [--arch amd64|arm64]

  user@host   the droplet, e.g. root@203.0.113.10
  --arch      target architecture (default: amd64; use arm64 for ARM droplets)
USAGE
  exit 2
fi

APP_DIR=/opt/mcp-proxy
BINARY=mcp-proxy-binary
SERVICE_USER=mcpproxy

say() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()  { printf '    \033[0;32mok\033[0m %s\n' "$*"; }
die() { printf '\n\033[0;31mFAILED:\033[0m %s\n' "$*" >&2; exit 1; }

[ -f go.mod ] || die "run this from the repository root"

say "Running verification before shipping"
make verify || die "verification failed — not deploying"

say "Building static linux/$ARCH binary"
make release GOARCH="$ARCH" >/dev/null || die "build failed"
[ -f "bin/$BINARY" ] || die "bin/$BINARY missing after build"
ok "$(ls -lh "bin/$BINARY" | awk '{print $5}') static binary"

# Checksum both sides so a corrupted transfer is caught before it is installed.
local_sum="$( (sha256sum "bin/$BINARY" 2>/dev/null || shasum -a 256 "bin/$BINARY") | awk '{print $1}')"
ok "sha256 ${local_sum:0:16}..."

say "Uploading to $TARGET"
scp -q "bin/$BINARY" "$TARGET:$APP_DIR/$BINARY.new" || die "upload failed"
ok "uploaded"

say "Installing and restarting"
ssh "$TARGET" "bash -euo pipefail -s" <<REMOTE || die "remote install failed"
  remote_sum="\$(sha256sum "$APP_DIR/$BINARY.new" | awk '{print \$1}')"
  if [ "\$remote_sum" != "$local_sum" ]; then
    rm -f "$APP_DIR/$BINARY.new"
    echo "checksum mismatch: expected $local_sum, got \$remote_sum" >&2
    exit 1
  fi
  echo "    checksum verified on droplet"

  chown $SERVICE_USER:$SERVICE_USER "$APP_DIR/$BINARY.new"
  chmod 0755 "$APP_DIR/$BINARY.new"
  mv -f "$APP_DIR/$BINARY.new" "$APP_DIR/$BINARY"

  systemctl restart mcp-proxy
  # TimeoutStopSec is 30s and a graceful drain can take a few seconds.
  for i in \$(seq 1 20); do
    systemctl is-active --quiet mcp-proxy && break
    sleep 1
  done
  systemctl is-active --quiet mcp-proxy || {
    echo "service did not come up:" >&2
    journalctl -u mcp-proxy -n 40 --no-pager >&2
    exit 1
  }
  echo "    service active: \$($APP_DIR/$BINARY -version)"

  # Prove it is actually serving, not merely running.
  curl -fsS --max-time 5 http://127.0.0.1:8080/healthz >/dev/null || {
    echo "health check failed:" >&2
    journalctl -u mcp-proxy -n 40 --no-pager >&2
    exit 1
  }
  echo "    local health check passed"
REMOTE

say "Deployed"
echo
echo "Check it from here:"
echo "    curl -s https://dbmcp.hookman.online/healthz"
echo "    ssh $TARGET journalctl -u mcp-proxy -f"
