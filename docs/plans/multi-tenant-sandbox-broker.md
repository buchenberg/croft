# croft-broker: a multi-tenant sandbox broker over croft

Plan for a service that brokers croft sandboxes to multiple tenants, fronted
as an MCP server. Croft stays the substrate; all tenancy, authorization, and
lifecycle policy lives in the broker.

## 0. Grounding (verified against croft @ v0.3.0)

- `Sandbox` interface: `Create · Destroy · Capture · Apply · Diff · Exec ·
  ReadFile · WriteFile`, optional `DeltaApplier · StateReleaser`.
- `WorkspaceState{Backend, Revision, Data}` with canonical `Digest()`;
  state keys: `snapshot_key`, `parent_key`, `image`, `git_head`,
  `delta_base`, `delta_patch` (sandbox.go:50-68).
- States are pinned on the namespace-scoped lease `croft-states`
  (client.go:204) — one per containerd namespace. `ReleaseState` is the only
  discard verb; `Destroy` never touches states.
- Task OCI spec is assembled in `initSpecOpts` (client.go:436) from
  `oci.SpecOpts` — this is where resource limits plug in. Today there are no
  CPU/memory limits, no network config, and `SandboxSpec` has only
  `Workdir/Env/Timeout`.
- Sandbox IDs are library-generated (`newSandboxID`, sandbox.go:347); callers
  cannot choose them.
- `Exec` has no stdin; `WriteFile` content travels base64 in argv (visible in
  `/proc/*/cmdline`) — no secrets through this path.
- Linux only; root or user namespaces; `Config.FIFODir` enables the
  unprivileged path.

## 1. Scope and trust model

**Goal.** One daemon, many tenants, each tenant running agent-style work:
long-lived resumable workspaces, cheap speculative forks, checkpoint/rollback.

**In scope.** Tenants are *trust-boundary peers* — one user's agent fleet,
or an org's internal teams sharing a kernel and a containerd daemon.
Authorization is about accounting, fairness, and hygiene, not about
defending against a hostile kernel exploit.

**Out of scope (v1).**
- Untrusted third parties on the shared kernel. If that requirement lands,
  the deployment model changes to croft-inside-a-per-tenant-VM; the broker
  API surface survives unchanged.
- Cross-tenant state sharing. States never cross tenants in v1. A deliberate
  share verb (`share_state`) can come later; it is a policy feature, not a
  technical one, since `ApplyDelta` already does the merge.

**Trust statement to publish:** containment is honest, not absolute; one
kernel under all tenants; secrets must never enter via `WriteFile`.

## 2. Architecture

```
MCP client (agent host)
        │  MCP (stdio or HTTP+SSE)
        ▼
┌─ broker process ────────────────────────────────┐
│  MCP tool layer     authN (tenant token)        │
│      │                 │                        │
│  tool handlers ───▶ authZ + quota checks         │
│      │                 │                        │
│  sandbox registry ─▶ state registry (DB)        │
│      │                 │                        │
│  per-sandbox locks   lifecycle engine           │
│      │              idle reaper · GC sweeper    │
└──────┼─────────────────┼────────────────────────┘
       ▼                 ▼
  croft.Sandbox     containerd daemon
  (per sandbox)     (namespace per tenant)
```

Components:

1. **MCP tool layer** — thin; maps tools to registry operations.
2. **Registry (SQLite first, Postgres later)** — the source of truth for
   tenants, sandboxes, states, quotas. Never the daemon: the daemon is
   rebuilt-from, not queried-per-call.
3. **Sandbox pool** — `map[brokerSandboxID]*entry` where `entry` holds the
   `*croft.ContainerdSandbox`, a `sync.Mutex`, tenant ID, config, and
   last-use time.
4. **Lifecycle engine** — background loops: idle reaper, GC sweeper, state
   TTL enforcer.
5. **croft library extensions** — small, optional, versioned (§6).

## 3. Tenancy layout

- **One containerd namespace per tenant**: `croft-<tenant>`. Rationale: the
  states lease and every snapshot key are namespace-scoped, so a namespace is
  a free accounting boundary; namespace teardown is the bulk-reclaim
  fallback; one tenant's states are structurally invisible to another's
  sandboxes. Cost: no cross-tenant layer dedup — accepted, since sharing was
  the thing needing ACLs.
