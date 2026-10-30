# The build stage runs on the builder's own platform and cross-compiles for
# each target, so a multi-arch build never emulates the Go compiler. The
# final stage only copies files, so it needs no emulation either.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates && mkdir -p /out/data

COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/phila-trashday ./cmd/phila-trashday

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/phila-trashday /phila-trashday
# /data holds the saved home address. It's owned by the user the container
# runs as, so a named volume mounted there starts out writable.
COPY --from=build --chown=65534:65534 /out/data /data

USER 65534:65534
EXPOSE 8080
VOLUME /data
HEALTHCHECK --interval=1m --timeout=10s --start-period=30s CMD ["/phila-trashday", "healthcheck"]
ENTRYPOINT ["/phila-trashday"]
