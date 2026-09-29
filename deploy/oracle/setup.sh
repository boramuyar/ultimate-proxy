#!/usr/bin/env bash
# Sets up Ultimate Proxy on a fresh Ubuntu VM (built for Oracle Cloud's Always
# Free Ampere instances, but any Ubuntu 22.04+ host works).
#
# Paste this file into "Advanced options > Management > Initialization script"
# when creating the instance, or run it later with: sudo bash setup.sh
#
# It is safe to run again: it pulls the latest main and restarts the stack,
# keeping existing credentials and data.
set -euo pipefail

REPO_URL=${REPO_URL:-https://github.com/boramuyar/ultimate-proxy.git}
DIR=/opt/ultimate-proxy
exec > >(tee -a /var/log/ultimate-proxy-setup.log) 2>&1

echo "== installing docker and git"
if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sh
fi
command -v git >/dev/null || (apt-get update -y && apt-get install -y git)

echo "== opening port 8080"
# Oracle's Ubuntu images reject inbound traffic in iptables except SSH.
if ! iptables -C INPUT -p tcp --dport 8080 -j ACCEPT 2>/dev/null; then
  iptables -I INPUT 1 -p tcp --dport 8080 -j ACCEPT
  if command -v netfilter-persistent >/dev/null; then netfilter-persistent save; fi
fi

echo "== fetching the code"
if [ -d "$DIR/.git" ]; then
  git -C "$DIR" fetch -q origin main && git -C "$DIR" reset -q --hard origin/main
else
  git clone -q "$REPO_URL" "$DIR"
fi

echo "== writing credentials"
ENV="$DIR/.env"
if [ ! -f "$ENV" ]; then
  rand() { head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32; }
  umask 077
  cat > "$ENV" <<VARS
PROXY_ADMIN_TOKEN=$(rand)
DEMO_APP_KEY=up_$(rand)
POSTGRES_PASSWORD=$(rand)
# Add a real provider key here, then run: sudo bash $DIR/deploy/oracle/setup.sh
OPENAI_API_KEY=
VARS
fi

echo "== starting the stack"
cd "$DIR/deploy/oracle"
docker compose --env-file "$ENV" up -d --build

for _ in $(seq 60); do
  curl -sf localhost:8080/healthz >/dev/null && break
  sleep 2
done

set -a; . "$ENV"; set +a
IP=$(curl -s --max-time 5 https://ifconfig.me || echo "<public ip>")
cat <<DONE

Ultimate Proxy is running on http://$IP:8080
Admin token: $PROXY_ADMIN_TOKEN
Demo key:    $DEMO_APP_KEY
(both are stored in $ENV)

Try it:
  curl http://$IP:8080/v1/responses -H "Authorization: Bearer $DEMO_APP_KEY" \\
    -H "X-Proxy-User-Email: you@example.com" -d '{"model":"fake-gpt","input":"hi"}'
  curl "http://$IP:8080/admin/usage?group_by=email" -H "Authorization: Bearer $PROXY_ADMIN_TOKEN"
DONE
