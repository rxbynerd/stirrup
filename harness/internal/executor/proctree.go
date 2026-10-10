package executor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	// groupPidFileDir holds the per-call file that records the process group
	// leader of a wrapped command inside a sandbox. The container executor
	// mounts a tmpfs here and the sandbox image is expected to provide it.
	groupPidFileDir = "/tmp"

	groupPidFileRandBytes = 8
)

// groupKillTimeout bounds the follow-up exec that kills a timed-out
// command's process group inside a sandbox. It is independent of the
// command's own (already expired) deadline. A variable so tests can shrink it.
var groupKillTimeout = 15 * time.Second

// groupRunScript runs "$3" through the shell "$1" in its own session when
// setsid is available, recording the leader's pid in "$2" first. Without
// setsid the command still runs and the pid is recorded, but the kill script
// can only reach the recorded process and its descendants. The pid file is
// removed when the command finishes on its own, and the exit status is
// preserved.
//
// setsid is deliberately not the last command: a process that is already a
// process group leader makes setsid fork and report success immediately,
// which would discard the command's exit status.
const groupRunScript = `s=
command -v setsid >/dev/null 2>&1 && s=setsid
$s "$1" -c 'echo "$$" >"$1" 2>/dev/null; shift; exec "$@"' sh "$2" "$1" -c "$3"
rc=$?
rm -f "$2"
exit "$rc"`

// groupKillScript signals the group recorded in the pid file "$1": SIGTERM,
// a short grace period, then SIGKILL. When the recorded pid does not lead a
// group (no setsid), it signals the pid and its descendants found by walking
// /proc instead. A missing pid file (the pid directory was unwritable or the
// deadline fired before the pid was recorded) exits 3 so the caller can report
// that nothing was signalled. A pid that is not above 1 is ignored because the
// file is writable from inside the sandbox.
const groupKillScript = `pf=$1
read -r pid <"$pf" 2>/dev/null || exit 3
case $pid in ''|*[!0-9]*) exit 0 ;; esac
[ "$pid" -gt 1 ] 2>/dev/null || exit 0
grp=
all=$pid
if kill -0 -- "-$pid" 2>/dev/null; then
	grp=1
else
	pairs=$(for s in /proc/[0-9]*/stat; do
		read -r l <"$s" 2>/dev/null || continue
		p=${s#/proc/}; p=${p%/stat}
		r=${l##*) }
		set -- $r
		echo "$p $2"
	done)
	frontier=$pid
	while [ -n "$frontier" ]; do
		next=
		for f in $frontier; do
			for c in $(echo "$pairs" | while read -r p pp; do [ "$pp" = "$f" ] && echo "$p"; done); do
				next="$next $c"
			done
		done
		all="$all $next"
		frontier=$next
	done
fi
sig() {
	if [ -n "$grp" ]; then kill -s "$1" -- "-$pid" 2>/dev/null; else kill -s "$1" $all 2>/dev/null; fi
}
alive() {
	if [ -n "$grp" ]; then kill -0 -- "-$pid" 2>/dev/null; return; fi
	for p in $all; do kill -0 "$p" 2>/dev/null && return 0; done
	return 1
}
sig TERM
i=0
while [ "$i" -lt 10 ] && alive; do sleep 0.1; i=$((i+1)); done
if alive; then sig KILL; fi
rm -f "$pf"`

// groupedCommand describes a shell command run inside a sandbox so that the
// harness can later kill everything it started.
type groupedCommand struct {
	shell   string
	pidFile string
	command string
}

func newGroupedCommand(shell, command string) (groupedCommand, error) {
	suffix := make([]byte, groupPidFileRandBytes)
	if _, err := rand.Read(suffix); err != nil {
		return groupedCommand{}, fmt.Errorf("generate exec id: %w", err)
	}
	return groupedCommand{
		shell:   shell,
		pidFile: groupPidFileDir + "/.stirrup-exec-" + hex.EncodeToString(suffix) + ".pid",
		command: command,
	}, nil
}

// runArgv is the argv that runs the command in its own process group.
func (g groupedCommand) runArgv() []string {
	return []string{g.shell, "-c", groupRunScript, "stirrup-exec", g.shell, g.pidFile, g.command}
}

// killArgv is the argv of the follow-up exec that kills the command's group.
func (g groupedCommand) killArgv() []string {
	return []string{g.shell, "-c", groupKillScript, "stirrup-kill", g.pidFile}
}
