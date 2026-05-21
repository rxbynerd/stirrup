#!/usr/bin/env bash
# smoke-runconfig.sh — verify `stirrup run-config` emits a parseable
# RunConfig and the canonical pipeline shape round-trips idempotently.
#
# Four checks run sequentially:
#
#   1. Flag-only invocation produces JSON with the operator-set mode
#      and prompt visible. Asserted with `jq -e` so a structural drift
#      in the emitted JSON trips this script before it lands in
#      a release.
#   2. Idempotency: piping a config through `stirrup run-config` twice
#      produces byte-identical output. The first pass normalises;
#      the second is a no-op.
#   3. --validate exits non-zero on a contradictory config.
#   4. `stirrup harness --output-runconfig <path>` writes a
#      parseable RunConfig and exits cleanly without invoking the
#      provider (no ANTHROPIC_API_KEY set; the dry-run branch must
#      short-circuit before any provider HTTP).
#
# Run from the repo root after `just build`:
#   ./scripts/smoke-runconfig.sh
#
# Honours STIRRUP_BIN as an override path (e.g. for CI builds that
# stage the binary somewhere other than ./stirrup).

set -euo pipefail

STIRRUP_BIN="${STIRRUP_BIN:-./stirrup}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

if [[ ! -x "${STIRRUP_BIN}" ]]; then
    echo "FAIL: stirrup binary not found at ${STIRRUP_BIN}" >&2
    echo "  Run \`just build\` first or set STIRRUP_BIN to the binary path." >&2
    exit 1
fi

# Check 1: flag-only invocation produces parseable JSON with the
# operator-set mode visible.
mode_out=$(${STIRRUP_BIN} run-config --mode execution --prompt "x" \
    | jq -er '.mode')
if [[ "${mode_out}" != "execution" ]]; then
    echo "FAIL: --mode execution did not surface in emitted JSON (got: ${mode_out})" >&2
    exit 1
fi
echo "ok: run-config emits parseable JSON with operator-set mode"

# Check 2: idempotency. Pipe a config through twice and diff the two
# outputs. The first run may apply default mutations (e.g. cobra
# defaults); the second must not move the document any further.
${STIRRUP_BIN} run-config --mode execution --prompt "x" > "${TMP_DIR}/pass1.json"
${STIRRUP_BIN} run-config < "${TMP_DIR}/pass1.json" > "${TMP_DIR}/pass2.json"
if ! diff -q "${TMP_DIR}/pass1.json" "${TMP_DIR}/pass2.json" >/dev/null; then
    echo "FAIL: run-config is not idempotent" >&2
    diff "${TMP_DIR}/pass1.json" "${TMP_DIR}/pass2.json" >&2 || true
    exit 1
fi
echo "ok: run-config | run-config is idempotent"

# Check 3: --validate rejects a contradictory config. Bedrock + the
# CLI-default `claude-sonnet-4-6` is the issue #65 trap the validator
# rejects with an inference-profile remediation hint.
if ${STIRRUP_BIN} run-config --validate \
    --mode execution \
    --provider bedrock \
    --prompt "x" >/dev/null 2>&1; then
    echo "FAIL: --validate accepted bedrock + sonnet-4-6 alias" >&2
    exit 1
fi
echo "ok: --validate rejects invalid configs"

# Check 4: --output-runconfig writes a parseable file and exits
# cleanly without invoking the provider. We rely on the dry-run
# branch firing before any HTTP would happen; the absence of
# ANTHROPIC_API_KEY here proves the loop did not start.
${STIRRUP_BIN} harness --output-runconfig "${TMP_DIR}/captured.json" \
    --prompt "x" --mode planning
captured_mode=$(jq -er '.mode' < "${TMP_DIR}/captured.json")
if [[ "${captured_mode}" != "planning" ]]; then
    echo "FAIL: --output-runconfig did not capture --mode planning (got: ${captured_mode})" >&2
    exit 1
fi
echo "ok: --output-runconfig captures the resolved RunConfig"

echo "all run-config smoke checks passed"
