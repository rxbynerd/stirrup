package version

// version is set at build time via -ldflags:
//
//	-X github.com/rxbynerd/stirrup/harness/internal/version.version=<sha>
var version = "dev"

// Version returns the build version string.
func Version() string {
	return version
}
