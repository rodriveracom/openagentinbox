#!/usr/bin/env bash
set -euo pipefail
origin=${1:?usage: bash install-client.sh https://inbox.example.org}
case "$origin" in https://*|http://127.0.0.1:*|http://localhost:*) ;; *) echo 'Use HTTPS or local loopback' >&2; exit 1;; esac
case "$(uname -s)/$(uname -m)" in
 Linux/aarch64|Linux/arm64) target=linux-arm64;;
 Linux/x86_64) target=linux-amd64;;
 Darwin/arm64) target=darwin-arm64;;
 *) echo 'Unsupported platform' >&2; exit 1;;
esac
dir=${INBOX_BIN_DIR:-"$HOME/.local/bin"}
mkdir -p "$dir"
stage=$(mktemp -d "$dir/.install.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT
curl -fsS "$origin/clients/SHA256SUMS" -o "$stage/sums"
awk -v name="$target" '$2 == name { print; found=1 } END { exit !found }' "$stage/sums" > "$stage/check"
curl -fsS "$origin/clients/$target" -o "$stage/$target"
(cd "$stage"; if command -v sha256sum >/dev/null; then sha256sum -c check; else shasum -a 256 -c check; fi)
chmod 755 "$stage/$target"
mv -f "$stage/$target" "$dir/openagentinbox-client"
# Short alias used by notices. Anything else already called oai is left alone.
if [[ ! -e "$dir/oai" && ! -L "$dir/oai" ]]; then ln -s openagentinbox-client "$dir/oai"; fi
printf '%s\n' "$dir/openagentinbox-client"
