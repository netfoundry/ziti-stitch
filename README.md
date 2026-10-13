# ziti-stitch

Stitches services across independent [OpenZiti](https://openziti.io) networks.

An identity on network A dials a service that `ziti-stitch` hosts. For each connection, `ziti-stitch` dials a matching service on network B with a second identity and splices the two together. It works with any number of networks and stitches, needs no admin access on any network, and runs as a system service on Linux, macOS and Windows.

Status: early development. `run` works against a hand-written config directory; the CLI for enrolling and managing stitches is next.

## Configure and run

A config directory holds one enrolled identity per network and a list of stitches:

```
networks/acme.json      identity on network "acme" (binds the ingress service)
networks/partner.json   identity on network "partner" (dials the egress service)
stitches.yaml
config.yaml             optional: log.level (debug|info|warn|error), log.format (text|json)
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

```
ziti-stitch run --config-dir /path/to/dir
```

Without `--config-dir`, `$ZITI_STITCH_CONFIG_DIR` is used, else `/var/lib/ziti-stitch` (Linux, as root), `/Library/Application Support/ziti-stitch` (macOS, as root), `%ProgramData%\ziti-stitch` (Windows), or the user's config directory.

The identity on the ingress network needs Bind on the ingress service; the one on the egress network needs Dial on the egress service. Neither needs to be an admin.

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
