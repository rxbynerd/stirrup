# syntax=docker/dockerfile:1.7

FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS builder

ARG VERSION=dev
ARG COMMIT=""

WORKDIR /src

COPY go.work ./
COPY types ./types
COPY gen ./gen
COPY eval ./eval
COPY harness ./harness
COPY examples/controlplane/go.mod ./examples/controlplane/go.mod

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w \
      -X github.com/rxbynerd/stirrup/types/version.version=${VERSION} \
      -X github.com/rxbynerd/stirrup/types/version.commit=${COMMIT}" \
    -o /out/stirrup ./harness/cmd/stirrup

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=builder /out/stirrup /usr/local/bin/stirrup

ENTRYPOINT ["/usr/local/bin/stirrup"]
