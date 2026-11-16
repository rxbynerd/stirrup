# Sandbox image contract

**Contract version: 1**

This document states everything the `container`, `k8s` and `k8s-sandbox`
executors need from the sandbox image (`executor.image`). It is the one
place the requirements are listed; the other docs, manifests and
comments link here instead of restating them.

The `local`, `api` and `none` executors, and the internal `replay`
executor, run no image and are not covered. The reference image,
[`Dockerfile.sandbox`](../../Dockerfile.sandbox) (published as
`ghcr.io/rxbynerd/stirrup-sandbox`), satisfies every entry below. The
harness image ([`Dockerfile`](../../Dockerfile)) is distroless, has no
shell, and satisfies none of it.

The contract has two parts. [Every run](#every-run) lists what an
executor needs whatever the `RunConfig` asks for. [Per
feature](#per-feature) lists what a run needs only when it enables a
feature. An image that meets the first part and lacks a feature's
requirements fails only runs that enable that feature. Where the
executors differ, the entry names them; an entry that names none applies
to all three.

## Versioning

The contract version is an integer. It increases by one when an executor
starts needing something new from the image: a command, a flag or
argument form a command must accept, a path, or a different image
metadata behaviour. Removing or relaxing a requirement edits the lists
below without a bump, because an image that met the earlier version
still meets the later one.

A tool outside this repository records the version it checked an image
against. The version a stirrup release requires is the one in this file
at that release's tag.

| Version | Change |
|---|---|
| 1 | First complete statement of the contract. |

`harness/internal/executor/imagecontract_test.go` compares the commands
the executors' scripts run with the commands this document lists, and
`harness/internal/invariants/imagecontract_test.go` pins the places that
run commands in the sandbox. Adding a command or a new place fails them
until the command is listed here. The version bump itself is a
convention ([`CONTRIBUTING.md`](../../CONTRIBUTING.md#the-sandbox-image-contract)),
and the tests recognise command names, not flags, so a new flag on an
existing command is a bump the author has to notice.

## What the executors set

The image's own metadata is partly overridden. Executors differ.

| Image setting | `container` | `k8s` and `k8s-sandbox` |
|---|---|---|
| `ENTRYPOINT` | Not overridden. An image entrypoint runs and receives `sleep <n>` as its arguments, so it must run them as its long-lived main process; an entrypoint that exits ends the sandbox. | Overridden: the Pod command is `/bin/sh -c "sleep infinity"`. |
| `CMD` | Overridden with `sleep <n>`, where `<n>` is the sandbox lifetime in whole seconds, or `infinity` without a lifetime. | Overridden with the same command (it replaces both `ENTRYPOINT` and `CMD`). |
| `USER` | Overridden: `65532:65532`. | Overridden: `runAsUser` and `runAsGroup` `65532`, with `runAsNonRoot`. |
| `WORKDIR` | Overridden: `/workspace`. | Overridden: `/workspace`. |
| `ENV` | Kept. The executor adds `HOME` and `TMPDIR` after the rest, so they override the image's values. It also adds `HTTP_PROXY`, `http_proxy`, `HTTPS_PROXY`, `https_proxy`, `NO_PROXY` and `no_proxy` in `allowlist` network mode, and, with `sandboxIdentity` or `gitProxy`, the token variable, its `_FILE` companion and the `GIT_CONFIG_*` variables. | Same. |
| Content at `/workspace` | Hidden by the bind mount of the host workspace. | Hidden by an `emptyDir`. |
| Root filesystem | Read-only. | Writable where the image's permissions allow the sandbox user. |
| Privileges | All capabilities dropped and `no-new-privileges`. | All capabilities dropped, privilege escalation disabled, `RuntimeDefault` seccomp. |

Because of the last row, an image that relies on setuid binaries or file
capabilities does not work in the sandbox.

The executors mount these locations. The image does not create them, and
anything the image places at them is hidden.

| Path | `container` | `k8s` and `k8s-sandbox` |
|---|---|---|
| `/workspace` | Bind mount of the host workspace; keeps host ownership. | `emptyDir`; group-writable through `fsGroup: 65532`. |
| `/scratch` | `tmpfs`, `exec`, mode `1777`, sized by `executor.resources.scratchMb`. | Disk-backed `emptyDir` with the same size limit. |
| `/tmp` | `tmpfs`, `noexec`, 256 MiB. | Not mounted; see [Every run](#every-run). |
| `/dev/shm` | `tmpfs`, `noexec`, 64 MiB. | Not mounted; whatever the container runtime provides. |
| `/run/stirrup/sandbox-identity` | `tmpfs`, only with `sandboxIdentity`. | Memory-backed `emptyDir`, only with `sandboxIdentity`. |

Every executor runs commands as `65532:65532`. The executors do not need
a `passwd` or `group` entry for that identity; the reference image
declares one for tools that look the user up. The user must be able to
execute the image's tools. See
[Sandbox identity and scratch](../security.md#sandbox-identity-and-scratch)
for what `/scratch`, `HOME` and `TMPDIR` hold.

## Every run

All commands below run as the sandbox user. Most run inside `sh -c`
scripts and are resolved through the shell's `PATH`. The executors exec
these directly instead: `sleep` on `container` (resolved by the container
runtime), `sh` on all three, `ls` on all three, `mkdir` (the scratch
setup) on all three, and `tar` (file writes) on `k8s` and `k8s-sandbox`.
They are resolved through the image's `PATH`, except `/bin/sh` on `k8s`
and `k8s-sandbox`.

| Requirement | Used for |
|---|---|
| `sh` on `PATH`, and a POSIX shell at exactly `/bin/sh` on `k8s` and `k8s-sandbox`. | `container` runs `sh -c` for every command and script. `k8s` and `k8s-sandbox` run `/bin/sh -c` for the Pod command (`sleep infinity`), every command, the exec readiness wait (`/bin/sh -c :`) and the scripts below. |
| Shell builtins `kill` (with `-s`, `-0` and `-- -<pgid>`), `read -r`, `command -v`, `printf`, `echo`, `[`, `exec`, `set --`, `shift`, `exit`, and the POSIX shell language the scripts use: functions, `case`, `$(...)`, pipelines, arithmetic and parameter expansion (including `${v:-x}`). | The process-group and file-guard scripts. |
| `sleep`, accepting `infinity`, a whole number of seconds and a fraction (`0.1`). | The keep-alive command: `infinity` (`k8s`, and `container` without a lifetime) or seconds (`container` with a lifetime). The grace period of the timeout kill uses `0.1`; where `sleep` rejects a fraction the grace period collapses and the kill follows at once. |
| `rm`, accepting `-f`. | Removing the per-command process-group file. Without it, pid files accumulate in `/tmp`. |
| A writable `/tmp`. | The per-command process-group file `/tmp/.stirrup-exec-<id>.pid`. `container` mounts `/tmp`. On `k8s` and `k8s-sandbox` the image's own `/tmp` must be writable by `65532`. When it is not, a command still runs and every command's standard error carries the shell's redirection error, and a timed-out command is not killed: the kill exits `3` and the executor logs a warning. |
| `tar`, accepting `-C`, `-c`, `-x`, `-f -` and `--`. | Reading a file (`tar -C / -cf - -- <path>`). On `k8s` and `k8s-sandbox`, also writing a file (`tar -C <dir> -xf -`); `container` writes through the engine's archive API and does not use `-x`, but the contract lists it for all three. Without `tar`, reads and writes fail. |
| `readlink`, accepting `-f` and `--`. | Resolving the real path of a read or write target so a symlink in the workspace cannot reach outside it. An image without it fails closed: the first read or write returns an error naming it. |
| `mkdir`, accepting `-p` and `--`. | Creating the parent directories of a write. |
| `ls`, accepting `-1`, `-A`, `-p` and `--`. | Listing a directory (`list_directory`, and the prompt's workspace listing). |
| `mkdir`, accepting `-m` and two operands. | The scratch setup: `mkdir -m 0700 /scratch/home /scratch/tmp`, run once as the sandbox user before the sandbox is handed to the run. It fails when either directory exists. |

An image without `sh` or `sleep` does not start: on `container` the
engine fails to create or start the container, and on `k8s` the Pod ends
with the runtime's start error (see the
[troubleshooting table](k8s.md#troubleshooting)).

Optional, and used when present:

| Requirement | Used for |
|---|---|
| `setsid` | Starts each command in its own session so a timeout kills the whole process group. Without it the executor records the leader's pid and signals it and its descendants. |
| A readable `/proc/<pid>/stat` | The descendant walk that replaces the group kill when `setsid` is absent. |

## Per feature

### Workspace export

Enabled by `executor.workspaceExportTo`.

| Requirement | Used for |
|---|---|
| `tar`, accepting `-C`, `-c` and `-f -`, with `.` as the operand. | Streaming the workspace out as one archive (`tar -C <root> -cf - .`). |
| The shell builtin `[` with `-d`. | Checking that the workspace root exists. |

### Deterministic git, the git tools and the prompt's git status

Enabled by `gitStrategy: "deterministic"`, the `git_status`,
`git_changed_files`, `git_diff` and `git_show` tools, and the git-status
prompt fragment. All of them run `git` inside the sandbox, never on the
harness host. An image without `git` fails the strategy's probe, makes
the git tools report that git is not available, and leaves the prompt
fragment empty.

| Requirement | Used for |
|---|---|
| `git`, accepting `-c <key>=<value>` and `--no-optional-locks` (git 2.15 or later), with ordinary upstream behaviour for `--version`, `init`, `add`, `commit`, `checkout`, `rev-parse`, `status`, `diff` and `show`. | The strategy's branch, checkpoint and handoff commits, the tools, and the prompt fragment. |
| `test` and `pwd -P`. | The strategy resolves the workspace's physical path and checks for a `.git` entry. Common shells provide both as builtins. |

The four git tools run `git` without naming `/workspace` as a safe
directory, so for them the image must trust the workspace; the reference
image runs `git config --system safe.directory '*'`. The sandbox user
generally does not own the workspace (a `container` bind mount keeps host
ownership, and a `k8s` `emptyDir` root is normally owned by root; this
is engine and cluster behaviour, not something the executors set). A
missing trust setting makes a tool answer that the workspace is not a git
repository. The strategy and the prompt fragment name the workspace as a
safe directory themselves.

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
| `git` 2.31 or later. | The proxy wiring is delivered as `GIT_CONFIG_COUNT`-style environment configuration, which older git ignores. |
| `cat` and a shell named `sh` on `PATH`. | The inline credential helper that git runs (`!f() { ...; }; f`) to read the token file on every git operation. |

Two further prerequisites are operational rather than commands any
executor runs: git needs the `https` remote helper, and a CA bundle it
trusts when the proxy URL uses `https`.

### The search tools

`grep_files` and `find_files` search inside the sandbox on `container`,
`k8s` and `k8s-sandbox`. An image missing `grep` or `find` makes the
matching tool return an error.

| Requirement | Used for |
|---|---|
| `grep`, accepting `-r`, `-n`, `-I`, `-E` and `-e`. | Content search when `rg` is absent. Both GNU and busybox `grep` accept these. |
| `find`, accepting `-type f`. | File search. |
| `rg` (optional). | Preferred for content search when `rg --version` succeeds. It is run with `--json`, `--color never`, `--max-count`, `-e` and `--glob`. |

### Runtime diagnostics

`executor.runtime: "nitro-isolated"` runs a diagnostic inside the
sandbox after readiness, on the `k8s` executor only (see
[Nitro Sandboxes](k8s.md#eks-with-nitro-sandboxes)). It reads kernel
files through the shell alone.

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
