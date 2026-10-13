# Lean Clean-Break Plan — croft

Remove every remnant of the shepherd-kernel-go / yaah parent and reshape the API to the agreed lean use case: **snapshot lifecycle + fork** (`Create / Exec / ReadFile / WriteFile / Capture / Apply / ReleaseState / Destroy`). Backwards compatibility explicitly not a concern.

**Diff and delta as features are retained.** This plan deletes their git-based implementations only; `snapshot-native-diff.md` re-lands `Diff` — plus `Changes` and `ReadFilesAt` — and `snapshot-native-delta.md` re-lands `ApplyDelta` as a tree-level snapshot merge, both on the lean surface, computed at the snapshotter layer with no git in the image contract. The plans are sequenced, not alternatives: lean lands first (this plan), diff second, delta third (each plan's Part 0 assumes exactly this order). Supersedes plan `1791906557075-architectural-review-remediation.md` (its surviving items are folded in; superseded items listed at the end).

## Part 1 — Inventory: where parent vocabulary remains

### A. Dead references to parent code, plans, and consumers (remove everywhere)
| Where | What |
|---|---|
| sandbox.go:225, 227, 653 | "settle through ApplyDelta", "settlement verb is Select only" |
| sandbox.go:524-527 | "sealed output … settle-apply … sealing time … the caller still holds the scope" |
| sandbox.go:699-700 | "Settlement's apply verb (settlement.go, SettleApply)" + "the overlap guard" |
| sandbox.go:712 | "its scope settles Select-only" |
| sandbox.go:796 | "sandbox_git.go's stagedIndex" |
| sandbox.go:46, 598 | "a git backend's states…", "This differs from the git backend…" (no sibling backend exists) |
| sandbox.go:239-251 | StateReleaser governance doc: "Promoting it into core's Sandbox … every backend would have to answer … the tripwire that forces that conversation" |
| sandbox_test.go:259 | "the kernel contract says states have no lifecycle" |
| sandbox_test.go:325-326 | "the contract the kernel relies on: Discard, Merge, and Halt may all call Destroy" (parent scope verbs) |
| sandbox_test.go:751 | "trace records and drift detection" |
| sandbox_test.go:833, 901 | "settle through ApplyDelta", "settle Select-only" |
| lease_test.go:533 | "The kernel's contract is that a WorkspaceState outlives…" |
| lease_test.go:755, 765 | "a git backend's state must never reach…" |
| live_test.go:5, 127, 323, 432, 435 | "plan 05 §3 / §5 item 4 / T0.5" |
| live_test.go:49, 89, 939 | "yaah" (developer CLI, sandboxWorkspace, "a host such as yaah") |
| live_test.go:284-286 | "States captured *inside* Materialize … the kernel's state-expiry business" |
| live_test.go:549 | "the acceptance sequence the substrate integration used to carry" |
| live_test.go:903, 907 | "the settlement apply verb's acceptance sequence", "sealed delta" |
| digest.go:273, 580 | "the kernel never produces", "kernel digest" (file deleted per Task 5) |
| croft.go:49-50 | "Hosts should probe the image … before selecting the backend" (removed-registry framing) |
| croft.go:105, 136-137 | "recorded by hosts in traces", "witness-level containment classification … traces and retained contexts" |

### B. Parent-shaped API surface (reshape, Task 3)
| Artifact | Parent origin | Lean replacement |
|---|---|---|
| `DeltaApplier` + `ApplyDelta` + `baseGitHead` + `StateKeyDeltaBase/Patch` + `deltaPatchScript`/`applyDeltaScript` | settlement merge verb | delete the git-era implementation only — the feature returns as a tree-level snapshot merge per `snapshot-native-delta.md` |
| `Diff` + `StateKeyGitHead` + `diffScript` + `diffNamesMarker` + `gitHead()`/`run()` | host change-reporting for traces | delete the git-era implementation only — the feature returns snapshot-native per `snapshot-native-diff.md` |
| `SandboxCapabilities` + `Containment` enum (ContainFull/Contained/Buffered/Uncontained) | parent trace taxonomy (`trace.Contain*`); registry-era backend selection | delete |
| `SandboxSpec` (Workdir ignored, Env unused, Timeout) | parent per-call spec | delete; `Create(ctx)` — callers set their own deadline |
| `WorkspaceState{Backend, Revision, Data map[string]any}` + `StateKey*` constants | parent cross-backend state envelope | typed `WorkspaceState{SnapshotKey, ParentKey, Image string}` |
| `Backend() string` + `BackendName` + foreign-backend rejection checks | multi-backend registry | delete (single backend) |
| `StateReleaser` optional interface | parent governance ("promoting it is a library-API decision") | promote `ReleaseState` into `Sandbox` |
| `ErrUnsupported` | only consumer was `ApplyDelta` | delete |
| image contract "sh, stat, GNU find, git" | git machinery | shrinks to "a shell (`/bin/sh`)" |

Note: the **name** `WorkspaceState` stays — it is generic-descriptive; the parent artifact was the envelope *shape* (Backend/Revision/Data + string-keyed payload), which Task 3 removes.

### C. Parent framing words (reword, Task 6)
- "substrate" — sandbox.go:160, README:10, live_test.go:284, 549, 906 → plain "workspace library / isolated workspace" phrasing.
- Parent-sense "kernel" in test comments (see §A rows) — rewrite to "the caller/the contract" phrasing. Keep "fork" (it names a feature croft keeps) and "host" where it plainly means "the embedding program".

### D. Linux-kernel FALSE positives — keep, do not touch
- sandbox.go:36 "kernel limit" (MAX_ARG_STRLEN), sandbox.go:113 "kernel's per-argument ceiling" (ARG_MAX), croft.go:41/110/113 "kernel exploit" (containment honesty).
- Ordinary English "drift" (test-drift guards), `io.Discard`, "discard a state" — not parent vocabulary.

## Part 2 — Target shape

```go
type Sandbox interface {
    Create(ctx context.Context) error
    Destroy(ctx context.Context) error
    Capture(ctx context.Context) (WorkspaceState, error)
    Apply(ctx context.Context, ws WorkspaceState) error
    ReleaseState(ctx context.Context, ws WorkspaceState) error
    Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error
}

type WorkspaceState struct {
    SnapshotKey string // committed snapshot this state resumes from
    ParentKey   string // snapshot this one was prepared from (lineage)
    Image       string // image the workspace was provisioned from
}
```

(`snapshot-native-delta.md` later adds `BaseKey string` — the merge base, a snapshot key. Do not treat the three-field shape as permanent.)

Capture collapses to: stop task → Commit → pin under states lease → advance (its git-head and delta blocks, sandbox.go:516-550, disappear — including their unpin-on-failure branches). Apply loses the delta-base recording (sandbox.go:652-657). Errors after this plan: `ErrStdinUnsupported`, `ErrNotImplemented` stay; everything else is `fmt.Errorf` wrapping.

## Part 3 — Tasks (execution order)

### Task 1 — Delete the git-era delta machinery (the feature is retained)

Sequencing note: the replacement (`snapshot-native-delta.md`) is a tree-level snapshot merge sharing zero code with the deleted machinery — nothing is carried across the gap except the verb name, the refuse-on-conflict contract, and the merge-base concept (it returns as `WorkspaceState.BaseKey`, a snapshot key instead of a git HEAD).

- Remove: `DeltaApplier` (croft.go), `ApplyDelta` + `var _ DeltaApplier` (sandbox.go:697-763), `deltaPatchScript`, `applyDeltaScript`, `baseGitHead` field + `baseGitHeadLocked`, `StateKeyDeltaBase/Patch`, the delta half of `splitChain` doc references.
- Capture: delete the git-head/delta block (sandbox.go:516-550) and `shell(ctx, deltaPatchScript, base)` path; Capture keeps stop→commit→pin→advance only.
- Apply: delete `s.baseGitHead = …` assignment (sandbox.go:652-657); delete the base-clears/rebind logic and `TestCreate_ResetsBase`-adjacent semantics.
- Delete tests: `TestApply_RecordsDeltaBaseAndCaptureCarriesDelta`, `TestCapture_CleanDeltaOmitsPatch`, `TestCapture_WithoutBaseCarriesNoDelta`, `TestApplyDelta_*` (all five), `TestCapture_BaseWithoutGitHeadFailsAndUnpins`, `TestCapture_DeltaFailureFailsAndUnpins`, `TestApply_StateWithoutGitHeadClearsBase`, `TestCreate_ResetsBase`, `applyBasedState` helper; live: `TestLive_ApplyDeltaMergesOntoMovedParent`, `TestLive_ApplyDeltaConflictRefusesUntouched`.

### Task 2 — Delete the git-era Diff implementation (the feature is retained)

Sequencing note: this deletion is the precondition for `snapshot-native-diff.md`, which re-lands `Diff` with the same signature and `maxLines` semantics on the lean surface. Carry the truncation contract forward explicitly so nothing is lost in the gap between plans: `maxLines <= 0` means unbounded; otherwise the text truncates at `maxLines` with a trailing `...[diff truncated]` marker (sandbox.go:946-950). The old implementation compared content against a recorded `git_head` — meaningless on non-git workspaces by construction; the replacement compares the state's actual committed layer, which is why the git machinery goes rather than stays.

- Remove: `Diff` from `Sandbox` + impl, `diffScript`, `diffNamesMarker`, `maxLines` handling, `gitHead()`, `run()` (only git consumer), `StateKeyGitHead`.
- Delete tests: `TestDiff_RequiresGitBaseline`, `TestDiff_TruncatesAndReportsFiles`, `TestDiff_StagesIntoScratchIndex`, `TestDiff_StagesOncePerCall` (lease_test.go), `TestDiff_MissingSeparatorIsAnError`, live `TestLive_DiffPreservesPreStagedChanges`; trim the diff/exec-Duration sections out of `TestLive_FullLifecycle`.
- `fake_test.go`: strip all git/diff/delta/merge canned responses from `fakeTasks` (keep mkdir/cat answers); delete `gitHead`, `diffStdout`, `deltaPatchStdout`, `mergeConflict` fields.
- `bootstrapGit` in live_test.go: delete (nothing needs a repo anymore); live default image may drop to any `sh`-bearing image (README Task 7 aligns).

### Task 3 — API reshape
- `WorkspaceState` → typed struct `{SnapshotKey, ParentKey, Image string}` (state.go); delete `Data`, `Revision`, `Backend` field, all `StateKey*` constants, `snapshotState` map-building, `stateSnapshotKey` (callers check `ws.SnapshotKey == ""` directly). `Apply`/`ReleaseState` validate `SnapshotKey != ""` with a clear error.
- Delete `SandboxCapabilities`, `Containment` + constants, `Capabilities()`, `Backend()`, `BackendName`, `ErrUnsupported` (croft.go, sandbox.go).
- Delete `SandboxSpec`; `Create(ctx context.Context) error`. Rework `TestCreate_TimeoutCoversLeaseAcquisition` to `context.WithTimeout`; delete `TestCreate_ProvisionsWorkdir`'s env concerns (none) and `TestContainerdSandbox_Capabilities`.
- Promote `ReleaseState` into `Sandbox`; delete `StateReleaser` + `var _ StateReleaser`; rewrite its doc as a plain method doc.
- `New`/`NewWithBackend`/`WithLeases` docs: drop multi-backend/registry phrasing.
- Update every test touching the old shapes (`TestApply_RejectsForeignBackend` deleted, `TestSnapshotState_RoundTrip` reworked, `TestStateSnapshotKey_RejectsBadStates` → zero-value validation tests, live tests' `state.Data[...]` reads → fields).

### Task 4 — RWMutex workspace serialization (carried from remediation plan, on the now-lean surface)
- As specified there: `opMu sync.RWMutex`; exclusive for Create/Destroy/Capture/Apply, shared for Exec/ReadFile/WriteFile/ReleaseState; public methods delegate to unexported impls; audit re-entrancy (the `ApplyDelta → WriteFile` hazard is gone with Task 1 — re-verify against the shrunken surface); document ctx-doesn't-cancel-lock-acquisition; update `TestCapture_ConcurrentWithApply` comment.

### Task 5 — Remove the digest module (H2, unchanged from remediation plan)
- Delete `digest.go` (removes §A's "kernel digest"/"kernel never produces" refs); remove `Digest()` from `WorkspaceState`; rework `TestSnapshotState_RoundTrip`; shepherd-kernel-go keeps its tested copy.

### Task 6 — Vocabulary scrub (§A + §C inventory)
- Rewrite every §A row as a self-contained statement in croft's own terms (no settlement/scopes/sealed/Select-only/parent-kernel/git-backend/Discard-Merge-Halt/plan-05/yaah/Materialize/settlement.go/sandbox_git.go).
- Replace "substrate" phrasing; keep Linux-kernel §D rows and plain-English "drift"/"discard".
- Include the `Image is required` wording check and `README` provenance paragraph rewrite.

### Task 7 — README + docs rewrite
- Image contract section: shrinks to "the image must provide a shell" (InitArgs default `/bin/sh -c "sleep infinity"`); delete git/find/stat contract, delete DeltaApplier/ForkState-adjacent capability prose.
- "What it does" list: drop Diff/merge bullets for now; keep layered/rollback/state-sharing(fork)/release. `snapshot-native-diff.md` Task 7 re-adds `Changes`/`Diff` prose when that plan lands — do not promise diff in the README until it does.
- Add the scale-envelope note (carried from remediation M3) and rewrite the Compatibility/provenance section: croft defines its own format, no digest, states are plain structs; provenance paragraph may state the extraction history factually or be dropped — prefer one factual sentence, no parent vocabulary.
- Package doc (croft.go): drop DeltaApplier paragraph, image-contract section, Diff mentions; reflect the new `Sandbox` shape and `Create(ctx)` signature; keep the design-constraints and lifecycle sections (they are croft's own and excellent).

### Task 8 — Low-priority cleanup (carried)
- Remove `taskService.Running` (interface + client.go + fake); move `execSeq` into `containerdTasks`; drop `connect`'s unused `ctx`; keep `newSandboxID` panic (documented).

### Task 9 — CHANGELOG v0.4.0 (single combined entry)
- **Breaking**: `Digest`, `DeltaApplier`/`ApplyDelta`, `Diff`, `SandboxCapabilities`/`Containment`, `SandboxSpec`, `Backend()`, `StateReleaser` removed; `WorkspaceState` is a typed struct; `ReleaseState` promoted into `Sandbox`; workspace ops serialize against lifecycle ops (RWMutex).
- **Fixed**: prepared-snapshot leak / half-wedged state on Apply's StartTask failure (carried H1, Task 10 below).
- Image contract reduced to "a shell".
- Sequencing: if `snapshot-native-diff.md` lands in the same release, do not ship Diff as removed-then-re-added — collapse into one entry: "`Diff` is now computed at the snapshotter layer; `Changes` added; the git dependency is gone." Keep the removed-then-added split only if the plans ship in separate releases.

### Task 10 — Carry-over fix: Apply StartTask-failure path (H1, unchanged)
- Exactly as remediation-plan Task 1: on `StartTask` failure after Prepare, remove the prepared successor (WithoutCancel); restart on the old `active` when `active != ""`; no bookkeeping change; add `TestApply_StartTaskFailureRemovesPreparedLayerAndRecovers` (reuse `failingStartTask`).

## Supersession map vs plan 1791906557075
- Survives unchanged: H1 (Task 10 here), H2 (Task 5), M1 RWMutex (Task 4), M3 README (folded into Task 7), low cleanup (Task 8).
- Absorbed: M4 stale-reference sweep (Tasks 6-7 are its superset).
- Superseded/moot: H3 conflict sentinel in its git-era form (returns with `snapshot-native-delta.md` as the same `ErrDeltaConflict`), M2 strict reads (typed struct has no map), M5 SandboxSpec.Env (SandboxSpec deleted).

## Risks
- **Large deletion surface**: tests are deleted with features; keep commits separable (one task per commit) so bisection stays possible.
- **Capture simplification removes tested failure paths** (git-head refusal, delta refusal) — their unpin-on-failure hygiene must be re-checked: the surviving paths (commit fail, pin fail, advance fail) keep their existing tests.
- **Live suite loses tests** (delta + diff + prestaged) — remaining live coverage: full lifecycle (trimmed), namespace isolation, destroy/leases, stdin rejection. Note the shrink honestly in the README testing section. Diff live coverage returns with `snapshot-native-diff.md`'s `TestLive_Changes*` set; delta coverage returns with `snapshot-native-delta.md`'s `TestLive_ApplyDelta*` set.
- **RWMutex re-entrancy** — audit repeated post-deletion (surface is smaller; single known hazard already gone).

## Validation
1. `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` green; `go test ./... && go test -race ./...` green.
2. Vocabulary straggler check (§A/§B/§C terms): `grep -rniE "settle|sealed|scope|substrate|yaah|shepherd|plan 05|Discard, Merge|git backend|witness|retained context|Containment|Capabilities|SandboxSpec|DeltaApplier|diff" --include=*.go --include=*.md .` — every hit must be §D-whitelisted or plain English; review hits manually.
3. API surface check: `grep -rn "StateKey\|\.Data\[\|Revision\|Backend(" --include=*.go .` returns nothing outside `Config`/doc prose.
4. Live suite (if a daemon is available) for the trimmed set; otherwise record the skip.

## Out of scope
- Renaming `WorkspaceState`/`Sandbox`/`Capture`/`Apply` (generic nouns, kept deliberately).
- Streaming I/O, resource limits, networking — lean scope keeps croft non-runtime.
- shepherd-kernel-go side changes (it keeps its own digest/canonical copy).
