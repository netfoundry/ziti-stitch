# ziti-stitch

Stitches services across independent [OpenZiti](https://openziti.io) networks.

An identity on network A dials a service that `ziti-stitch` hosts. For each connection, `ziti-stitch` dials a matching service on network B with a second identity and splices the two together. It works with any number of networks and stitches, needs no admin access on any network, and runs as a system service on Linux, macOS and Windows.

Status: early development. Nothing here is usable yet.

## Build

Requires the Go version in `go.mod`.

```
make build      # bin/ziti-stitch
make check      # gofmt, vet, staticcheck
make test
make build-all  # every release target, CGO off
```

## License

Apache 2.0. See [LICENSE](LICENSE).
