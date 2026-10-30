FROM golang:1.27-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates && mkdir -p /out/data

COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/phila-trashday ./cmd/phila-trashday

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
