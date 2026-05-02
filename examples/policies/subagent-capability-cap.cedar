// subagent-capability-cap.cedar
//
// Forbid `run_command` when the principal is a sub-agent — i.e. the
// principal carries a `parentRunId` attribute. Sub-agents are spawned by
// the parent run via the `spawn_agent` tool and should not be able to
// shell out independently. The parent run remains free to use
// `run_command`; only the descendant principals are capped.
//
// Cedar schema version: v1

forbid (
    principal,
    action == Action::"tool:run_command",
    resource == Tool::"run_command"
) when {
    principal has parentRunId && principal.parentRunId != ""
};
