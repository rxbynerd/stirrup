// destructive-shell.cedar
//
// Forbid `run_command` when the requested command contains a destructive
// pattern (recursive remove, mass chmod, force-pushed git history rewrite).
// Rationale: even read-only modes that allow shell access should never
// permit history-erasing or filesystem-wide destructive operations.
//
// This is a defence-in-depth backstop, not an allow-list: it matches the
// literal patterns below and nothing else, so an obfuscated command (see
// docs/safety-rings.md, "What Ring 3 does not defend against") passes it.
// Pair with a fallback of `allow-all` when this backstop is the only gate
// on run_command — with a `deny-side-effects` fallback every run_command
// call is already denied on no-match, and this forbid only changes which
// audit event fires.
//
// The field name `command` is `run_command`'s declared input property
// (harness/internal/tool/builtins/shell.go). The schema rejects any other
// spelling before Cedar is consulted, so a policy keying on a different
// name can never fire.
//
// Cedar schema version: v1

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
