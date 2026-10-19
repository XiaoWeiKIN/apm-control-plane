# APM Control Plane — initial requirements

Status: Draft (working baseline, not a human-approved Design or ADR)

## Goal

Manage a **custom OpenTelemetry Java Agent distribution** remotely through an OpAMP-based Go control plane. Manage instances and service/environment-scoped dynamic sampling policies, with verifiable Agent application status. OTLP trace telemetry stays on its own data plane.

## Functional requirements

| ID | Requirement | Verification / acceptance |
| --- | --- | --- |
| FR-001 | Register distinct Agent instances using OpAMP instance UID and description metadata. | Connected agents appear in inventory; new and repeated UIDs are handled deterministically; claimed attributes are not treated as authenticated tenant identity. |
| FR-002 | Track connection freshness separately from Agent-reported health. | Disconnect reports unknown runtime health instead of falsely reporting the JVM as stopped; timestamps update on messages. |
| FR-003 | Manage sampling policy by environment and `service.name`; distinguish policy revision from per-agent assignment. | Resolved policy is deterministic for a given set of selector attributes, priorities and inputs, with conflicts explicitly handled. |
| FR-004 | Send a versioned policy through OpAMP RemoteConfig when the Agent advertises the required capability. | Captured wire message carries expected bytes and hash; an unsupported Agent is not marked applied. |
| FR-005 | Record Desired and Observed configuration independently and store Agent RemoteConfigStatus with hash and failure text. | Send success leaves assignment pending; only matching `APPLIED` status marks the expected revision applied. |
| FR-006 | Preserve desired policy and assignment over control-plane restarts. | Restart with an offline test Agent, reconnect, and converge without losing last assigned revision. |
| FR-007 | Support safe rollout/rollback and audit of configuration changes before broad production deployment. | Failed or delayed updates are visible and rollback creates an auditable new desired revision. |
| FR-008 | Separate management authentication from Agent identity and support authenticated TLS transport. | Unauthorized callers cannot query or modify state; untrusted claimed service/environment attributes cannot grant tenant access. |
| FR-009 | Support the custom Java Agent distribution's runtime Head Sampling; keep application callback responsibility in the Java Agent. | A JVM integration test updates root-span sampling without restart; control-plane success requires hash-matched Agent acknowledgement plus telemetry-based validation. |

## Delivery phases

- **P0 (existing PR)**: OpAMP transport, ephemeral inventory, per-instance test policy and simulated status acknowledgement. Not a real Java sampler integration.
- **P1**: Durable Registry, identity/security boundary, PostgreSQL and reconnect.
- **P2**: Immutable policy versions, selector resolution, assignments and Reconciler.
- **P3**: Batch rollout, change history, rollback and readiness verification.
- **Java distribution**: separate workstream after control-plane contract stabilizes.

## Non-goals of initial control-plane milestones

No Collector Supervisor lifecycle control, no custom OTLP ingestion, no Java bytecode instrumentation implementation, no automatic agent binary upgrade and no claim of production HA before measured verification.

## Evidence and authority

This document records requirements identified during planning. It is not proof of implementation, an accepted ADR or an approved Design revision. Any long-lived decisions and execution plans must use RepoFoundry's official lifecycle controls.
