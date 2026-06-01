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

# === Provider integration tests (issue #47) ===
#
# Validate each supported provider end-to-end by running `stirrup
# harness` with a minimal probe prompt. Credentials are injected at
# recipe runtime from 1Password via `op run --env-file .env.int-test`;
# nothing here runs in CI. Prerequisites on the host:
#   - `jq` installed.
#   - `op` (1Password CLI) installed and authenticated (`op signin`
#     or biometric unlock).
#   - `.env.int-test` populated with op:// references for each
#     credential the targeted recipe needs.
#   - For Bedrock: the IAM user behind AWS_ACCESS_KEY_ID must hold
#     `bedrock:InvokeModelWithResponseStream` for the model in the
#     config's region.
#
# Each probe asks the model to reply with the single word "ok". The
# text-only answer needs no tools, so the default read-only `planning`
# mode is sufficient for the three flag-driven providers; the Bedrock
# config opts into `execution` because it carries an `allow-all`
# permission policy in the file (read-only modes reject allow-all).
#
# Validation reads the run_finished event from the streaming JSONL
# trace and asserts its embedded RunTrace outcome is "success". The
# outcome lives at `.trace.outcome` on the run_finished line (not at
# the top level, and not on every line — the file is multi-line
# JSONL); `select(.kind == "run_finished")` isolates that line so
# `jq -e` exits non-zero on a non-success outcome OR on a run that
# never reached run_finished (crash / SIGKILL leaves no such line).

# Run every provider integration test in sequence; stops on first failure.
int-test: int-test-anthropic int-test-bedrock int-test-openai-chat int-test-openai-responses

int-test-anthropic: build
    op run --env-file .env.int-test -- \
    ./stirrup harness \
        --provider anthropic \
        --api-key-ref secret://ANTHROPIC_API_KEY \
        --model claude-haiku-4-5-20251001 \
        --max-turns 2 \
        --trace /tmp/stirrup-int-test-anthropic.jsonl \
        --prompt "Reply with the single word 'ok' and nothing else."
    jq -e 'select(.kind == "run_finished") | .trace.outcome == "success"' /tmp/stirrup-int-test-anthropic.jsonl

int-test-bedrock: build
    op run --env-file .env.int-test -- \
    ./stirrup harness \
        --config examples/runconfig/int-test-bedrock.json \
        --max-turns 2 \
        --trace /tmp/stirrup-int-test-bedrock.jsonl \
        --prompt "Reply with the single word 'ok' and nothing else."
    jq -e 'select(.kind == "run_finished") | .trace.outcome == "success"' /tmp/stirrup-int-test-bedrock.jsonl

int-test-openai-chat: build
    op run --env-file .env.int-test -- \
    ./stirrup harness \
        --provider openai-compatible \
        --api-key-ref secret://OPENAI_API_KEY \
        --model gpt-4o-mini \
        --max-turns 2 \
        --trace /tmp/stirrup-int-test-openai-chat.jsonl \
        --prompt "Reply with the single word 'ok' and nothing else."
    jq -e 'select(.kind == "run_finished") | .trace.outcome == "success"' /tmp/stirrup-int-test-openai-chat.jsonl

int-test-openai-responses: build
    op run --env-file .env.int-test -- \
    ./stirrup harness \
        --provider openai-responses \
        --api-key-ref secret://OPENAI_API_KEY \
        --model gpt-4o-mini \
        --max-turns 2 \
        --trace /tmp/stirrup-int-test-openai-responses.jsonl \
        --prompt "Reply with the single word 'ok' and nothing else."
    jq -e 'select(.kind == "run_finished") | .trace.outcome == "success"' /tmp/stirrup-int-test-openai-responses.jsonl

# Remove every local worktree whose branch has been merged into origin/main,
# then delete the now-orphaned local branch. Fetches first so the merged
# check reflects the current state of the remote.
worktree-clean:
    #!/usr/bin/env bash
    set -euo pipefail
    git fetch --prune origin
    removed=0
    first=1
    wt="" branch=""
    while IFS= read -r line; do
        if [[ "$line" == worktree\ * ]]; then
            wt="${line#worktree }"
        elif [[ "$line" == branch\ * ]]; then
            branch="${line#branch refs/heads/}"
        elif [[ -z "$line" ]]; then
            if [[ $first -eq 1 ]]; then
                first=0
            elif [[ -n "${wt:-}" && -n "${branch:-}" ]]; then
                if git branch -r --merged origin/main | grep -qF "origin/$branch"; then
                    echo "removing $wt (branch: $branch)"
                    git worktree remove --force "$wt"
                    git branch -d "$branch" 2>/dev/null || true
                    removed=$((removed + 1))
                fi
            fi
            wt="" branch=""
        fi
    done < <(git worktree list --porcelain; echo "")
    echo "removed $removed merged worktree(s)"

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
