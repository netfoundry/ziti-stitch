VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"
STATICCHECK ?= honnef.co/go/tools/cmd/staticcheck@2026.2.2
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
GOFILES = $(shell find . -name '*.go' -not -path './.git/*')

export CGO_ENABLED := 0

.PHONY: build build-all test check fmt fmt-check vet lint clean

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

vet:
	go vet ./...

# staticcheck has to be built against this module's Go version; an older
# binary panics with `invalid Go version` instead of reporting findings.
# `go run pkg@version` compiles it with the toolchain in go.mod and leaves
# this module's own dependencies alone.
lint:
	go run $(STATICCHECK) ./...

check: fmt-check vet lint

test:
	go test ./...

clean:
	rm -rf bin dist
