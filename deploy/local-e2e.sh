#!/usr/bin/env bash
# Local end-to-end environment for ziti-stitch: two independent Ziti
# quickstart networks (A and B), with the identities, services and policies
# a stitch needs on each, enrolled into $RUN_DIR/{a,b}/*.json.
#
#   ./deploy/local-e2e.sh up         start both networks and configure them, leave running
#   ./deploy/local-e2e.sh env        print the identity files and service names
#   ./deploy/local-e2e.sh tunnelers  after up: real ziti-edge-tunnels on both sides of a
#                                    stitch (Linux, needs sudo for the client's TUN)
#   ./deploy/local-e2e.sh down       stop everything and remove the run directory
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
    local base="https://github.com/openziti/ziti/releases/download/v$ZITI_VERSION"
    local tarball="ziti-$os-$arch-$ZITI_VERSION.tar.gz" tmp
    tmp="$(mktemp -d)"
    curl -fsSL -o "$tmp/$tarball" "$base/$tarball" || die "download failed"
    curl -fsSL -o "$tmp/checksums" "$base/checksums.sha256.txt" || die "checksum download failed"
    grep -q " $tarball\$" "$tmp/checksums" || die "no checksum for $tarball in the release"
    (cd "$tmp" && grep " $tarball\$" checksums | sha256_check) || die "checksum mismatch for $tarball"
    mkdir -p "$dir"
    tar -xzf "$tmp/$tarball" -C "$dir" ziti || die "unpacking $tarball failed"
    rm -rf "$tmp"
  fi
  ZITI="$dir/ziti"
}

# sha256_check checks "<sum>  <file>" lines on stdin.
sha256_check() {
  if command -v sha256sum >/dev/null; then sha256sum -c --quiet -; else shasum -a 256 -c --quiet -; fi
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
  echo "$ZITI" >"$RUN_DIR/ziti-bin"        # for later commands

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

# ziti-edge-tunnel for the tunnelers test. Its releases carry no checksums,
# so the expected ones are pinned here.
ZET_VERSION=1.18.7.2
zet_sha256() {
  case "$1" in
    x86_64) echo 40840bf4739a8303b5cbeb63e84bc16017b0b98f2ce816bc5af629e3df56adb9 ;;
    arm64) echo 2b8c377ec457d0a8e97fd9ad6881e79e0325ed16a2143bd58a3039b591b91dc8 ;;
  esac
}

# resolve_zet sets ZET to a verified ziti-edge-tunnel, downloading it once.
resolve_zet() {
  local arch dir
  case "$(uname -m)" in
    x86_64 | amd64) arch=x86_64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
  dir="$PWD/.cache/ziti-edge-tunnel/$ZET_VERSION"
  if [[ ! -x "$dir/ziti-edge-tunnel" ]]; then
    log "downloading ziti-edge-tunnel $ZET_VERSION ($arch)"
    local tmp zip="ziti-edge-tunnel-Linux_$arch.zip"
    tmp="$(mktemp -d)"
    curl -fsSL -o "$tmp/$zip" "https://github.com/openziti/ziti-tunnel-sdk-c/releases/download/v$ZET_VERSION/$zip" ||
      die "download failed"
    (cd "$tmp" && echo "$(zet_sha256 "$arch")  $zip" | sha256_check) || die "checksum mismatch for $zip"
    mkdir -p "$dir"
    unzip -q -o "$tmp/$zip" ziti-edge-tunnel -d "$dir" || die "unpacking $zip failed"
    chmod +x "$dir/ziti-edge-tunnel"
    rm -rf "$tmp"
  fi
  ZET="$dir/ziti-edge-tunnel"
}

