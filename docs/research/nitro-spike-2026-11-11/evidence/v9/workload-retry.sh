#!/bin/sh
set -eu
cd /workspace
printf 'go_version='; go version
printf 'git_version='; git --version
printf 'proxy=%s\n' "$HTTPS_PROXY"
date -u '+started=%Y-%m-%dT%H:%M:%SZ'
# Fetch only. Do not build or run code from the cloned repository.
# Fresh workspace required: a warm clone/cache is a different measurement.
test ! -e /workspace/stirrup-source-retry
start=$(date +%s)
timeout 180 git -c credential.helper= -c core.hooksPath=/dev/null clone --depth 1 https://github.com/rxbynerd/stirrup.git /workspace/stirrup-source-retry
end=$(date +%s)
printf 'clone_seconds=%s\n' "$((end-start))"
git -C /workspace/stirrup-source-retry rev-parse HEAD
du -sk /workspace/stirrup-source-retry
cd /workspace/stirrup-source-retry/types
printf 'go_mod_sha256='; sha256sum go.mod
printf 'go_sum_sha256='; sha256sum go.sum
start=$(date +%s)
timeout 180 go mod download -json
end=$(date +%s)
printf 'download_seconds=%s\n' "$((end-start))"
du -sk /workspace/modcache
date -u '+finished=%Y-%m-%dT%H:%M:%SZ'
