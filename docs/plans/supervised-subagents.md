# Supervised speculative subagents — plan

A detailed plan for orchestrating speculative subagent work using
shepherd's scope manager, worktree isolation, trace supervision, and
checkpoint/restore rewind — with croft containers as the execution
substrate and the croft-broker SaaS as the multi-tenant delivery layer.

## 1. Problem statement

An agent (yaah) delegates speculative work to subagents: "try approach A,
try approach B, I'll pick the best." Today each subagent runs on the
host kernel (git worktrees give directory isolation but no containment),
the supervisor has no structured observation of what subagents do, and a
bad trajectory wastes the full token budget before anyone notices.

The system needs:

1. **Isolated speculative work** — each subagent in its own contained
   environment, working on its own copy of the codebase
2. **Real-time supervision** — the parent observes every subagent action
   as it happens, with enough context to judge quality
3. **Rewind and redirect** — when a subagent goes off course, the
   supervisor rolls back to a checkpoint and injects better guidance
4. **Merge with review** — the winner's work is reviewed before it
   reaches the parent's tree
5. **Provenance** — every action is traceable, verifiable, and auditable

## 2. What already exists (code inventory)

### Shepherd scope manager (shepherd-runtime-go)

| Capability | Method | Status |
|---|---|---|
| Fork child scope from parent | `ScopeManager.Fork` | ✅ tested |
| Fork with own sandbox (isolated) | `ScopeManager.ForkIsolated` | ✅ tested |
| Capture workspace state | `Scope.CaptureWorkspace` | ✅ tested |
| Restore workspace state | `Scope.ApplyWorkspace` | ✅ tested |
| Discard child (physical rollback) | `ScopeManager.Discard` | ✅ tested |
| Commit checkpoint | `Scope.CreateCheckpoint` | ✅ tested |
| Restore checkpoint (single-use) | `Scope.RestoreCheckpoint` | ✅ tested |
| Checkpoint carries conversation state | `Checkpoint.Snapshot []byte` | ✅ built |
| Merge with supervisor review | `ScopeManager.CommitMerge` | ✅ tested |
| Merge proposal with diff | `MergeProposal` | ✅ built |
| Supervisor deny with reason | `SupervisorDeniedError` | ✅ built |

### Shepherd trace model (shepherd-kernel-go)

| Capability | Type/Method | Status |
|---|---|---|
| Content-addressed records | `Record`, `CanonicalDigest` | ✅ frozen ABI |
| Causal chains | `CausedByFactIDs` | ✅ built |
| Witness (who/authority/containment) | `WitnessBody` | ✅ built |
| Containment levels | `Containment` (full/contained/buffered/uncontained) | ✅ built |
| Real-time observation bus | `EffectBus`, `EffectEvent` | ✅ built |
| Event carries yaah schema refs | `"yaah.tool.bash.v1"` | ✅ documented |
| Durable trace store | `SQLiteTraceStore` | ✅ tested |
| Cross-implementation digests | `CanonicalJSONBytes` (Python-compatible) | ✅ frozen |

### Croft (github.com/buchenberg/croft)

| Capability | Method | Status |
|---|---|---|
| Container isolation | Namespaces + overlayfs | ✅ v0.7.0 |
| Exec in container | `Exec` | ✅ tested |
| File I/O across boundary | `ReadFile`/`WriteFile` | ✅ tested |
| Snapshot capture/rollback | `Capture`/`Apply` | ✅ tested |
| Cheap fork from state | `Apply` (O(1) rebind) | ✅ tested |
| Tree-level merge | `ApplyDelta` | ✅ tested |
| Resource limits | `Config.Limits` | ✅ v0.5.0 |
| Network policy | `Config.Network` | ✅ v0.6.0 |
| Secret delivery | `Config.Secrets` | ✅ v0.7.0 |
| Argv-free writes | `Config.StagingDir` | ✅ v0.7.0 |

### Croft-broker (github.com/buchenberg/croft-broker)

