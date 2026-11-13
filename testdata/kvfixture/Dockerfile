# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .

ARG KV_VARIANT=buggy
RUN set -eu; \
    case "$KV_VARIANT" in \
      buggy)   TAGS="" ;; \
      kvfixed) TAGS="kvfixed" ;; \
      *) echo "KV_VARIANT must be 'buggy' or 'kvfixed', got '$KV_VARIANT'" >&2; exit 1 ;; \
    esac; \
    CGO_ENABLED=0 go build -tags "$TAGS" -trimpath -ldflags="-s -w" -o /out/kv ./cmd/kv; \
    CGO_ENABLED=0 go build              -trimpath -ldflags="-s -w" -o /out/loadgen ./cmd/loadgen; \
    /out/kv version

FROM alpine:3.20 AS kv
COPY --from=build /out/kv /kv
ENV KV_TICK_MS=25 \
    KV_HEARTBEAT_MS=100 \
    KV_ELECTION_MIN_MS=600 \
    KV_ELECTION_MAX_MS=900 \
    KV_RPC_TIMEOUT_MS=250 \
    KV_APPLY_WAIT_MS=2000 \
    KV_CLIENT_ADDR=0.0.0.0:8080 \
    KV_PEER_ADDR=0.0.0.0:9090 \
    KV_DATA_DIR=/var/lib/kv \
    KV_ROLE_HINT=replica
EXPOSE 8080 9090
ENTRYPOINT ["/kv", "serve"]

FROM alpine:3.20 AS tools
COPY --from=build /out/loadgen /loadgen
ENTRYPOINT ["/loadgen"]
