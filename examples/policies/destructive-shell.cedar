// destructive-shell.cedar
//
// Forbid `run_command` when the requested command contains a destructive
// pattern (recursive remove, mass chmod, force-pushed git history rewrite).
// Rationale: even read-only modes that allow shell access should never
// permit history-erasing or filesystem-wide destructive operations.
//
// Cedar schema version: v1

forbid (
    principal,
    action == Action::"tool:run_command",
    resource == Tool::"run_command"
) when {
    context.input has cmd && (
        context.input.cmd like "*rm -rf*" ||
        context.input.cmd like "*rm -fr*" ||
        context.input.cmd like "*chmod -R*" ||
        context.input.cmd like "*chown -R*" ||
        context.input.cmd like "*git push --force*" ||
        context.input.cmd like "*git push -f*" ||
        context.input.cmd like "*mkfs*" ||
        context.input.cmd like "*dd if=*of=/dev/*"
    )
};
