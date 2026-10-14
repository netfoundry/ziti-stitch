# Built by goreleaser (dockers_v2), which puts each platform's binary at
# $TARGETPLATFORM/ziti-stitch in the build context. To build by hand:
#   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o linux/amd64/ziti-stitch ./cmd/ziti-stitch
#   docker build --platform linux/amd64 .
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/ziti-stitch /usr/local/bin/ziti-stitch

# Mount a writable volume here: it holds the identities, renewed
# certificates and runtime state. With ZITI_STITCH_CONFIG_SOURCE set (see
# the Helm chart) config.yaml and stitches.yaml come from elsewhere.
ENV ZITI_STITCH_CONFIG_DIR=/config
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ziti-stitch"]
CMD ["run"]
