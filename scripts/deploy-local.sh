#!/usr/bin/env bash
# Installs the freshly built website binary, service unit, and listen-address file on this host, restarts the service,
# and rolls back to the previous binary and unit when the origin does not come up healthy.
set -euo pipefail

# Host-specific values come from the environment (GitHub secrets in the deploy workflow):
# DEPLOY_LISTEN_ADDR is the address the service binds, DEPLOY_ORIGIN the URL used to health-check it.
deploy_dir="${DEPLOY_DIR:-$HOME/decryptmypack}"
service_dir="${DEPLOY_SERVICE_DIR:-$HOME/.config/systemd/user}"
listen_addr="${DEPLOY_LISTEN_ADDR:?set DEPLOY_LISTEN_ADDR to the service listen address}"
origin="${DEPLOY_ORIGIN:?set DEPLOY_ORIGIN to the origin health-check URL}"
env_file="$deploy_dir/decryptmypack.env"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"
mkdir -p "$deploy_dir" "$service_dir"
exec 9>"$deploy_dir/.deploy.lock"
flock 9

test -s decryptmypack-linux-amd64
install -m 755 decryptmypack-linux-amd64 "$deploy_dir/decryptmypack.new"
install -m 644 deploy/decryptmypack.service "$service_dir/decryptmypack.service.new"
(umask 077 && printf 'LISTEN_ADDR=%s\n' "$listen_addr" > "$env_file.new")

# A first install has nothing to roll back to; later deploys keep the running binary and unit.
first_install=false
if [[ -f "$deploy_dir/decryptmypack" && -f "$service_dir/decryptmypack.service" ]]; then
  cp -p "$deploy_dir/decryptmypack" "$deploy_dir/decryptmypack.previous"
  cp -p "$service_dir/decryptmypack.service" "$service_dir/decryptmypack.service.previous"
  if [[ -f "$env_file" ]]; then
    cp -p "$env_file" "$env_file.previous"
  else
    rm -f "$env_file.previous"
  fi
else
  first_install=true
fi
mv "$deploy_dir/decryptmypack.new" "$deploy_dir/decryptmypack"
mv "$service_dir/decryptmypack.service.new" "$service_dir/decryptmypack.service"
mv "$env_file.new" "$env_file"

rollback() {
  if [[ "$first_install" == true ]]; then
    echo 'First deployment failed; there is no previous version to restore.' >&2
    systemctl --user stop decryptmypack.service || true
    return
  fi
  echo 'Deployment failed; restoring the previous service.' >&2
  cp -p "$deploy_dir/decryptmypack.previous" "$deploy_dir/decryptmypack.restore"
  mv "$deploy_dir/decryptmypack.restore" "$deploy_dir/decryptmypack"
  cp -p "$service_dir/decryptmypack.service.previous" "$service_dir/decryptmypack.service"
  if [[ -f "$env_file.previous" ]]; then
    cp -p "$env_file.previous" "$env_file"
  else
    rm -f "$env_file"
  fi
  systemctl --user daemon-reload
  systemctl --user restart decryptmypack.service
}

healthy() {
  systemctl --user is-active --quiet decryptmypack.service &&
    curl -fsS --max-time 5 "$origin/health" >/dev/null &&
    curl -fsS --max-time 5 "$origin/" | grep -qi 'decrypt my pack'
}

if ! systemctl --user daemon-reload || ! systemctl --user enable decryptmypack.service || ! systemctl --user restart decryptmypack.service; then
  rollback
  exit 1
fi
for attempt in {1..15}; do
  if healthy; then
    git rev-parse HEAD > "$deploy_dir/deployed-sha"
    echo "Deployed $(git rev-parse --short HEAD); the website is healthy."
    exit 0
  fi
  sleep 1
done
rollback
exit 1
