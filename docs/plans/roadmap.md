# The croft roadmap

The consolidated plan for croft's next arc: a lean core with snapshot-native
diff and delta, wrapped in an MCP broker for multitenancy. This document is
the sequencing and integration layer; each phase's task-level detail lives in
its own plan, and this roadmap carries only the decisions that cross plan
boundaries.

| Phase | Plan | Ships in | Status |
|---|---|---|---|
| 1 — Lean core | [lean-clean-break.md](lean-clean-break.md) | croft v0.4.0 | landed (PR #4) |
| 2 — Snapshot-native diff | [snapshot-native-diff.md](snapshot-native-diff.md) | croft v0.4.0 | landed (branch `phase2/snapshot-native-diff`) |
| 3 — Snapshot-native delta | [snapshot-native-delta.md](snapshot-native-delta.md) | croft v0.4.0 | landed (branch `phase3/snapshot-native-delta`) |
| 4 — Exec I/O | [exec-io.md](exec-io.md) | croft v0.5.0 | landed (branch `phase4/exec-io`) |
| 5 — Network policy | [network-policy.md](network-policy.md) | croft v0.6.0 | landed (branch `phase5/network-policy`) |
| 6 — Secrets delivery | [secrets-delivery.md](secrets-delivery.md) | croft v0.7.0 | landed (branch `phase6/secrets-delivery`) |
| 7 — MCP broker | [multi-tenant-sandbox-broker.md](multi-tenant-sandbox-broker.md) | croft-broker v0.1.0 | landed (v0.1.0 tagged) |

The architectural-review remediation plan is superseded by phase 1; its
surviving items are folded in there and the original is archived in
[history/](history/).

## Decisions (the consolidation layer)

These resolve conflicts between plans written independently against different
baselines. Each detail plan is amended by the corresponding decision here
wherever it disagrees.

### D1 — One release for the trio

Lean deletes the git-era `Diff` and `ApplyDelta`; the diff and delta plans
re-land them snapshot-native. Between those commits there is no working diff
or delta, so consumers pinning an interim release would see features vanish.
**Phases 1–3 ship together as one croft v0.4.0**, with a single collapsed
CHANGELOG entry ("Diff is now computed at the snapshotter layer; Changes
added; ApplyDelta is a tree-level snapshot merge; the git dependency is
gone") rather than removed-then-re-added noise. Each phase still lands as its
own commit sequence — bisection is preserved; only the release tag groups
them.

### D2 — Resource limits re-home to `Config.Limits`

The broker plan names `SandboxSpec.Limits` as its one blocking upstream
extension — but phase 1 deletes `SandboxSpec` entirely (`Create(ctx)` takes no
spec). The extension therefore lands as **`Config.Limits`** (a pointer:
`*Limits`, nil = unbounded, same shape as today), threaded into
`initSpecOpts` as `oci.WithLinuxResources*`/`oci.WithCPUMem` exactly where the
broker plan points. It is additive and lands in the library any time after
phase 1 — early in phase 4, as the first upstream PR of the broker arc, and
it does not wait for phases 2–3.

### D3 — Broker state handles are broker-minted IDs, not digests

The broker plan's registry keys states by `WorkspaceState.Digest()`; phase 1
deletes the digest module (states become plain structs — a host that wants a
digest can hash the struct itself). The broker instead mints an **opaque
state ID per capture** (`states.id` primary key, `snapshot_key` recorded
beside it), and clients only ever see broker IDs — never `WorkspaceState`
JSON, never snapshot keys. This is not a workaround; it strengthens the
broker's own §7 capability-containment rules, which already forbade returning
full state payloads. Registry writes still commit before `capture` is
acknowledged, unchanged.

### D4 — The broker is a separate repo

`croft-broker` (github.com/buchenberg/croft-broker, v0.1.0): the MCP SDK, the
SQLite registry, and auth must not enter croft's dependency tree — the same
boundary principle that produced croft in the first place. Croft stays the
library; the broker consumes it by version like any host. The broker plan's
§2 architecture and §11 milestones carry over unchanged except where D2/D3
amend them.

**Both croft repositories are private, for now** (2026-10-13). Consequence
for consumers: croft `v0.3.0` is cached by the public Go module proxy and
served immutably, so public consumers pinned to it (yaah's CI) build
unchanged. New versions from a private repo cannot be fetched by the public
proxy, so the first consumer that pins a post-v0.3.0 release — the yaah
migration PR against v0.4.0 — needs either `GOPRIVATE=github.com/buchenberg/croft`
plus credentials in its CI, or the repo public again by then. Decide at the
v0.4.0 boundary; croft-broker is created private when phase 7 starts.

### D5 — Broker tool surface, mapped to the post-lean API

The broker plan's §5 table was written against the v0.3.0 interface. Post-lean
mapping (all tools take the tenant from authN, as before):

| Tool | Maps to | Notes |
|---|---|---|
| `create_sandbox(image)` | `Create(ctx)` | image allowlist; max sandboxes; `Config.Limits` from tenant policy (D2) |
| `destroy_sandbox(id)` | `Destroy` | — |
| `exec` / `read_file` / `write_file` | `Exec` / `ReadFile` / `WriteFile` | per-sandbox lock; output and size caps |
| `capture(id)` | `Capture` | registry row before ack; broker state ID minted (D3) |
| `apply(id, state_id)` | `Apply` | registry-sourced state, tenant ACL |
| `changes(id, state_id)` / `diff(id, state_id, max_lines)` | `Changes` / `Diff` | phase 2 tools; tenant ACL |
| `apply_delta(id, state_id)` | `ApplyDelta` | phase 3 tool; same-tenant only in v1 |
| `release_state(state_id)` | `ReleaseState` | tenant ACL; idempotent; registry mark |
| `list_states(filter)` / `sandbox_info(id)` | registry | `capabilities()`/`Backend()` die with lean; `sandbox_info` reports broker-level facts (status, image, workdir, lineage, size) |

The single most important authorization rule survives the remap verbatim: no
tool accepts raw `WorkspaceState` JSON from a client; handles are broker IDs
the broker recorded (D3).

### D6 — The network default stays what it is: isolated (corrected)

**Corrected by ruling, 2026-10-15.** The original decision ("the network
default stays `host`") rested on a false premise: containerd's default spec
already unshares the network namespace, so croft's containers have never
shared the host's network — they have always been loopback-only. "No default
change" applied to reality means the default stays *isolated*:
`Config.Network` `""`/`"none"` keep today's private netns, and `"host"` is
the explicit opt-in that removes the NEWNET entry so the container joins the
daemon host's namespace (package installs, git remotes, and everything else
the host can reach — for trusted workloads, and what the broker surfaces as
per-tenant policy). Flipping the default to `host` as originally written
would silently grant every existing sandbox network access it never had: a
security regression shipped as a no-change phase, rejected. Controlled
egress remains follow-on work, not a phase. The correction is recorded in
full in the network plan.

### D7 — Secrets are files, not a vault

Secret delivery is bind-mounted host files (never env — `/proc/*/environ`
leaks it; never argv — `/proc/*/cmdline` leaks it), and croft stops there:
no encryption, rotation, issuance, or audit. The broker owns a 0600
per-tenant directory and maps credentials into mounts; vault integration is
a host concern. The same ruling extends to the argv-free write path: it is
exposure control, not secrecy.

## Sequencing and parallelism

```
Phase 1 lean ──► Phase 2 diff ──► Phase 3 delta ──► v0.4.0
      │                                            │
      │                                            └──► Phase 4 exec I/O ──► v0.5.0
      │                                                       │
      │                                                       ├──► Phase 5 network ──► v0.6.0
      │                                                       └──► Phase 6 secrets ──► v0.7.0
      │
      └──► broker Phase 0 spike (MCP over lean surface)     [croft-broker repo]
      └──► Config.Limits upstream PR (D2) ─► broker Phases 1–3
                                            broker v0.1.0 pins croft v0.5.0
                                            (output caps become
                                            Config.MaxOutputBytes);
                                            network posture and secret
                                            mounts (phases 5–6) unlock the
                                            broker's tenant-isolation and
                                            credentials features in v0.2.0
```

- Phases 1→2→3 are strictly sequential in the library (each plan's Part 0
  assumes exactly this order); phases 4–6 each follow the v0.4.0 tag, touch
  none of the diff/delta machinery, and are independent of one another —
  they can land in any order or interleaved, whichever the release train
  prefers.
- The broker Phase 0 spike (one sandbox over MCP, in-memory registry) needs
  only the lean surface and can start the moment phase 1 lands.
- `Config.Limits` (D2) and the other upstream extensions (state enumeration,
  size accounting) proceed in the library independently of diff/delta — they
  touch `initSpecOpts`, the lease plumbing, and the collaborator interfaces,
  not the diff machinery.
- Broker phases 1–2 (registry, ACL, tenancy, reaper) do not need diff/delta;
  the `changes`/`diff`/`apply_delta` tools join the surface as phases 2–3
  land, which is also the natural MCP surface-stable point for the broker's
  v0.1.0.

## Consumer impact (yaah)

Yaah consumes croft's `Diff` and the delta keys (`apply-out` reads
`delta_patch` from captured states). The v0.4.0 release changes every one of
those shapes:

- typed `WorkspaceState{SnapshotKey, ParentKey, BaseKey, Image}` replaces
  the `Data`-map envelope and all `StateKey*` constants;
- apply-out's "delta leaves the container" step becomes the post-lean
  `ApplyDelta` (tree-level: same-file divergent edits between the session
  baseline and the host now refuse rather than merge — see the delta plan's
  granularity decision; if that bites yaah's flow in practice, the line-level
  merge follow-up becomes the priority it was scoped as);
- the image contract for yaah's workspace images shrinks to `sh`/`cat`/`ls`/
  `rm` — git leaves the required set.

Yaah pins croft v0.3.0 until its migration PR; that migration is tracked in
yaah, not here. Long term, an open question below.

## Validation gates

- **Per task**: `gofmt -l .` empty; `go vet ./...` clean;
  `go test ./... && go test -race ./...` green. One task per commit.
- **Phase 2 entry**: the Task 0 live spike is a hard gate — golden change-set
  tars and findings are committed before any parser/renderer code exists.
  The spike needs a real containerd daemon (`CROFT_CONTAINERD_ADDR`); record
  the skip honestly if none is available, but do not write downstream code
  against remembered internals.
- **Phase 3 entry**: phase 2's machinery (`diffService`, `Changes`, parser,
  `ReadFilesAt`) landed and tested; the merge algebra is pure and
  table-tested before orchestration.
- **v0.4.0 tag**: the full live suite against a real daemon, including the
  diff plan's `TestLive_Changes*` set and the delta plan's
  `TestLive_ApplyDelta*` set; the `release verify` job green.
- **v0.5.0 tag**: the exec I/O plan's live set — stdin-as-data, the
  truncation marker, and both background-process tests (survives-exec and
  dies-at-Capture) — green against a real daemon.
- **v0.6.0 tag**: both network tests green (`lo`-only under `none` and the
  identical default; the host's own interface listing under `host`).
- **v0.7.0 tag**: the secrets live set green, and the fake's recorded exec
  log for a staging write contains no base64 payload — the exposure-closure
  assertion.
- **Broker v0.1.0**: its plan's §11 phase-2 exit criteria (parked sandbox
  resumes with an identical workspace; cross-tenant apply/apply_delta/release
  rejected in tests; the reaper survives broker restart), plus the chaos test
  (kill broker mid-capture, restart, workspace resumable).

## Follow-on work

Recognized gaps with no phase yet — recorded so they are choices, not
surprises. Each gets a plan only when a consumer hits it; none block v0.4.0
through v0.7.0 or the broker's v0.1.0.

- **State portability / multi-host** — states are pinned layers on one
  daemon; no export, import, or migration, and the broker is a single
  process against a single daemon. Cross-host workspaces and broker
  sharding are the gap; a state-archive format (layer chain exported as OCI
  artifacts) is the likely shape when fleet use arrives.
- **Directory API** — `ListDir`/`Glob` on the `Sandbox` interface; today
  consumers shell out via exec. Cheap filler, and the broker's tools are the
  natural first consumer (`list_files` maps to it).
- **Line-level merge** — the delta plan's accepted tree-level-only tradeoff;
  the follow-up exists as a scoped future plan there. Trigger: yaah's
  apply-out hits divergent same-file edits in practice.
- **Streaming output** — bounded-but-buffered is v0.5.0's answer; chunked
  exec output (callback or reader session) is API design on plumbing that
  already streams. Trigger: a consumer tailing long builds live.
- **Controlled egress** — the network phase's deliberate boundary: an
  egress proxy or allowlist story short of CNI machinery. Trigger: a tenant
  needs exactly-one-registry-reachable workspaces.
- **`MaxDiffBytes`/`MaxDeltaBytes`** — blob-cost ceilings, documented risks
  until a host hits the ceiling.

Permanent scope boundaries (not follow-on work, not open questions):
process-preserving capture (exec I/O plan), env-based secrets (secrets
plan), non-Linux runtimes (the README's platform statement).

## Open questions

1. **MCP SDK choice** — official `modelcontextprotocol/go-sdk` vs
   `mark3labs/mcp-go`. Decide at the broker Phase 0 spike against the actual
   tool surface, not before; yaah's existing MCP client code is a datapoint,
   not a constraint.
2. **yaah via the broker, long term** — once croft-broker exists, yaah's
   session sandbox could be an MCP tenant like any other, instead of a
   direct library consumer. That inverts the current dependency (yaah →
   croft) into (yaah → broker → croft) and would make yaah's subagent fleet a
   multitenancy use case of its own. Not decided; revisit when the broker
   reaches phase 2.
3. **Line-level merge** — the delta plan's accepted tradeoff (tree-level
   only) is the right v1; the follow-up exists as a scoped future plan in
   that document. Prioritize it only when a consumer (yaah apply-out is the
   likely first) hits divergent same-file edits in practice.
4. **Scale knob** — `MaxDiffBytes`/`MaxDeltaBytes` stay documented risks, not
   config, until a host actually hits the blob-cost ceiling.
5. **Interactive exec sessions** — deferred by the exec I/O plan's decision
   (upstream runc-v2 stdin-EOF behavior; use cases covered by stdin-as-data
   and the background-process recipe). Revisit when a consumer needs a TTY.
   Process-preserving capture is not an open question: it is a permanent
   scope boundary recorded there.
