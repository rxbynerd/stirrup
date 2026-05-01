// github-only-fetch.cedar
//
// Permit `web_fetch` only to the GitHub family and the official Python
// docs. All other URLs fall through to the configured fallback (which
// should be deny-side-effects in hardened deployments).
//
// Pair this with a `forbid` statement in another file if you want the
// fallback to deny rather than ask upstream.
//
// Cedar schema version: v1

permit (
    principal,
    action == Action::"tool:web_fetch",
    resource == Tool::"web_fetch"
) when {
    context.input has url && (
        context.input.url like "https://*.github.com/*" ||
        context.input.url like "https://github.com/*" ||
        context.input.url like "https://raw.githubusercontent.com/*" ||
        context.input.url like "https://docs.python.org/*"
    )
};
