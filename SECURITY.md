# Security policy

Stirrup is a coding-agent harness: it holds API keys and executes code
that an LLM can be coerced into producing. We take vulnerability reports
seriously and prefer private disclosure.

## Reporting a vulnerability

**Please do not open a public GitHub issue for security findings.**

Use GitHub's private vulnerability reporting on this repository:
[github.com/rxbynerd/stirrup/security/advisories/new](https://github.com/rxbynerd/stirrup/security/advisories/new).
We will acknowledge receipt within five business days and aim to triage
within ten.

If you cannot use GitHub for some reason, contact the maintainer via the
contact details on their GitHub profile and clearly mark the message
"stirrup security report".

When reporting, please include:

- A description of the issue and the harness component involved
  (provider adapter, executor, permission policy, etc.).
- Reproduction steps — a `RunConfig` JSON or CLI invocation is ideal.
- The version: `stirrup --version` output, or the commit SHA you tested
  against.
- Whether you believe the issue is exploitable in the default
  configuration or only with specific options enabled.

## Supported versions

Stirrup is pre-1.0. Until a `v1.0.0` tag exists, security fixes land on
`main` and are picked up by the next release. Older tagged releases
will not be retroactively patched.

## Scope

In scope:

- The harness binary (`harness/`) and the eval binary (`eval/`).
- The provider adapters (Anthropic, Bedrock, OpenAI Chat Completions,
  OpenAI Responses) and the eval-only `ReplayProvider` / `ReplayExecutor`
  test doubles.
- The container executor and the in-process egress proxy.
- The Cedar policy engine and `RunConfig` validation.
- The post-edit code scanner and the multi-strategy edit pipeline.
- The MCP client.
- Build and release pipelines under `.github/workflows/`.

Out of scope:

- Third-party model behaviour (e.g. an LLM choosing to ignore your
  Cedar policy is not a stirrup vulnerability — it is the reason Cedar
  is enforced *outside* the model).
- Issues that require an attacker who already has write access to the
  configured `executor.workspace` or who can already modify the
  `RunConfig` itself.
- Findings against examples in `examples/` that are clearly labelled as
  starters (e.g. the Cedar starter policies are samples, not a default).
- Outputs of the documented [Rule of Two override](docs/safety-rings.md):
  setting `ruleOfTwo.enforce: false` deliberately weakens an invariant
  and emits a `rule_of_two_disabled` security event for the operator's
  attention.

## Hardening summary

For operator-side hardening — choosing a runtime class, an egress
allowlist, a Cedar policy, the code scanner, and the Rule of Two — see
the operator guide at [`docs/safety-rings.md`](docs/safety-rings.md). The README
covers the in-harness security controls
([README § Security](README.md#security)).

## Coordinated disclosure

We follow standard coordinated-disclosure practice: you report
privately, we triage and fix, we publish a GitHub Security Advisory
crediting you (unless you prefer to remain anonymous), and we ship the
fix as part of the next tagged release. We will agree a disclosure
timeline with you on a per-case basis; 90 days is the default upper
bound.
