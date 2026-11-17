# syntax=docker/dockerfile:1.7

# Compile a small, static Linux binary. The module has no external Go modules,
# so copying go.mod first keeps the dependency layer cacheable if dependencies
# are introduced later.
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o /out/net-debugger ./cmd/proxy && \
    mkdir -p /out/logs

# Alpine retains a minimal runtime while providing /bin/sh, so operators can
# inspect the container with kubectl exec when troubleshooting.
FROM alpine:3.21 AS runtime
RUN apk add --no-cache ca-certificates && \
    addgroup -S netdebugger && \
    adduser -S -G netdebugger netdebugger && \
    mkdir -p /app/config /app/logs && \
    chown -R netdebugger:netdebugger /app
WORKDIR /app/config

COPY --from=build --chown=netdebugger:netdebugger /out/net-debugger /app/net-debugger
COPY --chown=netdebugger:netdebugger packager /app/packager
COPY --chown=netdebugger:netdebugger config.docker.json /app/config/config.json
COPY --from=build --chown=netdebugger:netdebugger /out/logs /app/logs

# Runtime-writable state: configuration changes from the dashboard and chaos rules.
VOLUME ["/app/config"]

# The dashboard listens on 9999. Proxy connections may be added dynamically
# through the dashboard, so reserve the supported listener range as well.
# EXPOSE is documentation only; publish it with -p 9999:9999 -p 10000-30000:10000-30000.
EXPOSE 9999 10000-30000

USER netdebugger:netdebugger
ENTRYPOINT ["/app/net-debugger"]
