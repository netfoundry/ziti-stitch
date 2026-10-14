#!/usr/bin/env bash
# Installs a pinned, checksum-verified Helm into /usr/local/bin (Linux amd64).
set -euo pipefail
version=v3.22.0
sha256=1e4ab49e429626cf6c6958d914248b78c9730803c2751b87627e171dc800e7bb
tarball="helm-$version-linux-amd64.tar.gz"
tmp="$(mktemp -d)"
curl -fsSL -o "$tmp/$tarball" "https://get.helm.sh/$tarball"
echo "$sha256  $tmp/$tarball" | sha256sum -c --quiet -
tar -xzf "$tmp/$tarball" -C "$tmp" linux-amd64/helm
sudo install -m 0755 "$tmp/linux-amd64/helm" /usr/local/bin/helm
rm -rf "$tmp"
helm version --short