| Capability | Method | Status |
|---|---|---|
| MCP tools (11) | create/destroy/exec/read/write/capture/apply/diff/delta/release/list | ✅ v0.1.0 |
| SQLite registry | tenants, sandboxes, states, audit | ✅ v0.1.0 |
| Tenancy (namespace-per-tenant) | `tenantNamespace` | ✅ v0.1.0 |
| Quotas | MaxSandboxes, MaxStates | ✅ v0.1.0 |
| Lifecycle (idle reaper, TTL) | `LifecycleEngine` | ✅ v0.1.0 |
| GC sweeper | `GCSweeper` | ✅ v0.1.0 |
| HTTP+SSE transport | `StreamableHTTPHandler` + auth | ✅ v0.1.0 |
| SQLite durability | Plan §14 | ✅ v0.1.0 |
| Metrics | `Metrics.Snapshot` | ✅ v0.1.0 |

### What is missing (the gaps)

| Gap | Size | Blocks |
|---|---|---|
| `sandbox/croft` adapter (croft as shepherd backend) | 9 days | Container-backed subagents |
| Broker tracing (shepherd trace model in croft-broker) | 7 days | Provenance in SaaS |
| Supervisor intervention policy (when to rewind) | 2 days | Automated quality control |
| `SelectBest` API (compare N states, pick winner) | 1 day | Automated fork selection |
| yaah guidance injection after rewind | yaah-side | Rewind-with-guidance |
| Cross-tenant trace isolation | 1 day | SaaS compliance |

## 3. Architecture

```
yaah agent loop (supervisor)
    │
    │  "try 3 approaches, pick the best"
    │
    ▼
shepherd ScopeManager
    │
    ├── Parent scope (the supervisor's workspace)
    │     │
    │     ├── ForkIsolated → Child A (worktree + croft container)
    │     │     ├── Subagent runs commands (croft Exec)
    │     │     ├── Reads/writes files (croft FileIO)
    │     │     ├── Every action → EffectEvent → EffectBus
    │     │     └── CreateCheckpoint(conversationSnapshot)
    │     │
    │     ├── ForkIsolated → Child B (worktree + croft container)
    │     │     ├── Same, different approach
    │     │     └── Checkpoint at each turn boundary
    │     │
    │     └── ForkIsolated → Child C (worktree + croft container)
    │           └── Same
    │
    │  Supervisor subscribes to EffectBus
    │  ┌──────────────────────────────────────────┐
    │  │ for event := range bus.Subscribe("sup") { │
    │  │   if event.SchemaRef == "yaah.tool.bash" │
    │  │     && event.Payload["exit_code"] != 0 { │
    │  │     rewindAndRedirect(event)              │
    │  │   }                                       │
    │  │   if meetsCompletionCriteria(event) {    │
    │  │     candidate <- event.TraceOwnerID       │
    │  │   }                                       │
    │  │ }                                         │
    │  └──────────────────────────────────────────┘
    │
    │  SelectBest(candidates) → winner
    │
    ├── CommitMerge(winner, reviewer) → parent tree updated
    │     └── trace: shepherd.merge.proposed.v1
    │              → shepherd.supervisor.decision.v1
    │
    └── Discard(losers) → worktrees destroyed, containers torn down
```

### 3.1 The execution substrate stack

```
┌─ yaah agent loop ────────────────────────────────┐
│  LLM conversation state                          │
│  Tool calls → shepherd ScopeManager              │
└──────────────────────┬───────────────────────────┘
                       │
┌─ shepherd Scope ─────┴───────────────────────────┐
│  Checkpoint (workspace + conversation snapshot)  │
│  CaptureWorkspace / ApplyWorkspace               │
│  Trace records (causal chain, witness)           │
└──────────────────────┬───────────────────────────┘
                       │
┌─ sandbox/croft ──────┴───────────────────────────┐
│  CroftSandbox implements sandbox.Sandbox         │
│  Worktree bind-mounted into container            │
│  Exec / ReadFile / WriteFile inside container    │
│  Capture → git tree SHA (shared mode)            │
│  Apply → git reset --hard (shared mode)          │
└──────────────────────┬───────────────────────────┘
                       │
┌─ croft ContainerdSandbox ────────────────────────┐
│  Namespaces, overlayfs, cgroups                  │
│  Resource limits, network policy, secrets        │
│  Snapshot capture, O(1) fork                     │
└──────────────────────────────────────────────────┘
```

