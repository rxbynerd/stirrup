# Stirrup Codex Agent Pack

This directory contains project-specific Codex subagent briefs for Stirrup.
They are plain Markdown files with agent frontmatter, matching the format used
by installed Codex plugin agents. If automatic project-agent discovery is not
enabled in a local Codex build, use these files as copyable subagent prompts.

## Recommended Agents

| Agent | Use for |
|---|---|
| `stirrup-backlog-cartographer` | Compressing GitHub/Beads backlog state, finding dependencies, and identifying parallel lanes. |
| `stirrup-workstream-architect` | Breaking complicated chains like the tool-use workstream into ordered PR stacks. |
| `stirrup-implementation-worker` | Implementing one bounded issue with scoped edits and verification. |
| `stirrup-provider-tooling-architect` | Provider adapters, tool schemas, request-shape compatibility, and tool-use reliability. |
| `stirrup-eval-ci-maintainer` | Eval framework, CI gate, lakehouse FileStore, replay, mining, and baselines. |
| `stirrup-security-boundary-reviewer` | Security, credentials, sandboxing, log scrubbing, Rule of Two, and MCP trust boundaries. |
| `stirrup-cli-config-reviewer` | CLI UX, `RunConfig` validation, config docs, `run-config`, and `config explain`. |
| `stirrup-test-flake-investigator` | Flaky tests, CI hangs, race-prone tests, and deterministic repros. |
| `stirrup-doc-tone-reviewer` | Docs consistency, impersonal instructional voice, and README/docs drift. |
| `stirrup-pr-review-synthesizer` | Synthesizing multi-perspective reviews into a short, actionable merge decision. |

## Suggested Fanouts

Backlog grooming:

1. `stirrup-backlog-cartographer`
2. `stirrup-workstream-architect`
3. Main agent synthesizes issue order and conflict risk.

Single issue implementation:

1. `stirrup-implementation-worker`
2. Add one specialist reviewer if the area is sensitive:
   `stirrup-security-boundary-reviewer`, `stirrup-provider-tooling-architect`,
   `stirrup-eval-ci-maintainer`, or `stirrup-cli-config-reviewer`.

PR review:

1. Run 2-4 specialist reviewers in parallel.
2. Give their outputs to `stirrup-pr-review-synthesizer`.
3. Main agent decides whether to patch, request changes, or merge.

Research packet:

1. `stirrup-workstream-architect`
2. One domain specialist.
3. `stirrup-doc-tone-reviewer` for final prose if the output lands under `docs/`.

## Shared Rules

- Read `AGENTS.md` and `CLAUDE.md` before making project-specific claims.
- Use Beads (`bd`) for durable local project task tracking where relevant.
- Prefer GitHub issue bodies and labels as the source of backlog truth.
- Keep work scoped to the assigned issue or review question.
- Verify with `just test`, targeted `go test`, or the narrowest meaningful command.
- Treat `gopls` workspace diagnostics as suspect until confirmed by Go commands.
- Do not weaken security invariants to simplify implementation.
- For completed implementation work, create a focused branch, commit verified
  changes, push, and open a draft PR for review unless the user explicitly asks
  to keep the work local.
- Never include unrelated dirty worktree changes in a commit or PR.
