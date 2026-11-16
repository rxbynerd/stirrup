package executor

import "fmt"

// Every sandboxing executor runs the sandbox as sandboxUID:sandboxGID, the
// distroless "nonroot" pair, and mounts a size-capped, writable and
// executable scratch directory at sandboxScratchDir. It sits outside the
// workspace, so it is never committed or exported, and it is created empty
// with each sandbox. HOME and TMPDIR point at directories the executor
// creates in it before the sandbox is handed to the run.
const (
	sandboxUID int64 = 65532
	sandboxGID int64 = 65532

	sandboxScratchDir = "/scratch"
	sandboxHomeDir    = sandboxScratchDir + "/home"
	sandboxTmpDir     = sandboxScratchDir + "/tmp"
)

// hardenedUser is the container engine's user:group form of the sandbox
// identity.
var hardenedUser = fmt.Sprintf("%d:%d", sandboxUID, sandboxGID)

// scratchEnv points HOME and TMPDIR into the scratch directory. The
// executors inject it after every other variable so that it wins over a
// duplicate.
func scratchEnv() []EnvPair {
	return []EnvPair{
		{Name: "HOME", Value: sandboxHomeDir},
		{Name: "TMPDIR", Value: sandboxTmpDir},
	}
}

// scratchSetupCommand creates HOME and TMPDIR as the sandbox user. It omits
// -p so that it fails when either directory already exists, which it never
// does in a fresh sandbox.
func scratchSetupCommand() []string {
	return []string{"mkdir", "-m", "0700", sandboxHomeDir, sandboxTmpDir}
}
