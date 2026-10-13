#!/usr/bin/env bash
# Local end-to-end environment for ziti-stitch: two independent Ziti
# quickstart networks (A and B), with the identities, services and policies
# a stitch needs on each, enrolled into $RUN_DIR/{a,b}/*.json.
#
#   ./deploy/local-e2e.sh up      start both networks and configure them, leave running
#   ./deploy/local-e2e.sh env     print the identity files and service names
#   ./deploy/local-e2e.sh down    stop everything and remove the run directory
#
# Needs: ziti 1.6.7+ (2.0+ for the caller-identity tests), jq, curl. ZITI_BIN=/path/to/ziti picks a binary;
# ZITI_VERSION=2.0.8 downloads that release into .cache/ instead.
# Ports: A_CTRL_PORT=12910 A_ROUTER_PORT=30910 B_CTRL_PORT=12920 B_ROUTER_PORT=30920
#
# Network A (where clients are):          Network B (where the real service is):
#   stitch-a  #stitch-hosts   binds echo    stitch-b  #stitch-dialers  dials echo, renew
#   stitch-a2 (a second instance, ditto)    stitch-b2 (ditto)
#   client-a  #stitch-clients dials echo    host-b    #echo-hosts      binds echo
#                                           renew-b   #renew-hosts     binds renew
#                                             (only the e2e renewal test uses renew-b: renewing
#                                              it invalidates the enrolled file's certificate)
set -euo pipefail

cd "$(dirname "$0")/.."
RUN_DIR="${RUN_DIR:-$PWD/.local-e2e}"
A_CTRL_PORT="${A_CTRL_PORT:-12910}"
A_ROUTER_PORT="${A_ROUTER_PORT:-30910}"
B_CTRL_PORT="${B_CTRL_PORT:-12920}"
B_ROUTER_PORT="${B_ROUTER_PORT:-30920}"

