#!/usr/bin/env bash
set -euo pipefail

: "${SHELLCRASH_IMAGE:?SHELLCRASH_IMAGE must be set to a digest-pinned official image}"
tmp=$(mktemp -d)
name=shellcrash-fnos-smoke
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

mkdir -p "$tmp/etc" "$tmp/data"
TRIM_PKGETC="$tmp/etc" \
TRIM_PKGVAR="$tmp/data" \
bash cmd/install_init
secret=$(cat "$tmp/etc/api.secret")

docker run -d --name "$name" \
  -p 127.0.0.1::9999/tcp \
  -v "$tmp/data/ShellCrash/configs:/etc/ShellCrash/configs" \
  -v "$tmp/data/ShellCrash/yamls:/etc/ShellCrash/yamls" \
  -v "$tmp/data/ShellCrash/jsons:/etc/ShellCrash/jsons" \
  -e TZ=Asia/Shanghai \
  "$SHELLCRASH_IMAGE" >/dev/null

port=$(docker port "$name" 9999/tcp | tail -n 1 | awk -F: '{print $NF}')
[[ "$port" =~ ^[0-9]+$ ]]
base="http://127.0.0.1:$port"
ready=false
for _ in $(seq 1 36); do
  if curl --silent --fail --max-time 2 \
      -H "Authorization: Bearer $secret" "$base/version" >/dev/null; then
    ready=true
    break
  fi
  sleep 5
done
if [ "$ready" != true ]; then
  echo "ShellCrash API did not become ready"
  exit 1
fi

code=$(curl --silent --output /dev/null --write-out '%{http_code}' "$base/version")
if [ "$code" != 401 ]; then
  echo "ShellCrash API did not reject a request without its token"
  exit 1
fi

curl --silent --show-error --fail "$base/ui/" -o "$tmp/dashboard.html"
grep -Eiq '<!doctype html|<html' "$tmp/dashboard.html"
test -s "$tmp/data/ShellCrash/configs/.autostart"
echo "Official ShellCrash dashboard and token-protected API passed smoke test"