# tunnelers runs a client and a host ziti-edge-tunnel around a stitch:
#   curl 192.0.2.10:7000 -> client tunneler (intercepts it on A) -> network A
#   -> ziti-stitch -> network B -> hosting tunneler -> 127.0.0.1:7000
# The hosting side forwards by the port in the client's app data
# (forwardPort), so a request only arrives if the stitch passed the app data
# on unchanged. It runs again with app_data annotate, to check that the C
# SDK accepts the extra ziti_stitch key. A second stitch does the same for
# UDP (192.0.2.11:7001): tunnelers carry one datagram per Ziti message, so
# every datagram must come back whole and on its own.
tunnelers() {
  [[ "$(uname -s)" == Linux ]] || die "the tunnelers test needs Linux, for ziti-edge-tunnel"
  [[ -s "$RUN_DIR/ziti-bin" ]] || die "start the networks first: $0 up"
  need go; need python3; need unzip
  sudo -n true 2>/dev/null || die "the client tunneler needs passwordless sudo, for its TUN device"
  ZITI="$(cat "$RUN_DIR/ziti-bin")"
  resolve_zet
  local ip=192.0.2.10 port=7000 marker="stitched by ziti-stitch $$"
  local udp_ip=192.0.2.11 udp_port=7001
  local www="$RUN_DIR/www" cfg="$RUN_DIR/tunnel-stitch" zs="$RUN_DIR/ziti-stitch"

  log "configuring the tunneled service on both networks"
  zt a create config tunneled-intercept intercept.v1 \
    "{\"protocols\":[\"tcp\"],\"addresses\":[\"$ip\"],\"portRanges\":[{\"low\":$port,\"high\":$port}]}" >/dev/null
  zt a create service tunneled -a stitched --configs tunneled-intercept >/dev/null
  zt b create config tunneled-host host.v1 \
    "{\"protocol\":\"tcp\",\"address\":\"127.0.0.1\",\"forwardPort\":true,\"allowedPortRanges\":[{\"low\":$port,\"high\":$port}]}" >/dev/null
  zt b create service tunneled -a echo --configs tunneled-host >/dev/null
  zt a create config tunneled-udp-intercept intercept.v1 \
    "{\"protocols\":[\"udp\"],\"addresses\":[\"$udp_ip\"],\"portRanges\":[{\"low\":$udp_port,\"high\":$udp_port}]}" >/dev/null
  zt a create service tunneled-udp -a stitched --configs tunneled-udp-intercept >/dev/null
  zt b create config tunneled-udp-host host.v1 \
    "{\"protocol\":\"udp\",\"address\":\"127.0.0.1\",\"forwardPort\":true,\"allowedPortRanges\":[{\"low\":$udp_port,\"high\":$udp_port}]}" >/dev/null
  zt b create service tunneled-udp -a echo --configs tunneled-udp-host >/dev/null
  create_identity a tunnel-client-a stitch-clients
  create_identity b tunnel-host-b echo-hosts

  log "starting a web server, the hosting tunneler and ziti-stitch"
  mkdir -p "$www" && echo "$marker" >"$www/marker"
  start_bg tunnel-www python3 -m http.server "$port" --bind 127.0.0.1 --directory "$www"
  start_bg tunnel-udp-echo python3 -c "$UDP_ECHO" "$udp_port"
  start_bg tunnel-host "$ZET" run-host -i "$RUN_DIR/b/tunnel-host-b.json"
  go build -o "$zs" ./cmd/ziti-stitch
  "$zs" --config-dir "$cfg" network add --identity "$RUN_DIR/a/stitch-a.json" --name a >/dev/null
  "$zs" --config-dir "$cfg" network add --identity "$RUN_DIR/b/stitch-b.json" --name b >/dev/null
  "$zs" --config-dir "$cfg" stitch add tunneled --from a:tunneled --to b:tunneled >/dev/null
  "$zs" --config-dir "$cfg" stitch add tunneled-udp --from a:tunneled-udp --to b:tunneled-udp >/dev/null
  start_bg tunnel-stitch "$zs" --config-dir "$cfg" run

  log "starting the client tunneler"
  # Root owns what it writes, so it gets its own copy of the identity.
  mkdir -p "$RUN_DIR/tunnel-client" && cp "$RUN_DIR/a/tunnel-client-a.json" "$RUN_DIR/tunnel-client/"
  start_bg tunnel-client sudo -n "$ZET" run -i "$RUN_DIR/tunnel-client/tunnel-client-a.json"

  fetch_through_tunnelers "$ip" "$port" "$marker" || die "no answer through the tunnelers (passthrough); see $RUN_DIR/tunnel-*.log"
  echo "passthrough: ok"
  python3 -c "$UDP_CHECK" "$udp_ip" "$udp_port" || die "UDP through the tunnelers failed; see $RUN_DIR/tunnel-*.log"

  log "switching the stitch to app_data annotate"
  "$zs" --config-dir "$cfg" stitch remove tunneled >/dev/null
  "$zs" --config-dir "$cfg" stitch add tunneled --from a:tunneled --to b:tunneled --app-data annotate >/dev/null
  sleep 5 # the running ziti-stitch picks the change up and rebinds
  fetch_through_tunnelers "$ip" "$port" "$marker" || die "no answer through the tunnelers (annotate); see $RUN_DIR/tunnel-*.log"
  echo "annotate: ok"
  log "tunnelers test passed"
}

