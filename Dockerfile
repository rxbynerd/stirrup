# syntax=docker/dockerfile:1.7

FROM golang:1.26.1-alpine AS builder

WORKDIR /src

COPY go.work ./
COPY types ./types
COPY gen ./gen
COPY harness ./harness

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness ./harness/cmd/harness && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/job ./harness/cmd/job

FROM gcr.io/distroless/static-debian12:nonroot AS harness

COPY --from=builder /out/harness /usr/local/bin/harness

ENTRYPOINT ["/usr/local/bin/harness"]

FROM gcr.io/distroless/static-debian12:nonroot AS job

COPY --from=builder /out/job /usr/local/bin/job

ENTRYPOINT ["/usr/local/bin/job"]
