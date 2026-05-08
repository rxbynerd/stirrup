default: build test

build:
    go build -o stirrup ./harness/cmd/stirrup
    go build -o stirrup-eval ./eval/cmd/eval

test:
    go test ./harness/... ./types/... ./eval/...

lint:
    golangci-lint run ./harness/... ./types/... ./eval/...

proto:
    buf generate

buf-lint:
    buf lint

docker:
    docker build -t stirrup .

# Smoke test: confirm Granite Guardian (issue #43) is reachable on the
# OpenAI-compatible endpoint at 127.0.0.1:1234 with a granite-guardian
# model loaded. Used during #43 implementation to verify a healthy local
# server before exercising the adapter.
guardian-smoke:
    #!/usr/bin/env bash
    set -euo pipefail
    endpoint="http://127.0.0.1:1234"
    echo "checking Granite Guardian at ${endpoint}..."
    if ! response=$(curl -fsS --max-time 5 "${endpoint}/v1/models" 2>&1); then
        echo "FAIL: cannot reach ${endpoint}/v1/models" >&2
        echo "  ${response}" >&2
        exit 1
    fi
    if ! printf '%s' "${response}" | grep -qi 'granite-guardian'; then
        echo "FAIL: ${endpoint} responded but no granite-guardian model is listed" >&2
        echo "  /v1/models payload: ${response}" >&2
        exit 1
    fi
    echo "ok: granite-guardian available at ${endpoint}"

clean:
    rm -f stirrup stirrup-eval

# === Kind sandbox ===

# Bring up the local K8sExecutor sandbox cluster (kind + gVisor +
# RuntimeClasses). See scripts/dev/README.md for prerequisites and
# the rationale behind the bring-up steps.
kind-up:
    ./scripts/dev/kind-up.sh

# Tear the sandbox cluster down. Idempotent — safe to run when no
# cluster is present.
kind-down:
    ./scripts/dev/kind-down.sh

# Smoke test: schedule Pods on RuntimeClass=gvisor and RuntimeClass=runc
# against the sandbox cluster and verify gVisor signatures in the
# kernel banner of the gvisor pod.
kind-smoke:
    ./scripts/dev/smoke-test.sh
