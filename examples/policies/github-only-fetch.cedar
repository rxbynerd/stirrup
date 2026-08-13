// github-only-fetch.cedar
//
// Permit `web_fetch` only to GitHub's web and API hosts, raw content, and
// the official Python docs. All other URLs fall through to the configured
// fallback — pair with `deny-all` so everything not permitted here is
// denied. (`deny-side-effects` does NOT deny the rest: web_fetch is not
// workspace-mutating, so a no-match falls through to allow.)
//
// Every pattern anchors the full scheme://host/ prefix. Cedar's `like`
// wildcard `*` matches any characters INCLUDING `/` and `@`, so a
// wildcard-subdomain pattern such as `https://*.github.com/*` is
// bypassable — `https://evil.example/x.github.com/y` matches it. Add new
// hosts as fully-anchored `https://<host>/*` lines, never with a
// wildcard before or inside the host.
//
// Cedar schema version: v1

permit (
    principal,
    action == Action::"tool:web_fetch",
    resource == Tool::"web_fetch"
) when {
    context.input has url && (
        context.input.url like "https://github.com/*" ||
        context.input.url like "https://api.github.com/*" ||
        context.input.url like "https://raw.githubusercontent.com/*" ||
        context.input.url like "https://docs.python.org/*"
    )
};
