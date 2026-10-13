# Architectural Review Remediation Plan — croft

Implement the findings from the 2026-10-13 architectural review in one sweep (High + Medium + Low), targeting v0.4.0.

## Decisions (agreed with user)

| Finding | Decision |
|---|---|
| H2 digest module | **Extract**: delete `digest.go` from croft; the consumer (`shepherd-kernel-go`) already owns the fully-tested copy (`trace/canonical.go` + `trace/canonical_*_test.go` + `testdata/canonical_*_v0.json`). Croft drops `WorkspaceState.Digest()` entirely. Croft-side changes only; cross-repo dedup is out of scope. |
| M1 concurrency | **Enforce via RWMutex** at the public API, plus documentation. |
| M2 state data | **Strict reads**: keep `map[string]any`, error loudly on wrong-typed values; absence keeps its existing meaning. |
| Scope | Full sweep including low-priority cleanup. |

## Execution order

Structural refactor first, behavioral changes on top: **Task 4 → 1 → 3 → 5 → 2 → 8 → 7 → 6 → 9 → 10**. Task 4 moves every public method body into an unexported impl; Tasks 1/3/5/8 then edit those impls without compounding churn.

## Tasks

### 1. H1 — Fix `Apply`'s StartTask-failure path (`sandbox.go:644-646`, becomes `applyImpl` after Task 4)
- On `StartTask(id, next)` failure after `Prepare` succeeded:
  - Always `_ = s.snap.Remove(context.WithoutCancel(ctx), next)` — the prepared successor leaks otherwise (Destroy only removes the current `activeKey`).
  - If `active != ""`: `_ = s.restart(ctx, id, active)` — the old layer still exists at this point (removal happens only on success, sandbox.go:663-665), so full recovery is correct. Ignore the restart error, consistent with the Prepare-failure branch at sandbox.go:638-639.
  - If `active == ""` (first Apply before Create): no task to restart; the sandbox stays cleanly-stopped.
  - Keep bookkeeping untouched (still points at the old `active`), mirroring the Prepare-failure branch.
- Unlike `advance()`, do NOT mark cleanly-stopped: `advance`'s old active was renamed away by Commit; here it survives.
- Test `TestApply_StartTaskFailureRemovesPreparedLayerAndRecovers` (sandbox_test.go, reuse `failingStartTask` from lease_test.go): arm → Apply fails → assert snapshot count unchanged, error surfaces, a subsequent Capture succeeds (StopTask idempotent + `Commit(active)` works), and a later un-armed Apply works.

