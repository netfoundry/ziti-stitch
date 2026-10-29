VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# -trimpath keeps the build machine's paths out of the binary.
LDFLAGS := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"
STATICCHECK ?= honnef.co/go/tools/cmd/staticcheck@2026.2.2
GOVULNCHECK ?= golang.org/x/vuln/cmd/govulncheck@v1.9.0
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
GOFILES = $(shell find . -name '*.go' -not -path './.git/*')

export CGO_ENABLED := 0

.PHONY: build build-all test e2e-test check fmt fmt-check vet lint vuln clean

build:
	go build $(LDFLAGS) -o bin/ziti-stitch ./cmd/ziti-stitch

# Cross-compiles every release target. CI runs it so that a dependency
# needing CGO, or one that does not build on some OS, fails here rather
# than at release time.
build-all:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "$$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build $(LDFLAGS) -o bin/ziti-stitch-$$os-$$arch$$ext ./cmd/ziti-stitch || exit 1; \
	done

fmt:
	gofmt -w $(GOFILES)

fmt-check:
	@unformatted=$$(gofmt -l $(GOFILES)); \
	if [ -n "$$unformatted" ]; then echo "Unformatted Go files (run 'make fmt'):"; echo "$$unformatted"; exit 1; fi

# The e2e-tagged tests only build with the tag, so vet and lint both ways.
vet:
	go vet ./...
	go vet -tags e2e ./...

# staticcheck has to be built against this module's Go version; an older
# binary panics with `invalid Go version` instead of reporting findings.
# `go run pkg@version` compiles it with the toolchain in go.mod and leaves
# this module's own dependencies alone.
lint:
	go run $(STATICCHECK) ./...
	go run $(STATICCHECK) -tags e2e ./...

check: fmt-check vet lint

# Fails only on vulnerabilities our code can reach, in the toolchain go.mod
# names or in a dependency.
vuln:
	go run $(GOVULNCHECK) ./...

test:
	go test ./...

# Integration tests against the local networks: ./deploy/local-e2e.sh up first.
e2e-test:
	ZITI_STITCH_E2E_DIR=$(CURDIR)/.local-e2e go test -tags e2e -count=1 ./...

clean:
	rm -rf bin dist