### 3.2 The trace flow

Every subagent action produces two trace records:

```
Declaration (intent):
  Mode: Declaration
  SchemaRef: "yaah.tool.bash.v1"
  KindLabel: "bash"
  Payload: { "command": "go test ./...", "cwd": "/workspace" }
  CausedBy: [previous_action_id]

Capture (outcome):
  Mode: Capture
  SchemaRef: "yaah.tool.bash.result.v1"
  KindLabel: "bash:result"
  Payload: { "exit_code": 0, "stdout_digest": "sha256:...", "duration_ms": 1542 }
  CausedBy: [declaration_id]

Both stamped with:
  Witness: { ActorRef: "sub:variant-a", Containment: "contained", ... }
```

The supervisor reads the causal chain to reconstruct "what happened and
why" — not just "what commands ran."

## 4. Detailed tasks

### Task 1 — `sandbox/croft` adapter skeleton (1 day)

Create `sandbox/croft` package in shepherd-runtime-go.

```go
package croft

type Mode int
const (
    ModeSharedWorktree Mode = iota
    ModeNativeWorkspace
)

type CroftSandbox struct {
    sb       *croft.ContainerdSandbox
    worktree string
    repo     string
    mode     Mode
}

func NewSharedWorktree(cfg croft.Config, repo, worktree string) *CroftSandbox
func NewNativeWorkspace(cfg croft.Config) *CroftSandbox
```

Implement `Backend()`, `Capabilities()`, `Create`, `Destroy`. Stub the
rest with `ErrUnsupported`.

Tests: capability matrix per mode; Create/Destroy lifecycle; worktree
bind-mount in OCI spec.

### Task 2 — File I/O bridge (1 day)

`ReadFile` → croft `ReadFile`. `WriteFile` → croft `WriteFile`.
Shared mode: verify writes appear in host worktree. Native mode: verify
writes stay inside container.

Tests: container→worktree visibility, worktree→container visibility,
mode boundaries.

### Task 3 — Exec bridge (1 day)

Map shepherd's `ExecRequest` to croft's `ExecRequest` (same field
names). Wire `Stdin`, `Timeout`, `Env`, `Cwd`.

Tests: stdout/stderr/exit-code; stdin delivery; timeout; env/cwd;
worktree files visible to commands.

### Task 4 — Capture/Apply with git digests (2 days)