### 2. H2 — Remove the digest module
- Delete `digest.go` (only consumer of `CanonicalDigest`/`CanonicalJSONBytes` is `state.go`; removing it drops no module deps — digest.go is stdlib-only).
- `state.go`: remove `Digest()`; reword the type doc (states are data carriers; digests are a host concern, e.g. shepherd-kernel-go's `trace.CanonicalDigest`).
- `sandbox_test.go` `TestSnapshotState_RoundTrip`: drop the digest-stability assertions, keep the round-trip assertions.
- Comment updates that reference croft's digest API:
  - `croft.go:206-210` (Capture doc: "Digest equality means identity…") — restate the identity property without referencing croft's `Digest`.
  - `README.md` Compatibility paragraph: remove `croft.state.canonical.v1` and digest-comparability claims; reword the known-gap paragraph (the sequence-derived-identity property is still true of states regardless of digesting).
- `CHANGELOG.md`: v0.4.0 entry (Task 10).

### 3. H3 — Export a delta-conflict sentinel
- Add `ErrDeltaConflict` in `croft.go` beside `ErrUnsupported` (wrap-style, modeled on `ErrStdinUnsupported`).
- `applyDeltaImpl`: capture the `ExecResult` from the merge-script call (currently `if _, err := s.execShell(...)`); when `err != nil && res.ExitCode == 3` (the script's only exit-3 branch is the conflict), return an error wrapping `ErrDeltaConflict` with the trimmed stderr (the conflicting paths). Exit-0-with-err and other codes stay unclassified operational failures.
- Update `DeltaApplier` doc (`croft.go`) and `applyDeltaImpl` doc to name the sentinel.
- Tests: `TestApplyDelta_ConflictRefusesWithPaths` asserts `errors.Is(err, ErrDeltaConflict)`; `TestLive_ApplyDeltaConflictRefusesUntouched` likewise.

### 4. M1 — Enforce workspace serialization with an RWMutex (do FIRST)
- Add `opMu sync.RWMutex` to `ContainerdSandbox`. Lock ordering: `opMu` → `s.mu` → `leaseMu` (document; verified no path takes `s.mu`/`leaseMu` then `opMu`).
- Rename each public method body to an unexported impl; the public method takes the lock and delegates:
  - Exclusive (`Lock`): `Create`, `Destroy`, `Capture`, `Apply`.
  - Shared (`RLock`): `Exec`, `ReadFile`, `WriteFile`, `Diff`, `ApplyDelta`, `ReleaseState`.
  - `Backend`, `Capabilities`, `ID` unchanged.
- Re-entrancy audit (verified): the only public→public call is `ApplyDelta` → `s.WriteFile` (sandbox.go:751) — it must call the unexported impl. All other exec paths go through `s.tasks.Exec`/`s.snap` directly. Helpers (`shell`, `execShell`, `run`, `gitHead`, `requireBackend`, lease/pin helpers) never touch `opMu`.
- Lock acquisition does not honor context cancellation (standard Go): an in-flight Exec delays a waiting Capture/Apply/Destroy until it completes. Document this in the struct doc (sandbox.go:160-167) alongside the new contract: lifecycle ops are exclusive, work ops are shared, in-flight execs finish before Capture commits — closing the delta-reads-after-advance window (sandbox.go:531-550) by construction.
- Note for tests: `lease_test.go` calls unexported helpers (`ensureLease`) directly without `opMu` — fine; `opMu` guards only the public API surface. Tests may also drive impls directly where a lock would be noise.
- `TestCapture_ConcurrentWithApply`: update its comment — it now exercises op-level serialization, not merely the race detector.

### 5. M2 — Strict state-data reads
- Add `stateString(data map[string]any, key string) (string, error)` with this exact contract:
  - key absent → `("", nil)` — absence stays meaningful (non-git workspace, clean capture);
  - value is `string` → `(v, nil)`;
  - value present, wrong type → error naming the key and the offending `%T`.
- Use it in `stateSnapshotKey`, `applyImpl` (git_head → baseGitHead), `applyDeltaImpl` (delta_base, delta_patch), `diffImpl` (git_head). A wrong-typed `delta_patch` must error, not read as the clean-capture no-op (sandbox.go:722-723).
- Tests: wrong-typed `delta_patch` → error (not nil); wrong-typed `git_head` in Diff → error naming the key.

### 6. M3 — README scale envelope
- Short "Scale envelope" note: workspaces are source-sized; `ReadFile`/`Diff` buffer whole payloads in memory; `WriteFile` costs ~1 exec per 512 KiB and exposes content via argv (`/proc/<pid>/cmdline`); large artifacts are out of design scope.

### 7. M4 — Sweep stale provenance references
- `sandbox.go:700` (`settlement.go, SettleApply`) and `sandbox.go:796` (`sandbox_git.go's stagedIndex`): rewrite as self-contained statements.
- `live_test.go`: replace `plan 05 §…` refs (lines 5, 127, 323, 435) and `yaah` refs (lines 49, 89, 903) with standalone rationale.

### 8. M5 — Honor `SandboxSpec.Env` in Create
- Thread `spec.Env` into the workdir-provisioning `ExecRequest` in `createImpl` (sandbox.go:370-374). `ExecRequest.Env` is already `map[string]string`; empty falls back to the default PATH (client.go:655-657).
- Tighten `SandboxSpec.Env`'s doc (croft.go:147-149) to say it applies to Create's provisioning step.
- Test: assert the mkdir exec's `req.Env` carries the supplied keys (fake records `allReqs`).

### 9. Low-priority cleanup
- Remove `taskService.Running` (interface sandbox.go:149, impl client.go:615, fake fake_test.go:297) — verified no callers.
- Move `execSeq` (client.go:55) into `containerdTasks` as a per-sandbox `atomic.Uint64` field (one instance per sandbox via `connect`; zero value works — `client_spec_test.go` builds `&containerdTasks{}`).
- Drop `connect`'s unused `ctx` parameter (client.go:74, `_ = ctx` at :94); update `requireBackend`.
- `newSandboxID` panic: keep as-is (documented, effectively unreachable) — explicitly out of scope.

### 10. CHANGELOG v0.4.0
- **Breaking**: `WorkspaceState.Digest` removed — digests are a host-side concern; digest module extracted to the consumer.
- **Added**: `ErrDeltaConflict`.
- **Changed**: workspace operations serialize against lifecycle operations (RWMutex contract; lock acquisition does not honor context cancellation); `SandboxSpec.Env` now applied to Create's provisioning exec.
- **Fixed**: prepared-snapshot leak and half-wedged state when the successor task fails to start during Apply.

## Risks
- **RWMutex re-entrancy deadlock** if a locked public method reaches another public method. Mitigation: impl-delegate pattern; audit complete (single known case handled).
- **Cancellation gap**: waiting on `opMu` ignores ctx; a hung Exec delays Destroy/Apply. Accepted with the user's enforcement choice; documented in the struct doc and CHANGELOG.
- **Behavior change**: callers relying on Exec proceeding during Capture now serialize (intended).
- **Breaking API**: `Digest()` removal; v0.4.0 CHANGELOG calls it out.
- Live-suite assertions (`TestLive_ApplyDeltaConflictRefusesUntouched`) move to `errors.Is`; live runs need a containerd daemon — if unavailable, record the skip in the final report.

## Validation
1. `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` green.
2. `go test ./...` and `go test -race ./...` green (unit suite, no daemon needed).
3. New tests from Tasks 1, 3, 5, 8 pass.
4. Straggler check: `grep -rn "Digest\|CanonicalJSON\|execSeq\|Running(" --include=*.go` shows no references to removed code; `grep -rn "settlement\|sandbox_git\|plan 05\|yaah"` is clean.
5. If a containerd socket is available: `sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock CROFT_CONTAINERD_IMAGE=docker.io/library/debian:bookworm go test -count=1 -run TestLive_ ./...` — otherwise note the skip.

## Out of scope
- Cross-repo dedup of `shepherd-kernel-go/trace/canonical.go` into a shared module.
- `newSandboxID` panic signature change.
- Context-cancellable lock acquisition (select/ticker loops) — plain blocking, documented.
- Streaming file I/O / containerd-native transports (M3 documents the ceiling instead).
