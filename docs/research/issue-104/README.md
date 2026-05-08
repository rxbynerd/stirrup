# Research artefacts — issue #104

Source: [#104 — research: Lakehouse architecture, ownership, and ingestion contract](https://github.com/rxbynerd/stirrup/issues/104).

This directory contains the underlying research that produced the synthesis comment posted on #104. Four senior researchers worked in parallel on disjoint slices; a synthesis pass cross-referenced their findings into a single deliverable.

## Files

- [`SYNTHESIS.md`](SYNTHESIS.md) — consolidated deliverable (also posted as a comment on #104). The headline architecture recommendation, ingestion-contract sketch, and cross-cutting findings.
- [`FOLLOWUPS.md`](FOLLOWUPS.md) — eight follow-up issues spun out of #104 with one-paragraph briefs. Each follow-up is also filed as its own GitHub issue, referencing #104.
- [`research-1-survey.md`](research-1-survey.md) — industry survey of Langfuse, LangSmith, Braintrust, Helicone, Arize Phoenix, Honeycomb GenAI, and the OpenTelemetry GenAI semantic conventions.
- [`research-2-architecture.md`](research-2-architecture.md) — Q1 (ownership), Q2 (OTel duplication), Q4 (ingestion contract). Recommends the hybrid 2+3 architecture and contains the proto IDL sketch.
- [`research-3-storage.md`](research-3-storage.md) — Q3 (recording vs trace separation), Q6 (schema evolution), Q9 (open-table formats).
- [`research-4-security-cli.md`](research-4-security-cli.md) — Q5 (eval CLI evolution), Q7 (PII / scrubbing), Q8 (multi-tenancy), Q10 (migration).

## Reading order for reviewers

1. `SYNTHESIS.md` — read first; covers everything at a high level.
2. `FOLLOWUPS.md` — what spins out of #104.
3. The four underlying packets — read whichever slice you have an opinion on.

The synthesis cross-references the underlying packets by filename. File:line citations point at the codebase as of the commit on which this research was conducted (2026-05-08).
