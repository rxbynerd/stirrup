# syntax=docker/dockerfile:1.7

FROM golang:1.26.1-alpine AS builder

WORKDIR /src

COPY types ./types
COPY harness ./harness

WORKDIR /src/harness

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness ./cmd/harness

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/harness /usr/local/bin/harness

ENTRYPOINT ["/usr/local/bin/harness"]
