# APM Control Plane — initial requirements

Status: Draft (working baseline, not a human-approved Design or ADR)

## Goal

Build a **general OpAMP Agent Control Plane** that can eventually manage **Java, Go, Python and other APM Agent distributions, with a later path for OpenTelemetry Collector agents**. The initial deliverable supports only a **custom OpenTelemetry Java Agent**. The common management layer covers agent identity, inventory, health, versioned configuration, rollout and acknowledgement; each Agent Kind uses its own supported configuration dialect, capabilities and application semantics. OTLP telemetry remains on a separate data plane.

## Functional requirements

| ID | Requirement | Verification / acceptance |
| --- | --- | --- |
| FR-001 | Register distinct Agent instances using OpAMP instance UID and description metadata. | Connected agents appear in inventory; new and repeated UIDs are handled deterministically; claimed attributes are not treated as authenticated workspace identity. |
| FR-002 | Track connection freshness separately from Agent-reported health. | Disconnect reports unknown runtime health instead of falsely reporting the JVM as stopped; timestamps update on messages. |
| FR-003 | Manage sampling policy by environment and `service.name`; distinguish policy revision from per-agent assignment. | Resolved policy is deterministic for a given set of selector attributes, priorities and inputs, with conflicts explicitly handled. |
| FR-004 | Send a versioned policy through OpAMP RemoteConfig when the Agent advertises the required capability. | Captured wire message carries expected bytes and hash; an unsupported Agent is not marked applied. |
| FR-005 | Record Desired and Observed configuration independently and store Agent RemoteConfigStatus with hash and failure text. | Send success leaves assignment pending; only matching `APPLIED` status marks the expected revision applied. |
| FR-006 | Preserve desired policy and assignment over control-plane restarts. | Restart with an offline test Agent, reconnect, and converge without losing last assigned revision. |
| FR-007 | Support safe rollout/rollback and audit of configuration changes before broad production deployment. | Failed or delayed updates are visible and rollback creates an auditable new desired revision. |
| FR-008 | Separate management authentication from Agent identity and support authenticated TLS transport. | Unauthorized callers cannot query or modify state; untrusted claimed service/environment attributes cannot grant workspace access. |
| FR-009 | Support the custom Java Agent distribution's runtime Head Sampling; keep application callback responsibility in the Java Agent. | A JVM integration test updates root-span sampling without restart; control-plane success requires hash-matched Agent acknowledgement plus telemetry-based validation. |
| FR-010 | Represent Agent Kind, Distribution, Distribution Version and advertised Capabilities independently of the workload service identity. | Inventory can distinguish Java, Go, Python and Collector-shaped test agents without treating `service.name` or `service.version` as universal agent-version or authorization fields. |
| FR-011 | Resolve and compile configuration through Agent-Kind/Distribution-aware adapters and reject unsupported configuration or runtime semantics. | A Java sampling policy compiles only for compatible Java targets; an incompatible or Collector-shaped Agent is reported UNSUPPORTED without receiving Java-specific JSON. Other adapters remain future work. |
| FR-012 | Separate Service, Agent Instance and Agent Session identities and lifecycles. | A service may map to multiple APM Agent instances, each with changing sessions; Collector identity does not require a Java service grouping. HTTP polling uses last-seen freshness, not persistent WebSocket connection state. |
| FR-013 | Select a relational database driver at Control Plane startup and support PostgreSQL and MySQL without coupling domain semantics to a specific SQL dialect. | Both drivers pass the same repository/transaction/CAS and restart-recovery contract suite on real PostgreSQL and MySQL; unknown drivers fail configuration validation; switching drivers does not imply automatic data migration. |

## Delivery phases

- **P0 (existing PR)**: OpAMP transport, ephemeral inventory, per-instance test policy and simulated status acknowledgement. Not a real Java sampler integration.
- **P1**: Durable Registry with generic Agent Kind/Distribution/Capabilities, identity/security boundary, PostgreSQL/MySQL storage adapters, selected-driver configuration and reconnect. Both databases require contract tests; only Java Agent runtime integration is required.
- **P2**: Generic immutable configuration revision/assignment/reconciliation core with a single Java Sampling Config Adapter; future Agent Kind adapters are contracts only.
- **P3**: Agent-kind-aware release targets and batch rollout, change history, rollback and readiness verification, exercised first with Java/test agents.
- **Java distribution**: separate workstream after control-plane contract stabilizes.
- **Future agent adapters**: Go/Python runtime support and Collector configuration/lifecycle support require their own compatibility research and runtime tests; not included in the current Control Plane implementation milestones.

## Non-goals of initial control-plane milestones

No Go/Python agent runtime integration or Collector configuration/lifecycle execution in the initial milestones; no Collector Supervisor lifecycle control, no custom OTLP ingestion, no Java bytecode instrumentation implementation, no automatic agent binary upgrade and no production HA claim before measured verification.

## Confirmed target scope

On 2026-10-19, the product direction was confirmed as multi-language APM Agent management with a future Collector extension. This is a scope input to DD-001 and EP-001, **not an approved design revision or accepted ADR**. Initial feature acceptance still tests the Java distribution first.

## Relational storage requirement

The product needs more than one database driver and may use MySQL. Database selection is a deployment parameter, not a runtime data migration facility. PostgreSQL/MySQL have different SQL and DDL behavior, so store-level dialects and migrations can be separate while required business invariants remain consistent. The proposed technology stack is described in [P1–P3 technology selection](../architecture/technology-selection.md); compatibility remains unverified until real database CI tests pass.

## Confirmed workspace scope

The resource authorization boundary is the **observability platform Workspace**, shared conceptually across Agent management, logs, metrics, traces and dashboards. The Control Plane must not create its own customer Tenant namespace. Agent Inventory and future Policy/Release/Assignment resources use the authenticated `workspace_id`. P1.1 currently scopes each credential to an operator-configured Workspace; authoritative IAM/Workspace validation remains a separate security requirement. Because no existing deployment data needs compatibility, the initial schema uses `workspace_id` directly and does not keep a `tenant_id` compatibility column.

## Evidence and authority

This document records requirements identified during planning. It is not proof of implementation, an accepted ADR or an approved Design revision. Any long-lived decisions and execution plans must use RepoFoundry's official lifecycle controls.
