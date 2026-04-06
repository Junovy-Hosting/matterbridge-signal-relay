# Thin relay bridging signal-cli-rest-api ↔ matterbridge API gateway.
# See https://github.com/Junovy-Hosting/matterbridge-signal-relay
FROM golang:1.22-bookworm AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /signal-relay .

FROM debian:bookworm-slim

RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends ca-certificates tini; \
  apt-get clean; \
  rm -rf /var/lib/apt/lists/*

COPY --from=builder /signal-relay /usr/local/bin/signal-relay

RUN groupadd --system --gid 1001 relay \
  && useradd --system --uid 1001 --gid relay --shell /usr/sbin/nologin relay

USER relay

EXPOSE 8081

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/signal-relay"]
