#!/usr/bin/env bash
#
# One-time droplet preparation for mcp-proxy. Run as root ON THE DROPLET.
#
#   curl -fsSLO https://raw.githubusercontent.com/christianjombo/go-dbmcp/main/scripts/bootstrap-droplet.sh
#   less bootstrap-droplet.sh          # read it before running it
#   chmod +x bootstrap-droplet.sh
#   DOMAIN=dbmcp.hookman.online EMAIL=you@example.com ./bootstrap-droplet.sh
#
# Idempotent: safe to re-run. It will not overwrite an existing environment
# file, and it will not request a certificate that already exists.
#
# It deliberately does NOT start the service — the binary is shipped
# separately by scripts/deploy.sh from your workstation.

set -euo pipefail

DOMAIN="${DOMAIN:-dbmcp.hookman.online}"
EMAIL="${EMAIL:-}"
REPO_RAW="${REPO_RAW:-https://raw.githubusercontent.com/christianjombo/go-dbmcp/main}"

SERVICE_USER=mcpproxy
APP_DIR=/opt/mcp-proxy
ETC_DIR=/etc/mcp-proxy
BINARY=mcp-proxy-binary
NGINX_SITE=/etc/nginx/sites-available/mcp-proxy

say()  { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '    \033[0;32mok\033[0m %s\n' "$*"; }
warn() { printf '    \033[0;33m!\033[0m  %s\n' "$*"; }
die()  { printf '\n\033[0;31mFAILED:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo -i)"
[ -n "$EMAIL" ] || die "set EMAIL=you@example.com — Let's Encrypt requires it for expiry notices"

say "Target domain: $DOMAIN"

# ---------------------------------------------------------------------------
# 1. Confirm DNS points here BEFORE touching certbot. A certificate request
#    against an unpropagated record burns Let's Encrypt rate limit (5 failures
#    per account per hostname per hour), so this check saves real time.
# ---------------------------------------------------------------------------
say "Checking DNS"
command -v dig >/dev/null || apt-get install -y -qq dnsutils >/dev/null 2>&1 || true
resolved="$(dig +short "$DOMAIN" A | tail -1 || true)"
public_ip="$(curl -fsS --max-time 10 https://api.ipify.org || echo '')"
if [ -z "$resolved" ]; then
  die "$DOMAIN does not resolve yet. Add an A record pointing at ${public_ip:-this droplet} and wait for propagation."
elif [ -n "$public_ip" ] && [ "$resolved" != "$public_ip" ]; then
  die "$DOMAIN resolves to $resolved but this droplet is $public_ip. Fix the A record first."
else
  ok "$DOMAIN -> $resolved"
fi

# ---------------------------------------------------------------------------
# 2. Packages
# ---------------------------------------------------------------------------
say "Installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq nginx certbot python3-certbot-nginx ufw curl ca-certificates >/dev/null
ok "nginx, certbot, ufw"

# Security updates applied automatically; this box is internet-facing.
apt-get install -y -qq unattended-upgrades >/dev/null
dpkg-reconfigure -f noninteractive unattended-upgrades >/dev/null 2>&1 || true
ok "unattended security upgrades"

# ---------------------------------------------------------------------------
# 3. Service account and directories
# ---------------------------------------------------------------------------
say "Creating service account and directories"
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  adduser --system --group --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER" >/dev/null
  ok "user $SERVICE_USER created"
else
  ok "user $SERVICE_USER already exists"
fi

install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$APP_DIR"
install -d -o root -g "$SERVICE_USER" -m 0750 "$ETC_DIR"
ok "$APP_DIR and $ETC_DIR"

# ---------------------------------------------------------------------------
# 4. Fetch deployment files. Prefer a local checkout if the script is being
#    run from inside the repo; otherwise pull from GitHub.
# ---------------------------------------------------------------------------
say "Fetching deployment files"
fetch() { # fetch <relative-path> <destination>
  local rel="$1" dst="$2"
  if [ -f "$(dirname "$0")/../$rel" ]; then
    cp "$(dirname "$0")/../$rel" "$dst"
    ok "$rel (local checkout)"
  else
    curl -fsSL "$REPO_RAW/$rel" -o "$dst" || die "could not fetch $rel"
    ok "$rel (github)"
  fi
}
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fetch deploy/mcp-proxy.service     "$tmp/mcp-proxy.service"
fetch deploy/nginx-mcp-proxy.conf  "$tmp/nginx.conf"
fetch deploy/mcp-proxy.env.example "$tmp/env.example"

# ---------------------------------------------------------------------------
# 5. Environment file. Never overwritten — it may hold tuned production values.
# ---------------------------------------------------------------------------
say "Configuring environment"
if [ -f "$ETC_DIR/mcp-proxy.env" ]; then
  warn "$ETC_DIR/mcp-proxy.env exists; leaving it untouched"
else
  cp "$tmp/env.example" "$ETC_DIR/mcp-proxy.env"
  # Harden the defaults for a public multi-tenant deployment:
  #  - listen on loopback only, since nginx fronts us
  #  - writes left enabled (the command policy permits INSERT/UPDATE/DELETE);
  #    set MCP_ALLOW_WRITES=false for a query-only deployment
  #  - refuse private/loopback targets so this host cannot be used to reach
  #    networks the caller should not be able to reach from the internet
  sed -i \
    -e 's|^MCP_LISTEN_ADDR=.*|MCP_LISTEN_ADDR=127.0.0.1:8080|' \
    -e 's|^MCP_BLOCK_PRIVATE_HOSTS=.*|MCP_BLOCK_PRIVATE_HOSTS=true|' \
    -e 's|^MCP_DISABLE_LOCALHOST_PROTECTION=.*|MCP_DISABLE_LOCALHOST_PROTECTION=true|' \
    "$ETC_DIR/mcp-proxy.env"
  chown root:"$SERVICE_USER" "$ETC_DIR/mcp-proxy.env"
  chmod 0640 "$ETC_DIR/mcp-proxy.env"
  ok "wrote $ETC_DIR/mcp-proxy.env with multi-tenant defaults"
fi

# ---------------------------------------------------------------------------
# 6. Firewall. 8080 is NOT opened: nginx reaches it over loopback.
# ---------------------------------------------------------------------------
say "Configuring firewall"
ufw allow OpenSSH   >/dev/null
ufw allow 80/tcp    >/dev/null
ufw allow 443/tcp   >/dev/null
ufw --force enable  >/dev/null
ok "$(ufw status | tr '\n' ' ' | sed 's/  */ /g')"

# ---------------------------------------------------------------------------
# 7. TLS. Obtain the certificate with a temporary HTTP-only vhost, using
#    `certonly` so certbot does not rewrite our carefully tuned SSE config.
# ---------------------------------------------------------------------------
say "Obtaining TLS certificate"
if [ -f "/etc/letsencrypt/live/$DOMAIN/fullchain.pem" ]; then
  ok "certificate already present; skipping issuance"
else
  install -d -m 0755 /var/www/html
  cat >"$NGINX_SITE" <<EOF
# Temporary HTTP-only vhost, replaced below once the certificate exists.
server {
    listen 80;
    listen [::]:80;
    server_name $DOMAIN;
    location /.well-known/acme-challenge/ { root /var/www/html; }
    location / { return 404; }
}
EOF
  ln -sf "$NGINX_SITE" /etc/nginx/sites-enabled/mcp-proxy
  rm -f /etc/nginx/sites-enabled/default
  nginx -t >/dev/null 2>&1 || die "temporary nginx config failed validation"
  systemctl reload nginx

  certbot certonly --webroot -w /var/www/html -d "$DOMAIN" \
    --non-interactive --agree-tos -m "$EMAIL" \
    || die "certbot failed — check that $DOMAIN resolves here and port 80 is reachable"
  ok "certificate issued for $DOMAIN"
fi

# ---------------------------------------------------------------------------
# 8. Real nginx config, with the SSE-critical settings.
# ---------------------------------------------------------------------------
say "Installing nginx site"
sed "s/dbmcp\.hookman\.online/$DOMAIN/g" "$tmp/nginx.conf" >"$NGINX_SITE"
ln -sf "$NGINX_SITE" /etc/nginx/sites-enabled/mcp-proxy
rm -f /etc/nginx/sites-enabled/default
nginx -t >/dev/null 2>&1 || { nginx -t; die "nginx config failed validation"; }
systemctl reload nginx
ok "nginx serving $DOMAIN with proxy_buffering off and a 3600s read timeout"

systemctl list-timers 2>/dev/null | grep -q certbot && ok "certbot auto-renewal timer active" \
  || warn "certbot renewal timer not found; check: systemctl list-timers | grep certbot"

# ---------------------------------------------------------------------------
# 9. systemd unit. The service is enabled but only started if the binary is
#    already present, so a fresh droplet does not sit in a restart loop.
# ---------------------------------------------------------------------------
say "Installing systemd unit"
install -m 0644 "$tmp/mcp-proxy.service" /etc/systemd/system/mcp-proxy.service
systemctl daemon-reload
systemctl enable mcp-proxy >/dev/null 2>&1
ok "mcp-proxy.service installed and enabled at boot"

echo
if [ -x "$APP_DIR/$BINARY" ]; then
  systemctl restart mcp-proxy
  sleep 2
  systemctl is-active --quiet mcp-proxy && ok "service running" || warn "service not active: journalctl -u mcp-proxy -n 50"
else
  printf '\033[1;33m%s\033[0m\n' "Droplet is ready. The binary is not here yet."
  echo
  echo "From your workstation, in the repo:"
  echo "    scripts/deploy.sh root@${public_ip:-<droplet-ip>}"
  echo
  echo "That builds a static linux binary, ships it, and starts the service."
fi
echo
echo "Verify once deployed:"
echo "    curl -s https://$DOMAIN/healthz"
echo "    journalctl -u mcp-proxy -f"
