#!/usr/bin/env bash
# Research only. This runs commands in an existing, explicitly selected Pod.
set -euo pipefail
: "${STIRRUP_TEST_KUBECONFIG:?Set a dedicated spike kubeconfig}"
: "${STIRRUP_TEST_NAMESPACE:?Set the dedicated spike namespace}"
: "${STIRRUP_NITRO_SPIKE_POD:?Set an existing owned research Pod}"
probe_dir=$(cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(git -C "$probe_dir" rev-parse --show-toplevel)
overlay_file=$(mktemp)
trap 'rm -f "$overlay_file"' EXIT
python3 - "$repo_dir" "$probe_dir" "$overlay_file" <<'PY'
import json,pathlib,sys
repo,probe,out=map(pathlib.Path,sys.argv[1:])
json.dump({'Replace':{str(repo/'harness/internal/executor/nitro_spike_existing_test.go'):str(probe/'nitro_spike_existing_test.go')}},out.open('w'))
PY
cd "$repo_dir"
go test -overlay="$overlay_file" -tags integration_k8s -count=1 -v -timeout=8m \
  ./harness/internal/executor -run "${NITRO_TEST_PATTERN:-^TestNitroSpike_OutputDiagnostics$}"
