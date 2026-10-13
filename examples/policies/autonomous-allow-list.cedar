// autonomous-allow-list.cedar
//
// Allow-list for an unattended execution run: read-only workspace tools,
// read_command_output (registered automatically alongside run_command),
// edit_file, and run_command are permitted; destructive shell patterns are
// forbidden. Every tool not named here falls through to the configured
// fallback, so pair this file with `deny-all`. `deny-side-effects` would
// still allow non-mutating tools such as web_fetch.
//
// The forbid clause is the same backstop as destructive-shell.cedar and is
// repeated here because a policyFile is a single document. It matches
// literal substrings, so it narrows run_command rather than sandboxing it;
// the container runtime, egress allowlist, and verifier remain the
// load-bearing controls.
//
// Cedar schema version: v1

permit (
    principal,
    action in [
        Action::"tool:read_file",
        Action::"tool:list_directory",
        Action::"tool:grep_files",
        Action::"tool:find_files",
        Action::"tool:read_command_output",
        Action::"tool:edit_file",
        Action::"tool:run_command"
    ],
    resource
);

forbid (
    principal,
    action == Action::"tool:run_command",
    resource == Tool::"run_command"
) when {
    context.input has command && (
        context.input.command like "*rm -rf*" ||
        context.input.command like "*rm -fr*" ||
        context.input.command like "*chmod -R*" ||
        context.input.command like "*chown -R*" ||
        context.input.command like "*git push --force*" ||
        context.input.command like "*git push -f*" ||
        context.input.command like "*mkfs*" ||
        context.input.command like "*dd if=*of=/dev/*"
    )
};
