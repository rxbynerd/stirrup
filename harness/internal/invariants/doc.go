// Package invariants holds source-level tests for properties that span
// several packages: secret scrubbing at telemetry sinks and in tool error
// text, sandbox boundaries, the commands run inside the sandbox, byte
// bounds on model- and transport-facing payloads, the RunConfig proto
// mirror, and the run outcome vocabulary. Each property is stated once here
// so new code cannot reintroduce a bypass unnoticed.
package invariants
