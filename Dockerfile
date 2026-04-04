# syntax=docker/dockerfile:1.7

FROM golang:1.26.1-alpine AS builder

ARG VERSION=dev

WORKDIR /src

COPY go.work ./
COPY types ./types
COPY gen ./gen
COPY harness ./harness

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/rxbynerd/stirrup/harness/internal/version.version=${VERSION}" \
    -o /out/stirrup ./harness/cmd/stirrup

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/stirrup /usr/local/bin/stirrup

ENTRYPOINT ["/usr/local/bin/stirrup"]
