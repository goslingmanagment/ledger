# ledger

Double-entry ledger service for a small agency: collects money movements (exchange,
cards, payouts), posts balanced journal entries, sends nightly briefs for owner
decisions, and gives the owner's agents read-only SQL over the books.

Status: design stage. Spec (Russian): `docs/specs/2026-11-04-ledger-design.md`.
Plan: `docs/plans/2026-11-04-stage-1.md`.

No real financial data lives in this repository: code and synthetic tests only.
