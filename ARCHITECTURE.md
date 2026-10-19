# Architecture

Status: P0 source-backed description; P1–P3 Design is pending review
Last verified: 2026-10-19 (repository structure and existing CI, not production runtime behavior)

## System Purpose and boundary

The service receives Open Agent Management Protocol (OpAMP) management messages from agents, maintains status and distributes per-instance sampling configuration. It is the **control plane** for a future custom OpenTelemetry Java Agent distribution. OTLP traces do not transit this control plane.

The current P0 implementation is an in-memory interoperability PoC. Proposed PostgreSQL Registry, policy revision management and Reconciler are **not implemented**; see [DD-001](docs/design-docs/dd-001_opamp-control-plane.md) and [EP-001](docs/exec-plans/active/ep-001_build-control-plane/EXECPLAN.md).

## Code map

- cmd/server/main.go — starts the HTTP server and attaches upstream opamp-go, offers /v1/opamp and administrative API, implements the in-memory Registry and per-instance sampling policy.
- cmd/server/main_test.go — unit tests for registry configuration status, authentication, input validation and UID handling.
- cmd/testagent/main.go — standalone HTTP polling OpAMP peer; validates a sampling JSON and reports APPLIED. This **does not** execute a JVM sampler.
- docs/architecture.md — P0 protocol and threat-model limitations.
- docs/requirements/APM_CONTROL_PLANE.md — FR-001 through FR-009 functional requirements.
- docs/design-docs/dd-001_opamp-control-plane.md — future modular design (draft, unapproved).
- docs/exec-plans/active/ep-001_build-control-plane/EXECPLAN.md — delivery plan (blocked on review and identity boundaries).

## Dependency direction (observed)

~~~mermaid
flowchart LR
    ADMIN["Admin API"] --> MEMORY["In-memory Agent State"]
    OPAMP["opamp-go HTTP/WS callbacks"] --> MEMORY
    ADMIN --> CONFIG["Per-Agent Sampling JSON"]
    CONFIG --> OPAMP
    TEST["Test Agent · HTTP Polling"] <-->|"AgentToServer / ServerToAgent"| OPAMP
    FUTURE["PostgreSQL + Policy / Reconciler"] -.->|"DD-001 proposed"| OPAMP
~~~

The protocol server uses the upstream opamp-go Server.Attach() callback interface; a shared net/http server routes management APIs separately. No real database, queue or external OTLP pipeline is started in this repository.

## Verified invariants and current limits

- A 16-byte OpAMP instance UID is validated before Registry updates.
- Management endpoints and Agent communication use separate bearer tokens. **They are development-scoped shared tokens, not secure per-agent or per-tenant identity.**
- An Agent's desired sampling JSON hash and its reported RemoteConfigStatus hash are tracked separately; the Go tests verify matching acknowledgements.
- Connected control session is not equivalent to application/JVM health.
- Remote configuration is offered as a response to the next Agent message. WebSocket proactive push, persistent reconciliation and HA are not present in P0.

## Change governance and verification

- User-visible requirements: docs/requirements/APM_CONTROL_PLANE.md.
- RepoFoundryAI project Harness: .repo-foundry/skills/repo-foundry-ai/SKILL.md and docs/.engineering/specs.lock.json.
- Design and execution authority: docs/DESIGN-DOCS.md, docs/DECISIONS.md, docs/PLANS.md.
- CI: go test -race ./...; go vet ./...; RepoFoundry CLI validate.

## Known gaps

PostgreSQL migrations, tenant-bound Agent identity, immutable policy revisions, config rollback, service-level selectors, race-safe proactive WS sending, production TLS, multi-instance ownership, production SLOs and real Java Agent dynamic sampling remain unimplemented or unverified. The future DD is not an accepted ADR or an approved Design.