- **Per-tenant Config**: same `Address`, `Runtime`, `Snapshotter`, `Workdir`;
  different `Namespace`. `Image` comes from the allowlist (§5).
- **Broker sandbox IDs**: the broker mints its own opaque IDs and maps them
  to croft's internal `newSandboxID` values. Client-facing handles are
  broker IDs only — croft's keys never appear to tenants except inside the
  `WorkspaceState` payload, which is a record, not a capability to the
  tenant (see §7).

## 4. Data model

```
tenants(id, name, token_hash, quota_json, created_at)
sandboxes(id, tenant_id, croft_id, namespace, image, workdir,
          status[running|parked|destroyed], created_at, last_used_at)
states(digest PK, tenant_id, sandbox_id, state_json, size_bytes,
       kind[checkpoint|fork-base|terminal], created_at,
       expires_at NULL, released_at NULL)
events(id, ts, tenant_id, actor, tool, subject, outcome)  -- audit
```

Notes:
- `states.digest` is the canonical `WorkspaceState.Digest()` — recorded
  *before* the state is returned to any client. Capture is only acknowledged
  after the registry write commits.
- `state_json` stores the full `WorkspaceState` so the broker can `Apply` on
  resume without trusting client-supplied state JSON.
- Sizes come from the snapshotter usage API (needs the library extension in
  §6 or a direct containerd client in the sweeper).

## 5. MCP tool surface

All tools take `tenant` implicitly from authN (bearer token in MCP session).

| Tool | Maps to | Broker policy enforced |
|---|---|---|
| `create_sandbox(image)` | `Create` | image allowlist; max running sandboxes |
| `destroy_sandbox(id)` | `Destroy` | — |
| `exec(id, command, args, cwd, env, timeout)` | `Exec` | per-sandbox lock; output cap; timeout cap |
| `read_file(id, path)` / `write_file(id, path, content)` | `ReadFile`/`WriteFile` | size caps; content never treated as secret (documented) |
| `capture(id)` | `Capture` | quota on states; registry insert before ack |
| `apply(id, state_digest)` | `Apply` | state must belong to tenant (or sandbox's tenant); registry-sourced `WorkspaceState`, not client JSON |
| `diff(id, state_digest, max_lines)` | `Diff` | tenant ACL on state |
| `apply_delta(id, state_digest)` | `ApplyDelta` | same-tenant only in v1 |
| `release_state(state_digest)` | `ReleaseState` | tenant ACL; idempotent; registry mark |
| `list_states(filter)` | registry | tenant scoping |
| `sandbox_info(id)` / `capabilities()` | `Backend`/`Capabilities` | — |

Deliberate omissions: no tool accepts raw `WorkspaceState` JSON from the
client. `apply` takes a digest and the broker supplies the payload. This is
the single most important authorization decision in the design — states are
bearer capabilities at the daemon level, and the broker must not forward
capabilities it cannot attribute.

## 6. croft library extensions (upstream, small and versioned)

Ordered by necessity:

1. **Resource limits** — `SandboxSpec.Limits` (CPU shares/quota, memory
   limit, pids limit) threaded into `initSpecOpts` as
   `oci.WithLinuxResources*` / `oci.WithCPUMem` opts. Without this the
   broker cannot enforce fairness; this is the only *blocking* extension.
2. **State enumeration** — a way to list pinned states in a namespace:
   either an optional interface (`StateLister.ListStates(ctx)` reading the
   `croft-states` lease resources) or a documented "broker reads the lease
   directly" recipe. Needed by the GC sweeper to find pins the registry
   doesn't know (pre-registry captures, registry corruption).
3. **State size accounting** — usage bytes per committed snapshot, for
   quotas. Can fall back to snapshotter `Usage()` via the daemon client in
   the broker if the library doesn't expose it.
4. **Caller-chosen sandbox ID** (nice-to-have) — lets the registry key be
   the croft ID, removing one mapping; not required.

Each lands as a separate PR with the in-memory fake extended first, then
live tests, mirroring croft's existing verification style.

## 7. Authorization model

- **AuthN:** tenant tokens at MCP session setup; constant-time compare
  against `token_hash`.
- **Capability containment:** a `WorkspaceState` handed to the daemon by
  anyone with the same namespace config can be applied — croft cannot
  defend this, so the broker's rules are:
  1. never accept client-supplied state payloads, only digests the broker
     recorded (§5);
  2. never return full state JSON to clients — digest, metadata, and
     lineage only;
  3. namespace-per-tenant means a stolen digest is inert without the
     namespace's daemon access anyway (defense in depth, not the primary
     control).