# UDP_ECHO is a UDP echo server on 127.0.0.1:<port>.
UDP_ECHO='
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("127.0.0.1", int(sys.argv[1])))
while True:
    data, addr = s.recvfrom(65535)
    s.sendto(data, addr)
'

# UDP_CHECK sends datagrams of several sizes to <ip> <port>, one at a time
# and then in a burst, and checks each comes back whole and on its own.
# Sizes stop at 1472, a whole 1500-byte packet: ziti-edge-tunnel (1.18)
# forwards each IP fragment of a larger datagram as a datagram of its own,
# with or without a stitch.
UDP_CHECK='
import os, socket, sys, time
addr = (sys.argv[1], int(sys.argv[2]))
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(3)
deadline = time.time() + 60
while True:  # the first datagram may race the intercept being set up
    try:
        s.sendto(b"hello", addr)
        if s.recvfrom(65535)[0] == b"hello":
            break
    except OSError:
        pass
    if time.time() > deadline:
        sys.exit("no UDP answer through the tunnelers")
for size in (1, 100, 1400, 1472):
    sent = os.urandom(size)
    s.sendto(sent, addr)
    got = s.recvfrom(65535)[0]
    if got != sent:
        sys.exit(f"a {size}-byte datagram came back as {len(got)} bytes")
sizes = [37 * (i + 1) for i in range(20)]
burst = [os.urandom(n) for n in sizes]
for d in burst:
    s.sendto(d, addr)
got = []
try:
    while len(got) < len(burst):
        got.append(s.recvfrom(65535)[0])
except socket.timeout:
    pass
if sorted(got) != sorted(burst):
    sys.exit(f"a burst of {len(burst)} datagrams came back as {len(got)} with sizes {sorted(map(len, got))}")
print("udp: ok (single datagrams up to 1472 bytes, and a burst of 20)")
'

# fetch_through_tunnelers waits up to a minute for the marker to come back.
fetch_through_tunnelers() {
  local i body
  for ((i = 0; i < 60; i++)); do
    body="$(curl -sf --max-time 5 "http://$1:$2/marker" 2>/dev/null)" && [[ "$body" == "$3" ]] && return 0
    sleep 1
  done
  return 1
}

down() {
  local f pid
  for f in "$RUN_DIR"/*.pid; do
    [[ -e "$f" ]] || continue
    pid="$(cat "$f")"
    # The client tunneler runs as root, under sudo.
    kill "$pid" 2>/dev/null || sudo -n kill "$pid" 2>/dev/null || true
  done
  sleep 1
  rm -rf "$RUN_DIR" 2>/dev/null || sudo -n rm -rf "$RUN_DIR"
  echo "stopped; run directory removed"
}

case "${1:-}" in
  up) up ;;
  env) env_info ;;
  tunnelers) tunnelers ;;
  down) down ;;
  *) sed -n '2,15p' "$0"; exit 1 ;;
esac
