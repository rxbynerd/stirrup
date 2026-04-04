// Command stirrup is the unified CLI for the stirrup coding agent harness.
// It provides subcommands for running the harness interactively and as a
// Kubernetes job connected to a control plane.
package main

import "github.com/rxbynerd/stirrup/harness/cmd/stirrup/cmd"

func main() {
	cmd.Execute()
}