- **Audit:** every mutating tool call writes an `events` row before
  executing the underlying croft call.

## 8. Lifecycle engine

- **Idle reaper:** sandboxes idle > `idle_timeout` (default 30m) get
  `Capture()` → registry insert → `Destroy()`, status `parked`. Next
  `apply`/`exec` on the broker ID transparently rebuilds via `Apply(state)`
  — the crash-resume path doubles as the parking path. Compute stops; disk
  stays pinned.
- **State TTL:** optional `expires_at` per state at capture time (tenant
  policy); the sweeper releases expired states with `ReleaseState`.
- **GC sweeper (reconciliation loop):**
  - DB → daemon: registry states with no pin on the `croft-states` lease
    (external deletion) → mark released.
  - daemon → DB: pinned states absent from the registry (pre-registry
    captures, bugs) → adopt if attributable via `parent_key` lineage to a
    known tenant state, else release. Releasing unknown pins is the
    conservative default and is safe because `ReleaseState` is idempotent.
- **Namespace teardown:** operator verb per tenant (delete tenant = remove
  namespace = bulk reclaim). Requires no croft support; it is a documented
  containerd operation.

## 9. Concurrency model

- One mutex per sandbox entry; `Exec`, `Capture`, `Apply`, `WriteFile`
  serialize behind it. Rationale: `Capture` stops and restarts the task; a
  concurrent `Exec` during the commit window is a correctness bug, and the
  MCP layer permits concurrent tool calls.
- Registry writes on capture are transactional: insert state row and update
  sandbox lineage in one tx; a crash between croft's `Capture` returning and
  the registry commit is repaired by the daemon→DB sweep direction.
- Tool timeouts are broker-side ceilings over croft's own `Exec` timeout.

## 10. Deployment and security posture

- Broker runs on the daemon host (or with socket access + `FIFODir` for
  unprivileged). Root broker = simplest; unprivileged path is documented.
- Image allowlist: registry of permitted refs + digests; `create_sandbox`
  rejects everything else. This is also cost control (image pulls are shared
  across a tenant namespace).
- MCP transport: stdio for single-agent-local use; HTTP+SSE with tokens for
  the fleet case.
- Published limits: output cap per exec, file size caps, max states per
  tenant, max sandboxes per tenant, state TTL defaults.
- Non-goals restated in operator docs: no secrets in `WriteFile`; kernel
  shared across tenants.

## 11. Milestones

| Phase | Deliverable | Exit criteria |
|---|---|---|
| 0 — Spike (days) | MCP server wrapping one croft sandbox; in-memory registry; fake backend tests | all 11 tools work against `NewWithBackend` fake; live smoke test on real daemon |
| 1 — Registry + ACL (1-2 wk) | SQLite registry; token authN; digest-only state handles; audit log | capture does not ack without registry commit; cross-tenant apply/apply_delta/release rejected in tests |
| 2 — Tenancy + lifecycle (2-3 wk) | namespace-per-tenant; quotas; idle reaper with transparent park/resume; list_states | parked sandbox resumes with identical workspace; idle reaper survives broker restart |
| 3 — Upstream extensions + GC (2 wk) | croft PRs: SandboxSpec.Limits, state enumeration, size accounting; sweeper reconciliation both directions | race test: concurrent exec+capture serialized; sweeper reconciles injected orphans in test |
| 4 — Hardening | metrics (states, bytes, tenants), operator runbook, MCP client docs, Postgres option | chaos test: kill broker mid-capture, restart, workspace resumable |

Phase 3 can start in parallel with Phase 1 — the limits extension is the
critical path for any real multi-tenant deployment.

## 12. Testing strategy

- **Unit:** broker logic against croft's in-memory fake (`NewWithBackend`),
  which already enforces snapshotter invariants — authz, quotas, registry
  transactions, reaper logic.
- **Concurrency:** `go test -race`; a test that fires `exec` and `capture`
  concurrently must never observe a torn capture.
- **Live:** gated like croft's `live_test.go` (daemon present); park/resume
  round-trip, capture-then-kill-broker-then-apply.
