# ziti-stitch

Stitches services across independent [OpenZiti](https://openziti.io) networks.

An identity on network A dials a service that `ziti-stitch` hosts. For each connection, `ziti-stitch` dials a matching service on network B with a second identity and splices the two together. It works with any number of networks and stitches, needs no admin access on any network, and runs as a system service on Linux, macOS and Windows.

Status: early development. Enrolling, managing stitches, running, and running as a service on Linux, macOS and Windows work.

- [Installing and running as a service](docs/install.md)
- [What to ask your network admins for](docs/admin-checklist.md)

## Quick start

Your network admins create the identities and policies. ziti-stitch needs no admin access: the identity on the ingress network needs Bind on the ingress service, and the one on the egress network needs Dial on the egress service.

```
ziti-stitch enroll --jwt acme.jwt                  # network name derived from the controller host
ziti-stitch enroll --jwt partner.jwt --name partner
ziti-stitch stitch add billing --from acme:billing-api --to partner:billing
ziti-stitch validate                               # checks every Bind and Dial the stitches need
ziti-stitch run
```

`validate` and `network test <name>` tell you which policy is missing, so you can ask for exactly that.

While `run` is up, changes made with the CLI (or by hand) are applied on their own. Stitches that did not change are not touched; removed or changed ones finish their open connections. An invalid change is logged and not applied.

| Command | |
|---|---|
| `enroll --jwt <file\|-> [--name]` | enroll from a one-time token |
| `network add --identity <file> [--name]` | add an identity that is already enrolled |
| `network list \| remove \| rename \| test` | |
| `stitch add \| list \| remove \| enable \| disable` | `stitch add --help` for app data, precedence, timeouts |
| `validate [--offline]` | |
| `run [--log-file]` | SIGHUP forces a reload |
| `web login` | a one-time sign-in link for the web UI |
| `service install \| uninstall \| start \| stop \| restart \| status` | see [docs/install.md](docs/install.md) |

## Web UI

Optional, and off until you turn it on in `config.yaml`:

```yaml
web:
  listen: 127.0.0.1:8474   # loopback only
```

Restart ziti-stitch, then run `ziti-stitch web login` (with `sudo` if the config belongs to root) and open the link it prints. It shows the stitches, networks, open connections and recent events, lets you add, edit, enable, disable and remove stitches, and tests what each network's identity can do. Enrolling stays in the CLI.

There are no passwords: a link works once, for five minutes, and only someone who can write the config directory can make one.

## Config directory

```
networks/acme.json      identity on network "acme", one file per network
stitches.yaml           written by the stitch commands; can be edited by hand
config.yaml             optional: log.level (debug|info|warn|error), log.format (text|json), web.listen
```

```yaml
# stitches.yaml
stitches:
  - name: billing
    from: {network: acme, service: billing-api}    # optional: precedence (default|required|failed), cost
    to: {network: partner, service: billing}        # optional: connect_timeout (default 5s)
    app_data: passthrough                            # or strip, annotate
    idle_timeout: 1h                                 # optional; 0 or absent disables it
```

The stitch commands rewrite `stitches.yaml`; comments in it are not kept.

Without `--config-dir`, `$ZITI_STITCH_CONFIG_DIR` is used, else `/var/lib/ziti-stitch` (Linux, as root), `/Library/Application Support/ziti-stitch` (macOS, as root), `%ProgramData%\ziti-stitch` (Windows), or the user's config directory.

## Build

Requires the Go version in `go.mod`.

```
make build      # bin/ziti-stitch
make check      # gofmt, vet, staticcheck
make test
make build-all  # every release target, CGO off
```

Integration tests run against two local Ziti networks:

```
./deploy/local-e2e.sh up      # ZITI_VERSION=2.0.8 to pin a release
make e2e-test
./deploy/local-e2e.sh down
```

## License

Apache 2.0. See [LICENSE](LICENSE).
