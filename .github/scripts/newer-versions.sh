#!/usr/bin/env bash
# Prints the versions the weekly "newer versions" workflow tests, as
# key=value lines for $GITHUB_OUTPUT:
#   ziti  JSON list: the newest OpenZiti release, the newest pre-release if
#         it is ahead of that, and the oldest version we support
#   zet   the newest ziti-edge-tunnel release
# Needs gh, with GH_TOKEN set in Actions.
set -euo pipefail

# The oldest OpenZiti we support (README, "Requirements").
floor=1.6.7

latest="$(gh release view --repo openziti/ziti --json tagName -q .tagName)"
latest="${latest#v}"
pre="$(gh release list --repo openziti/ziti --limit 50 --exclude-drafts --json tagName,isPrerelease \
  -q '.[] | select(.isPrerelease) | .tagName' | sed 's/^v//' | sort -V | tail -n 1)"

versions=("$latest")
# Compare without any -suffix: sort -V puts 2.1.0 before 2.1.0-pre2.
if [[ -n "$pre" && "${pre%%-*}" != "$latest" &&
  "$(printf '%s\n' "$latest" "${pre%%-*}" | sort -V | tail -n 1)" == "${pre%%-*}" ]]; then
  versions+=("$pre")
fi
versions+=("$floor")

zet="$(gh release view --repo openziti/ziti-tunnel-sdk-c --json tagName -q .tagName)"

printf 'ziti=%s\n' "$(printf '%s\n' "${versions[@]}" | jq -Rnc '[inputs]')"
printf 'zet=%s\n' "${zet#v}"
