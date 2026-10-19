# Research

This file is the repository entry point and rebuildable index for persistent
Research packages. Schema 1.1 concluded packages contain explicit Owner
approval plus a sealed `SYNTHESIS.md`.

## Protocol

- Active Research lives at `docs/research/active/r-NNN_slug/RESEARCH.md`.
- Concluded or cancelled Research moves to `docs/research/completed/`.
- Raw evidence belongs in `artifacts/`; focused notes belong in `notes/`;
  rounds belong in `rounds/`; review snapshots belong in `snapshots/`.
- Decision-ready Research remains active until the Owner explicitly authorizes
  conclusion.
- IDs are monotonic across active, completed, this index and
  `.epctl/state.json`.
- The managed tables are projections. Rebuild them with `epctl reindex`.

## Active

<!-- RCTL:ACTIVE:START -->
| ID | Title | Type | Status | Maturity | Owner | Updated | Synthesis | Path |
|---|---|---|---|---|---|---|---|---|
| R-001 | Open-source architecture patterns for OpAMP Control Plane | Comparative | active | exploratory | Unassigned | 2026-10-19 | [Synthesis](research/active/r-001_opamp-oss-architecture/SYNTHESIS.md) | [Research](research/active/r-001_opamp-oss-architecture/RESEARCH.md) |
<!-- RCTL:ACTIVE:END -->

## Completed

<!-- RCTL:COMPLETED:START -->
| ID | Title | Type | Status | Maturity | Owner | Updated | Synthesis | Path |
|---|---|---|---|---|---|---|---|---|
<!-- RCTL:COMPLETED:END -->