log() { printf '\n== %s\n' "$*"; }
die() { echo "error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || die "$1 not found in PATH"; }
port_free() { ! lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }
start_bg() { local name=$1; shift; nohup "$@" >"$RUN_DIR/$name.log" 2>&1 & echo $! >"$RUN_DIR/$name.pid"; }
wait_http() { local i; for ((i = 0; i < $2; i++)); do curl -sk -o /dev/null "$1" && return 0; sleep 1; done; return 1; }

# resolve_ziti sets ZITI: ZITI_BIN if given, else a downloaded ZITI_VERSION,
# else whatever is on PATH.
resolve_ziti() {
  if [[ -n "${ZITI_BIN:-}" ]]; then ZITI="$ZITI_BIN"; return; fi
  if [[ -z "${ZITI_VERSION:-}" ]]; then ZITI=ziti; return; fi
  local os arch dir
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
  dir="$PWD/.cache/ziti/$ZITI_VERSION"
  if [[ ! -x "$dir/ziti" ]]; then
    log "downloading ziti $ZITI_VERSION ($os/$arch)"
    mkdir -p "$dir"
    curl -fsSL "https://github.com/openziti/ziti/releases/download/v$ZITI_VERSION/ziti-$os-$arch-$ZITI_VERSION.tar.gz" |
      tar -xz -C "$dir" ziti || die "download failed"
  fi
  ZITI="$dir/ziti"
}

# zcli runs the ziti CLI for network $1 (a or b) with a home directory of its
# own: the CLI keeps logins and trusted CAs there, older versions keyed by
# host name alone, so two networks on localhost would overwrite each other's
# CA. It also keeps the harness out of the user's own ~/.ziti.
zcli() { local net=$1; shift; HOME="$RUN_DIR/$net/home" "$ZITI" "$@"; }

# zt runs an edge command against network $1.
zt() { local net=$1; shift; zcli "$net" edge -i "stitch-e2e-$net" "$@"; }

# start_network starts a quickstart for network $1 and logs the CLI in.
start_network() {
  local net=$1 ctrl=$2 router=$3 i
  log "starting network $net ($("$ZITI" version)) on $ctrl/$router"
  mkdir -p "$RUN_DIR/$net/home"
  start_bg "quickstart-$net" env HOME="$RUN_DIR/$net/home" "$ZITI" edge quickstart --home "$RUN_DIR/$net/ziti" \
    --ctrl-address localhost --ctrl-port "$ctrl" --router-address localhost --router-port "$router" \
    -u admin -p admin
  wait_http "https://localhost:$ctrl/edge/client/v1/version" 60 || die "controller $net did not start; see $RUN_DIR/quickstart-$net.log"
  for ((i = 0; i < 30; i++)); do
    zcli "$net" edge login "localhost:$ctrl" -u admin -p admin -y -i "stitch-e2e-$net" >/dev/null 2>&1 && break
    sleep 2
  done
  [[ $i -lt 30 ]] || die "could not log in to controller $net"
  for ((i = 0; i < 30; i++)); do
    [[ "$(zt "$net" list edge-routers -j 2>/dev/null | jq -r '.data | length')" == 1 ]] && break
    sleep 2
  done
  [[ $i -lt 30 ]] || die "router $net did not register"
  # Quickstart may or may not create default router policies; ours are
  # named so they never collide with its own.
  zt "$net" create edge-router-policy e2e-all --edge-router-roles '#all' --identity-roles '#all' >/dev/null
  zt "$net" create service-edge-router-policy e2e-all --edge-router-roles '#all' --service-roles '#all' >/dev/null
}

# create_identity creates and enrolls identity $2 on network $1 with role attribute $3.
create_identity() {
  local net=$1 name=$2 attr=$3
  zt "$net" create identity "$name" -a "$attr" -o "$RUN_DIR/$net/$name.jwt" >/dev/null
  zcli "$net" edge enroll "$RUN_DIR/$net/$name.jwt" -o "$RUN_DIR/$net/$name.json" >/dev/null 2>&1 ||
    die "enrolling $name on $net failed"
  rm -f "$RUN_DIR/$net/$name.jwt"
}

up() {
  need jq; need curl
  resolve_ziti
  command -v "$ZITI" >/dev/null || die "ziti not found at $ZITI"
  for p in "$A_CTRL_PORT" "$A_ROUTER_PORT" "$B_CTRL_PORT" "$B_ROUTER_PORT"; do
    port_free "$p" || die "port $p is in use (another run? try: $0 down)"
  done
  # A reboot kills the processes but leaves the run directory, and the
  # second `up` then fails on names that already exist with no hint why.
  [[ -d "$RUN_DIR/a" || -d "$RUN_DIR/b" ]] && die "$RUN_DIR holds state from an earlier run. Run: $0 down"
  mkdir -p "$RUN_DIR/a" "$RUN_DIR/b"
  "$ZITI" version >"$RUN_DIR/ziti-version" # the e2e tests skip what older versions lack

  start_network a "$A_CTRL_PORT" "$A_ROUTER_PORT"
  start_network b "$B_CTRL_PORT" "$B_ROUTER_PORT"

  log "configuring network A: stitch-a binds echo, client-a dials it"
  zt a create service echo -a stitched >/dev/null
  zt a create service e2e -a stitched >/dev/null # for go test -tags e2e only
  zt a create service stitch-admin -a stitched >/dev/null # the web UI over Ziti, ditto
  zt a create service ha -a stitched >/dev/null # two instances, ditto
  zt a create service-policy stitch-hosts-bind Bind --identity-roles '#stitch-hosts' --service-roles '#stitched' >/dev/null
  zt a create service-policy stitch-clients-dial Dial --identity-roles '#stitch-clients' --service-roles '#stitched' >/dev/null
  create_identity a stitch-a stitch-hosts
  create_identity a stitch-a2 stitch-hosts # a second instance
  create_identity a client-a stitch-clients

  log "configuring network B: host-b binds echo, stitch-b dials it"
  zt b create service echo -a echo >/dev/null
  zt b create service e2e -a echo >/dev/null # for go test -tags e2e only
  zt b create service ha -a echo >/dev/null # ditto
  zt b create service-policy echo-hosts-bind Bind --identity-roles '#echo-hosts' --service-roles '#echo' >/dev/null
  zt b create service-policy stitch-dialers-dial Dial --identity-roles '#stitch-dialers' --service-roles '#echo,#renew' >/dev/null
  zt b create service renew -a renew >/dev/null
  zt b create service-policy renew-hosts-bind Bind --identity-roles '#renew-hosts' --service-roles '#renew' >/dev/null
  create_identity b stitch-b stitch-dialers
  create_identity b stitch-b2 stitch-dialers # a second instance
  create_identity b host-b echo-hosts
  create_identity b renew-b renew-hosts

  env_info
}

env_info() {
  cat <<EOF

Networks are running.
  A: https://localhost:$A_CTRL_PORT   CLI: HOME=$RUN_DIR/a/home $ZITI edge -i stitch-e2e-a ...
  B: https://localhost:$B_CTRL_PORT   CLI: HOME=$RUN_DIR/b/home $ZITI edge -i stitch-e2e-b ...

Identities:
  $RUN_DIR/a/stitch-a.json   binds  A:echo
  $RUN_DIR/a/stitch-a2.json  binds  A:echo  (a second instance)
  $RUN_DIR/a/client-a.json   dials  A:echo
  $RUN_DIR/b/stitch-b.json   dials  B:echo
  $RUN_DIR/b/stitch-b2.json  dials  B:echo  (a second instance)
  $RUN_DIR/b/host-b.json     binds  B:echo
  $RUN_DIR/b/renew-b.json    binds  B:renew  (go test -tags e2e ./... renews it)

Logs: $RUN_DIR/*.log      Stop: $0 down
EOF
}

down() {
  local f
  for f in "$RUN_DIR"/*.pid; do [[ -e "$f" ]] && kill "$(cat "$f")" 2>/dev/null || true; done
  sleep 1
  rm -rf "$RUN_DIR"
  echo "stopped; run directory removed"
}

case "${1:-}" in
  up) up ;;
  env) env_info ;;
  down) down ;;
  *) sed -n '2,13p' "$0"; exit 1 ;;
esac
