package core

import "fmt"

// HostFileError reports a file named in the RunConfig that the harness
// could not use from its own filesystem. Error carries the detail for the
// harness log; Redacted names only the field, so a control plane cannot
// use the field to probe the harness host.
type HostFileError struct {
	// Field is the RunConfig field that named the file, such as
	// "replayFile" or "permissionPolicy.policyFile".
	Field string
	Err   error
}

func (e *HostFileError) Error() string {
	return fmt.Sprintf("%s: %v", e.Field, e.Err)
}

func (e *HostFileError) Unwrap() error { return e.Err }

// Redacted is the error text safe to send to a control plane.
func (e *HostFileError) Redacted() string {
	return fmt.Sprintf("%s unavailable on the harness host", e.Field)
}

// APIKeyRefError reports a provider apiKeyRef that did not resolve to a
// key, so a caller can point the operator at the field that names it.
type APIKeyRefError struct {
	Ref string
	Err error
}

func (e *APIKeyRefError) Error() string {
	return fmt.Sprintf("apiKeyRef %q: %v", e.Ref, e.Err)
}

func (e *APIKeyRefError) Unwrap() error { return e.Err }
