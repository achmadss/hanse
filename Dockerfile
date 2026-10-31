# Build the Gateway as a small, static image and run it as a non-root user.
# See docs/guides/docker.md for the volumes and the configuration layout.
#
# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build

WORKDIR /src

# Download the modules first, so a source-only change does not repeat the
# download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 gives a static binary: the SQLite driver is pure Go.
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags="-s -w -X github.com/achmadss/hanse/internal/version.Version=docker" \
        -o /out/hanse-gateway \
        ./cmd/hanse-gateway

FROM alpine:3.22

# ca-certificates lets http handlers and the Gateway's own outbound TLS calls
# verify a public certificate.
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 hanse \
    && adduser -S -u 10001 -G hanse -H -h /var/lib/hanse hanse \
    && install -d -o hanse -g hanse -m 0750 /etc/hanse \
    && install -d -o hanse -g hanse -m 0700 /var/lib/hanse

COPY --from=build /out/hanse-gateway /usr/local/bin/hanse-gateway

# The image runs as an unprivileged user; it never needs root, and the
# Gateway listens on an unprivileged port (8443 by default).
USER 10001:10001

EXPOSE 8443

VOLUME ["/etc/hanse", "/var/lib/hanse"]

ENTRYPOINT ["/usr/local/bin/hanse-gateway"]
CMD ["--config", "/etc/hanse/gateway.yaml"]