**Shared worktree mode:**
- `Capture`: `git add -A && git write-tree` → tree SHA as digest.
  Content-derived: same content → same SHA (satisfies shepherd's contract).
- `Apply`: `git reset --hard <tree-sha>`.
- Uncommitted changes: `git stash create` for a working-tree snapshot.

**Native workspace mode:**
- `Capture` → croft `Capture` (snapshot key).
- `Apply` → croft `Apply`.
- Report `ContentStable: false` in capabilities.

Tests: digest stability; capture→mutate→apply restores; native
round-trip.

### Task 5 — Diff bridge (1 day)

Shared mode: `git diff <tree-sha>` → `(diff, files, err)` per shepherd's
signature. `maxLines` cap.

Native mode: croft `Diff` + `Changes` → file list.

Tests: diff correctness; maxLines; changed file paths.

### Task 6 — ApplyDelta bridge (1 day)

Shared mode: croft `ApplyDelta` on the worktree's overlayfs view, or
`git merge` for line-level (configurable).

Native mode: croft `ApplyDelta`.

Tests: merge over diverged worktree; conflict refusal.

### Task 7 — Worktree bind-mount plumbing (1 day)

Add `Config.BindMounts` to croft:

```go
type BindMount struct {
    Source  string   // host path
    Target  string   // container path
    Options []string // "ro", "rbind", etc.
}

Config.BindMounts []BindMount
```

Validation: source exists and is a directory; target is absolute and
doesn't collide with workdir root.

Tests: mount in spec; validation rejects bad mounts.

### Task 8 — Registration + fork/choose example (1 day)

Self-register as `"containerd"` backend. End-to-end example:

```
1. Create parent scope with git in-place sandbox
2. Fork 3 children, each with a croft shared-worktree sandbox
3. Each child writes different code (simulated agent work)
4. Supervisor reads traces, evaluates diffs
5. CommitMerge(winner, reviewer)
6. Discard(losers)
```

Tests: `OpenSandbox("containerd", cfg)` works; example passes.

### Task 9 — Live tests + docs (1 day)

Live tests against real containerd (gated like croft's live suite).
README: architecture diagram, capability matrix, mode comparison.

### Task 10 — Broker tracing (7 days)

Wire shepherd's trace model into croft-broker:

**Day 1-2: Witness + record builders.**
Map tenant/token/session → `WitnessBody`. Map tool calls → `RecordDraft`
with schema refs (`broker.tool.exec.v1`, `broker.state.capture.v1`, etc.).
Stamp containment level at session start.

**Day 3-4: Trace store adapter.**
`SQLiteTraceStore` works today for single-node. For Postgres (plan §15),
write a `PostgresTraceStore` implementing the same `Store` interface.

**Day 5: EffectBus integration.**
Publish `EffectEvent` after every mutation. Subscribe in the broker for
internal policy checks (quota alerts, containment violations).

**Day 6: MCP trace query tools.**
`list_records(actor, since, containment)` → paginated records.
`verify_trace(query)` → digest verification report.
`get_witness(record_id)` → witness body.

**Day 7: Tests + docs.**
Digest stability across restart; containment filtering; bus latency
bounds; causal chain reconstruction.

### Task 11 — Supervisor intervention policy (2 days)

A policy engine that subscribes to the EffectBus and decides when to
rewind:

```go
type InterventionPolicy interface {
    Evaluate(event trace.EffectEvent) Intervention
}

type Intervention struct {
    Action   InterventionAction // none, rewind, abort, alert
    Reason   string
    Guidance string // injected after rewind
}

// Built-in policies:
// - FailFast: rewind on any non-zero exit
// - TokenBudget: abort when cumulative cost exceeds limit
// - PatternMatch: rewind on suspicious commands (curl, rm -rf, etc.)
// - QualityScore: rewind when diff quality drops below threshold
```

Tests: policy triggers correctly; rewind restores conversation + workspace.

### Task 12 — SelectBest API (1 day)

```go
func (m *ScopeManager) SelectBest(
    ctx context.Context,
    candidates []*Scope,
    criteria SelectionCriteria,
) (*Scope, trace.WorkspaceState, error)

type SelectionCriteria struct {
    DiffSize    int    // prefer smaller diffs
    TestPassed  bool   // require tests to pass
    ScoreFunc   func(trace.WorkspaceState) float64 // custom
}
```

Tests: selection by criteria; ties broken deterministically.

### Task 13 — Cross-tenant trace isolation (1 day)

Every trace record carries `TraceOwnerID = tenant:ID`. Queries filter
by owner. A tenant cannot read another tenant's records. The visibility
model (`VisibilityProfile`) enforces this at query time.

Tests: cross-tenant read denied; owner filtering; visibility profiles.

## 5. The rewind-with-guidance flow (end to end)

```
1.  Supervisor: "try implementing feature X 3 ways"
2.  ForkIsolated × 3 (worktree + croft container per child)
3.  Each subagent works:
      turn 1: write file → exec test → checkpoint(cp1)
      turn 2: fix bug → exec test → checkpoint(cp2)
      turn 3: refactor → exec test → checkpoint(cp3)
4.  EffectBus streams every action to supervisor
5.  Supervisor observes Child B's turn 2:
      Declaration: bash "rm -rf tests/"
      Capture: exit_code 0
      → Policy: PatternMatch("rm -rf") triggers REWIND
6.  Supervisor rewinds Child B to cp1:
      RestoreCheckpoint(cp1) → workspace reverted
                            → conversation snapshot returned
7.  Supervisor injects guidance (yaah-side):
      "Do not delete test files. Fix the failing test instead."
8.  Child B retries turn 2 with corrected context
9.  All children complete → SelectBest(candidates)
10. CommitMerge(winner, reviewer) → parent updated
11. Discard(losers) → containers + worktrees destroyed
12. Full trace: causal chain of every action, every rewind,
    every merge decision — digest-verified
```

## 6. Operational efficiency — honest assessment

| Claim | Verdict | Evidence |
|---|---|---|
| Speculative forks reduce wall-clock | **Real** | N forks in parallel; code proves isolation |
| Supervision catches bad trajectories early | **Real** | EffectBus + policy engine; reduces wasted tokens |
| Rewind saves compute | **Partial** | Saves sandbox work (tests, builds); LLM re-generates from restored conversation |
| Forked worktrees are cheap | **Real** | `git worktree add --detach` is O(1) hardlinks |
| N forks cost N× tokens | **True** | The dominant cost is LLM inference, not infrastructure |
| Better guidance improves outcomes | **Unproven** | Mechanism exists; effectiveness is empirical |
| Full provenance is a compliance asset | **Real** | Shepherd's canonical digests are frozen ABI, cross-verifiable |
| Multi-tenant SaaS with containment transparency | **Real** | Witness stamps + containment levels per action |

**The real win** is not token savings — it's **deployability**. A
supervised, rewound, traced multi-agent system is one you can run in
production because you can prove what happened and undo what went wrong.

## 7. Sequencing

```
Phase A: Container-backed subagents (Tasks 1-9)
  ├── Task 1 → Task 2 → Task 3 → Task 4 (critical path)
  ├── Task 5 → Task 6 (diff + delta)
  ├── Task 7 (bind mounts, unblocks Task 1's worktree mode)
  └── Task 8 → Task 9 (registration + docs)
  Total: 9 days

Phase B: Tracing + supervision (Tasks 10-13)
  ├── Task 10 (broker tracing, 7 days)
  ├── Task 11 (intervention policy, 2 days)
  ├── Task 12 (SelectBest, 1 day)
  └── Task 13 (cross-tenant isolation, 1 day)
  Total: 11 days

Phase C: yaah integration (yaah-side)
  ├── Wire EffectBus subscription into agent loop
  ├── Guidance injection after RestoreCheckpoint
  └── SelectBest + CommitMerge in the orchestrator
  Total: yaah estimate
```

**Grand total (shepherd + croft): 20 working days** across two phases.

## 8. Risks

1. **Overlayfs-over-overlayfs performance.** Worktree bind-mounted into
   an overlayfs container may be slow for large trees. Benchmark with
   10K+ file repos; fall back to copy-in/copy-out if needed.

2. **Git index contention.** Both host and container may run git
   commands simultaneously. Document "git on the host, build/test in
   the container"; use `git --no-optional-locks` in the container.

3. **Digest stability bridge.** Computing git tree SHAs from the
   container's filesystem requires either running git inside the
   container or reading the worktree from the host. The host read is
   simpler but breaks for native-mode workspaces.

4. **EffectBus drops events.** The bus is transient (drops when slow).
   Supervisors that miss events must read the trace store after the
   fact. Document this; never rely on the bus for correctness.

5. **Checkpoint blob size.** Conversation snapshots (LLM state) can be
   large. The store has a blob limit; oversized snapshots fail the
   checkpoint. May need external blob storage for long conversations.

6. **N× token cost.** Speculative forks multiply LLM spend. The
   intervention policy must include token budgets to prevent runaway
   costs. This is a yaah concern, not a shepherd/croft one.
