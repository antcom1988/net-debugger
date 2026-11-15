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
    go build -trimpath -ldflags='-s -w' -o /out/net-debugger ./cmd/proxy

# Distroless provides a minimal non-root runtime without a shell or package
# manager. ISO packager XML remains external to the executable because the
# parser intentionally loads it at startup.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app/config

COPY --from=build --chown=nonroot:nonroot /out/net-debugger /app/net-debugger
COPY --chown=nonroot:nonroot packager /app/packager
COPY --chown=nonroot:nonroot config.docker.json /app/config/config.json

# Runtime-writable state: config changes from the dashboard, chaos rules, and logs.
VOLUME ["/app/config", "/app/logs"]

# Defaults from config.docker.json. Publish only the ports enabled in your
# deployed configuration; EXPOSE documents the standard bundled defaults.
EXPOSE 9999 15432 16379 13306 8990 18088

USER nonroot:nonroot
ENTRYPOINT ["/app/net-debugger"]
