# Sandbox image contract

**Contract version: 1**

This document states everything the `container`, `k8s` and `k8s-sandbox`
executors need from the sandbox image (`executor.image`). It is the one
place the requirements are listed; the other docs, manifests and
comments link here instead of restating them.

The `local`, `api` and `none` executors run no image and are not
covered. The reference image, [`Dockerfile.sandbox`](../../Dockerfile.sandbox)
(published as `ghcr.io/rxbynerd/stirrup-sandbox`), satisfies every entry
below. The harness image ([`Dockerfile`](../../Dockerfile)) is distroless,
has no shell, and satisfies none of it.

The contract has two parts. [Every run](#every-run) lists what an
executor needs whatever the `RunConfig` asks for. [Per
feature](#per-feature) lists what a run needs only when it enables a
feature. An image that meets the first part and lacks a feature's
requirements fails only runs that enable that feature.

## Versioning

The contract version is an integer. It increases by one when an executor
starts needing something new from the image: a command, a flag or
argument form a command must accept, a path, or a different image
metadata behaviour. Removing or relaxing a requirement edits the lists
below without a bump, because an image that met the earlier version
still meets the later one.

A tool outside this repository records the version it checked an image
against, and compares it with the version here for the stirrup release
it targets.

| Version | Change |
|---|---|
| 1 | First complete statement of the contract. |

`harness/internal/executor/imagecontract_test.go` pins the version to the
commands the executors' scripts run, and
`harness/internal/invariants/imagecontract_test.go` pins the places that
run commands in the sandbox. Adding a command or a new place fails them
until this document and the version are updated
([`CONTRIBUTING.md`](../../CONTRIBUTING.md#the-sandbox-image-contract)).
The tests recognise command names, not flags: a new flag on an existing
command is a bump the author has to notice.

## What the executors set

The image's own metadata is partly overridden. Executors differ.

| Image setting | `container` | `k8s` and `k8s-sandbox` |
|---|---|---|
| `ENTRYPOINT` | Not overridden. An image entrypoint runs and receives `sleep <n>` as its arguments, so it must `exec` them. | Overridden: the Pod command is `/bin/sh -c "sleep infinity"`. |
| `CMD` | Overridden with `sleep <n>`, where `<n>` is the sandbox lifetime in whole seconds, or `infinity` without a lifetime. | Overridden with the same command (it replaces both `ENTRYPOINT` and `CMD`). |
| `USER` | Overridden: `65532:65532`. | Overridden: `runAsUser` and `runAsGroup` `65532`, with `runAsNonRoot`. |
| `WORKDIR` | Overridden: `/workspace`. | Overridden: `/workspace`. |
| `ENV` | Kept. The executor adds `HOME` and `TMPDIR` after the rest, so they override the image's values, plus any proxy and sandbox identity variables. | Same. |
| Content at `/workspace` | Hidden by the bind mount of the host workspace. | Hidden by an `emptyDir`. |
| Root filesystem | Read-only. | Writable where the image's permissions allow the sandbox user. |

The executors provide these locations, so the image does not create them
and anything the image places at them is hidden:

| Path | `container` | `k8s` and `k8s-sandbox` |
|---|---|---|
| `/workspace` | Bind mount of the host workspace; keeps host ownership. | `emptyDir`; group-writable through `fsGroup: 65532`. |
| `/scratch` | `tmpfs`, `exec`, mode `1777`, sized by `executor.resources.scratchMb`. | Disk-backed `emptyDir` with the same size limit. |
| `/tmp` | `tmpfs`, `noexec`, 256 MiB. | Not mounted: the image's own `/tmp` is used. |
| `/dev/shm` | `tmpfs`, `noexec`, 64 MiB. | Not mounted. |
| `/run/stirrup/sandbox-identity` | `tmpfs`, only with `sandboxIdentity`. | Memory-backed `emptyDir`, only with `sandboxIdentity`. |

Every executor runs commands as `65532:65532`. The executors do not need
a `passwd` or `group` entry for that identity; the reference image
declares one for tools that look the user up. The user must be able to
read the image's tools and any repository content the image carries
outside `/workspace`. See
[Sandbox identity and scratch](../security.md#sandbox-identity-and-scratch)
for what `/scratch`, `HOME` and `TMPDIR` hold.

## Every run

All commands below run as the sandbox user. The executors run most of
them inside `sh -c` scripts, so they are resolved through the shell's
`PATH`. The exceptions are `sleep` (resolved by the container runtime on
`container`), and `ls`, `tar` (on `k8s`) and `mkdir` (the scratch
setup), which are exec'd directly and resolved through the image `PATH`.

| Requirement | Used for |
|---|---|
| A POSIX shell at `/bin/sh`, also found as `sh` on `PATH`. | Every command a run executes (`/bin/sh -c`), the exec readiness wait on `k8s` and `k8s-sandbox` (`/bin/sh -c :`), and every script below. |
| Shell builtins `kill` (with `-s`, `-0` and `-- -<pgid>`), `read -r`, `command -v`, `printf`, `echo`, `[`, `exec`, `set --`, `shift`, `exit`, arithmetic expansion, and the `${v#x}`, `${v##x}`, `${v%x}` and `${v%%x}` parameter expansions. | The process-group and file-guard scripts. |
| `sleep`, accepting `infinity`, a whole number of seconds and a fraction (`0.1`). | The keep-alive command (`infinity` or seconds) and the grace period of the timeout kill (`0.1`). |
| `rm`, accepting `-f`. | Removing the per-command process-group file. |
| A writable `/tmp`. | The per-command process-group file `/tmp/.stirrup-exec-<id>.pid`. Without it a command still runs, but a timed-out command is not killed: the kill exits `3` and the executor logs a warning. `container` provides `/tmp`; on `k8s` the image's `/tmp` must be writable by `65532` (mode `1777`). |
| `tar`, accepting `-C`, `-c`, `-x`, `-f -` and `--`. | Reading a file (`tar -C / -cf - -- <path>`), exporting the workspace (`tar -C <root> -cf - .`), and, on `k8s` and `k8s-sandbox`, writing a file (`tar -C <dir> -xf -`). On `container` writes go through the engine's archive API, so `-x` is unused there; the contract still lists it. |
| `readlink`, accepting `-f` and `--`. | Resolving the real path of a read or write target so a symlink in the workspace cannot reach outside it. An image without it fails closed: the first read or write returns an error naming it. |
| `mkdir`, accepting `-p` and `--`. | Creating the parent directories of a write. |
| `ls`, accepting `-1`, `-A`, `-p` and `--`. | Listing a directory (`list_directory`, and the prompt's workspace listing). |
| `mkdir`, accepting `-m` and creating two directories in one call. | The scratch setup: `mkdir -m 0700 /scratch/home /scratch/tmp`, run once as the sandbox user before the sandbox is handed to the run. It fails when either directory exists. |

Optional, and used when present:

| Requirement | Used for |
|---|---|
| `setsid` | Starts each command in its own session so a timeout kills the whole process group. Without it the executor records the leader's pid and signals it and its descendants. |
| A readable `/proc/<pid>/stat` | The descendant walk that replaces the group kill when `setsid` is absent. |

## Per feature

### Deterministic git, the git tools and the prompt's git status

Enabled by `gitStrategy: "deterministic"`, the `git_status`,
`git_changed_files`, `git_diff` and `git_show` tools, and the git-status
prompt fragment. All of them run `git` inside the sandbox, never on the
harness host.

| Requirement | Used for |
|---|---|
| `git`, accepting `-c <key>=<value>`, `--no-optional-locks`, and the subcommands `--version`, `init`, `add`, `commit`, `checkout`, `rev-parse`, `status`, `diff` and `show`. | The strategy's branch, checkpoint and handoff commits, the tools, and the prompt fragment. |
| Trust of `/workspace` by git. | The git tools run `git` without `safe.directory`, and the sandbox user generally does not own the workspace (a `container` bind mount keeps host ownership; a `k8s` `emptyDir` root is owned by root). The image must trust the workspace; the reference image runs `git config --system safe.directory '*'`. The strategy and the prompt fragment name the workspace as a safe directory themselves. |
| `test` and `pwd -P`. | The strategy resolves the workspace's physical path and checks for a `.git` entry. Common shells provide both as builtins. |

### `executor.sandboxIdentity`

| Requirement | Used for |
|---|---|
| `cat`, `wc` (accepting `-c`), `tr` (accepting `-d`), `mv` (accepting `-f` and `--`), and `rm` (accepting `-f` and `--`). | Delivering and refreshing the token file: the token is read from standard input into a staging file, its byte length is checked, and the file is renamed into place. |
| The shell builtin `umask`. | The token file mode, `0600`. |
| `/run/stirrup/sandbox-identity`, writable by `65532`. | Provided by the executor; listed so an image does not shadow it. |

### `executor.gitProxy`

`gitProxy` requires `sandboxIdentity`, so the section above applies as
well.

| Requirement | Used for |
|---|---|
| `git` 2.31 or later, built with the `https` remote helper. | The proxy wiring is delivered as `GIT_CONFIG_COUNT`-style environment configuration, which older git ignores. |
| `cat` and a shell named `sh` on `PATH`. | The inline credential helper that git runs (`!f() { ...; }; f`) to read the token file on every git operation. |
| A CA bundle git trusts. | Needed when the proxy URL uses `https`. |

### The search tools

`grep_files` and `find_files` search inside the sandbox on `container`,
`k8s` and `k8s-sandbox`.

| Requirement | Used for |
|---|---|
| `grep`, accepting `-r`, `-n`, `-I`, `-E` and `-e`. | Content search when `rg` is absent. Both GNU and busybox `grep` accept these. |
| `find`, accepting `-type f`. | File search. |
| `rg` (optional). | Preferred for content search when `rg --version` succeeds. It is run with `--json`, `--color never`, `--max-count`, `-e` and `--glob`. |

### Runtime diagnostics

`executor.runtime: "nitro-isolated"` runs a diagnostic inside the
sandbox after readiness (see
[Nitro Sandboxes](k8s.md#eks-with-nitro-sandboxes)). It reads kernel files
through the shell alone.

| Requirement | Used for |
|---|---|
| `/bin/sh` with the builtins `read -r`, `printf` and `[`, and no external command. | Printing `/sys/class/dmi/id/product_name`, `/proc/net/dev` and `/proc/net/route`. The `cat` command is not used. |

The files themselves come from the guest kernel, not the image.

## Not in the contract

The executors do not need a language toolchain, a package manager, a
text editor, `curl`, or network tools. Commands the model runs through
`run_command`, and the commands of the `test-runner` verifier, hooks and
post-edit checks, are the deployment's own and run in the sandbox. The
image has to carry whatever they invoke. The reference image carries no
language toolchain, so a verifier such as `go test ./...` needs an image
derived from it that adds the toolchain.