- **Adversarial:** cross-tenant digest use; forged state JSON (must be
  rejected — broker never accepts payloads); quota exhaustion; sweeper with
  orphaned pins.

## 13. Risks and open questions

1. **Fairness without limits** — until the upstream `Limits` lands, a tenant
   can starve the daemon. Interim: per-tenant exec timeout caps and sandbox
   count limits only mitigate. Flag as a known v0 limitation.
2. **Pinned-layer growth** — states are immutable and cumulative; a
   long-running tenant's lineage grows monotonically. Policy answer: TTLs +
   `release_state` ergonomics + `size_bytes` visibility per state.
3. **Digest = identity, not content** — two captures of identical trees are
   distinct states; dedup by content is out of scope (recorded upstream
   follow-up). Registry indexes by digest and stays correct regardless.
4. **Broker as single process** — the registry DB makes state durable, but
   running sandboxes are in-process. A second broker replica cannot share
   running sandboxes; scaling is per-tenant sharding, not active-active.
5. **Apply rebinds identity** — "apply is single-use per state on this
   backend" means the broker must not promise that two sandboxes resuming
   the same state share identity; document, don't paper over.

## 14. Durability plan (SQLite across restarts)

The Phase 1–2 implementation persists tenants and audit events to SQLite
but keeps sandboxes and states in-memory. This section is the plan to close
that gap — the change that makes "idle reaper survives broker restart" and
"kill broker mid-capture, restart, workspace resumable" real.

### What must survive a restart

| Data | Survives? | Mechanism |
|---|---|---|
| Tenants | ✅ already | `tenants` table |
| Audit events | ✅ already | `events` table |
| Sandbox metadata (id, tenant, image, workdir, namespace, status) | **to add** | `sandboxes` table (schema exists; rows not written) |
| State records (digest, tenant, sandbox, state_json, kind) | **to add** | `states` table (schema exists; rows not written) |
| Running containers | ❌ never | croft's `Destroy` is idempotent; a restart finds them via the `croft.sandbox` label |

### Changes

1. **Persist on write**: `CreateSandbox` calls `SQLiteRegistry.CreateSandbox`;
   `Capture` calls `SQLiteRegistry.InsertState`; `DestroySandbox` calls
   `SQLiteRegistry.DestroySandbox`; `ReleaseState` calls
   `SQLiteRegistry.ReleaseState`. Each write goes to SQLite before the
   operation is acknowledged to the client (same capture-before-ack rule
   the plan already states for states).

2. **Reload on startup**: `NewWithSQLite` (or a `Broker.Recover()` method)
   reads all non-destroyed sandboxes and non-released states from SQLite
   and repopulates the in-memory maps. Sandbox entries with no live croft
   container are marked `parked` (the state is still valid; the next
   exec/apply rebuilds via `Apply`).

3. **Crash window**: the worst case is a capture acknowledged to the
   client but not yet committed to SQLite. The daemon-side layer exists
   but the registry has no record. The GC sweeper's daemon→DB direction
   (Phase 3 upstream extension) will find and adopt such orphans; until
   then, the window is documented as bounded by the SQLite commit latency
   (sub-millisecond for a local file).

4. **Sandbox ID mapping**: the broker's `sb-N` ID is the registry key;
   the croft sandbox's internal ID is not exposed. On restart, a `parked`
   sandbox has no croft object — `Apply` creates a fresh one via the
   factory and binds it to the recorded state. No croft ID persistence
   is needed.

### Tests

- `TestRecover_SandboxesSurviveRestart`: create sandbox → write to SQLite →
  new Broker from same DB → sandbox metadata is present.
- `TestRecover_StatesSurviveRestart`: capture → write to SQLite →
  new Broker → state digest is findable and the state JSON round-trips.
- `TestRecover_ParkedSandboxResumes`: create + capture + destroy → restart →
  apply rebuilds the workspace from the recorded state.
- `TestRecover_MidCaptureCrash`: simulate a crash between croft Capture
  and the SQLite write → the state is absent from the registry (documented
  window) → the GC sweeper's daemon→DB pass adopts it when available.

### Sequencing

This is Phase 1.5 in practice — it unlocks the Phase 2 and Phase 4 exit
criteria but does not change the tool surface. It can land as a single
commit on the same branch before merge.
