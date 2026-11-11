#!/bin/sh
set -eu
cd /workspace
printf 'go_version='; go version
printf 'git_version='; git --version
printf 'proxy=%s\n' "$HTTPS_PROXY"
date -u '+started=%Y-%m-%dT%H:%M:%SZ'
# Fetch only. Do not build or run code from the cloned repository.
# Fresh workspace required: a warm clone/cache is a different measurement.
test ! -e /workspace/stirrup-source-large
start=$(date +%s%N)
timeout 150 git -c credential.helper= -c core.hooksPath=/dev/null clone --depth 1 https://github.com/rxbynerd/stirrup.git /workspace/stirrup-source-large
end=$(date +%s%N)
printf 'clone_ms=%s\n' "$(((end-start)/1000000))"
git -C /workspace/stirrup-source-large rev-parse HEAD
du -sk /workspace/stirrup-source-large
cd /workspace/stirrup-source-large/harness
printf 'go_mod_sha256='; sha256sum go.mod
printf 'go_sum_sha256='; sha256sum go.sum
start=$(date +%s%N)
timeout 150 go mod download -json
end=$(date +%s%N)
printf 'download_ms=%s\n' "$(((end-start)/1000000))"
du -sk /workspace/modcache
date -u '+finished=%Y-%m-%dT%H:%M:%SZ'
