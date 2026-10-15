# Snapshot-Native Delta Plan — croft

Re-land `ApplyDelta` without git: a tree-level three-way merge computed from
snapshot layers. Decided scope (2026-10-13): **tree-level only** — two
divergent edits to the same file conflict and refuse. Line-level text merge
is a separate future plan, not a milestone here.

## Part 0 — Baseline and dependencies

Assumes, in order:

1. `lean-clean-break.md` has landed — typed `WorkspaceState`, no git
   machinery, git-era `ApplyDelta` gone. Its deletion stands: this plan
   shares zero code with the deleted machinery, only the verb name and the
   refuse-on-conflict contract.
2. `snapshot-native-diff.md` has landed — this plan consumes its machinery
   directly: `diffService`, `Change`/`Changes`, the blob helper with
   per-call lease pinning, the tar→`[]Change` parser, `ReadFilesAt`, reader
   tasks, `ErrStateUnavailable`. The diff plan's Task 0 goldens pin the tar
   conventions relied on here; no new spike is needed.

The git delta did three jobs; each has a snapshot-native replacement:

| git's job | git mechanism | replacement |
|---|---|---|
| record a merge base | `delta_base` = git HEAD at last Apply | `BaseKey` = snapshot key of the last applied state — lineage is already recorded |
| represent their changes | `delta_patch`, git binary patch carried in the state | `Changes(BaseKey → SnapshotKey)` — the daemon's diff over two committed layers; nothing is carried in the state |
| three-way merge, refuse atomically | `git merge-tree` in the object database | tree-level merge of two change sets against one base, computed fully before any write |

What the old version had that this one deliberately does not (documented,
decided — see Risks): line-level granularity (divergent same-file edits now
conflict), rename detection (rename+edit reads as delete+add), and a
self-contained patch (the merge needs the base layer still pinned).

What it gains: it works on workspaces that were never git repositories —
the headline — and the image contract stays git-free.

## Part 1 — Mechanism

### 1.1 The base: `BaseKey`, not a git HEAD

- `ContainerdSandbox` gains `baseKey string`: the snapshot key the working
  tree diverged from, successor of the old `baseGitHead`.
- `Create` clears it; `Apply(ws)` sets `baseKey = ws.SnapshotKey` (the tree
  after Apply is exactly ws's layer); `ApplyDelta` leaves it untouched.
- `Capture` records it verbatim: `WorkspaceState.BaseKey`. Empty means the
  sandbox never Applied — the successor of the old "state without a git
  base cannot be merged": `ApplyDelta` errors clearly; no sentinel needed.
- `BaseKey` vs `ParentKey`: identical on the first capture after an Apply,
  but they diverge on repeated captures — the second capture's parent is the
  first capture, while its base is still the last applied state. The delta
  spans every layer since the base, matching the old patch's span. Both
  fields are load-bearing; do not collapse them.

### 1.2 The merge: two change sets against one base

`ApplyDelta(ws)` on a sandbox whose tree has moved past `ws.BaseKey`:

- **base** = the committed layer of `ws.BaseKey`. Pinned by whoever holds
  it; released → `ErrStateUnavailable`, the same contract `Changes`/`Diff`
  already accept.
- **theirs** = `Changes(base → ws.SnapshotKey)` via the diff service.
- **ours** = `Changes(base → current active)` — `DiffMounts` mounts the
  running active a second time read-only (the diff plan's multi-mount
  property).

Tree-level merge, per path:

- only theirs changed → take theirs.
- only ours changed → keep ours.
- both → delete+delete keep; modify with identical bytes and mode keep;
  every other combination (modify/delete, delete/modify, divergent modify,
  mode disagreement) → conflict.

Pure function `mergeChanges(theirs, ours []Change) (apply []Change,
conflicts []Change)` — table-testable in isolation.

### 1.3 Refusal and application

- Everything is computed before anything is written. Conflict refusal is
  exact: workspace untouched, error wraps `ErrDeltaConflict` and names the
  conflicting paths. Parity with `merge-tree`'s exit-3.
- Clean merge: fetch theirs' bodies via `ReadFilesAt(ws, paths)`; apply with
  `WriteFile` (add/modify, mode included) and an exec `rm -rf --` (delete).
  The application phase is not itself transactional — same as the git
  version's clean path (`git apply` onto the working tree); the *refusal* is
  the atomic part, and it is.
- Blob lease hygiene is the house rule: both per-call drops paired on every
  failure path, cancel-free teardown.

### 1.4 Image contract consequence

`ApplyDelta` adds `rm` to the diff plan's shell/`cat`/`ls` contract —
exercised only by the delete-application path. State it in the README
rather than assuming it silently; busybox and coreutils both carry it.

## Part 2 — Target shape

```go
type WorkspaceState struct {
    SnapshotKey string
    ParentKey   string
    BaseKey     string // layer this state's changes are relative to; "" = fresh lineage
    Image       string
}

// Added to Sandbox (lean surface is deliberately breaking-friendly):
//
// ApplyDelta merges ws's changes — everything since ws.BaseKey — onto the
// current workspace at the tree level. Mutating on success; the workspace
// is untouched when it refuses. Errors: ErrDeltaConflict (conflicting
// paths), ErrStateUnavailable (base or state layer released).
ApplyDelta(ctx context.Context, ws WorkspaceState) error

var ErrDeltaConflict = errors.New("croft: delta conflicts with the workspace's own changes")
```

Locking: shared (`RLock`), like `WriteFile` — it mutates but never stops the
task; exclusive lifecycle ops serialize it away, matching the diff plan's
reader-task interaction notes.

## Part 3 — Tasks (one task per commit, bisectable)

### Task 1 — `BaseKey` plumbing
- Field on `ContainerdSandbox`; clear in Create, set in Apply, record in
  Capture; zero-value validation on the state.
- Tests (snapshot semantics; the old git-era test names are retired, not
  resurrected): apply records the base; capture carries it; a second
  capture keeps the base while the parent advances; create clears it.

### Task 2 — `mergeChanges` algebra
- Pure function + table tests covering every combination class in §1.2,
  including directories, mode-only changes, and type changes
  (file↔dir↔symlink).

### Task 3 — Content planning
- Pre-fetch plan: theirs' bodies via `ReadFilesAt`; ours' bodies where the
  identical-content check needs them; everything fetched before any
  mutation; cancel-safe teardown on fetch failure; size guard consistent
  with the diff plan's `maxDiffInlineBytes`.

### Task 4 — `ApplyDelta` orchestration
- Validate `ws` (`SnapshotKey` and `BaseKey` non-empty; clear error on empty
  `BaseKey`); `diffBlob(base→theirs)` and `diffBlob(base→active)`; parse;
  `mergeChanges`; on conflict return `ErrDeltaConflict` + paths with zero
  mutation; else apply per §1.3.
- Doc contract: mutating on success, untouched on refusal, base-released
  error, tree-level granularity stated, concurrent-exec consistency equal
  to `Diff` (results as of some moment during the call).

### Task 5 — Tests: fake + live
- Fake: the full merge-class matrix against the fake diff service;
  a conflict refusal leaves the fake's tree bit-identical.
- Live, parity-named where the concern survived: `TestLive_ApplyDeltaMergesOntoMovedParent`,
  `TestLive_ApplyDeltaConflictRefusesUntouched`,
  `TestLive_ApplyDeltaWorksOnNonGitWorkspace` (the headline — impossible
  before), `TestLive_ApplyDeltaAfterBaseReleasedFails`,
  `TestLive_ApplyDeltaBlobLeavesNoContentResidue`.

### Task 6 — Docs + CHANGELOG
- README capability prose: ApplyDelta returns with tree-level semantics; the
  `rm` contract note; the "no git" claim extended to merge.
- `docs/overview.md`: rewrite the hand-off section for the snapshot merge;
  the merge sequence diagram replaces the git one.
- CHANGELOG: Added — `ApplyDelta` (snapshot-native, tree-level), `BaseKey`,
  `ErrDeltaConflict`; contract now shell/`cat`/`ls`/`rm`.

## Risks

- **Granularity tradeoff, accepted and decided**: divergent same-file edits
  conflict where git's line-level merge would have succeeded. Refuse rather
  than pretend; the line-level follow-up is a separate plan if a host hits
  it enough to ask.
- **No rename detection**: a rename plus edit reads as delete+add; if the
  other side touched the old path, that is a conflict.
- **The base is a layer, not a payload**: merge needs `ws.BaseKey` pinned;
  released bases fail with `ErrStateUnavailable`. Consistent with
  `Changes`/`Diff`, but a real contract difference from the git delta.
- **Blob cost**: change sets carry full file contents, transiently — same
  caveat the diff plan documents.
- **Opaque dirs on both sides** → conflict in v1 (enumeration-based
  reconciliation is a follow-up).
- **Not transactional after the refusal point** — parity with the git
  version's clean-merge path; documented, not hidden.

## Validation

1. `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` green;
   `go test ./... && go test -race ./...` green.
2. Live suite against a real daemon including the `TestLive_ApplyDelta*`
   set; record the skip honestly if no daemon.
3. Straggler check: `grep -rn "mergeChanges\|BaseKey" --include=*.go .`
   shows no references outside this feature; no git-era names
   (`baseGitHead`, `delta_patch`) reappear.
4. Contract check: README names shell/`cat`/`ls`/`rm` and still claims no
   git for diff *and* merge.

## Out of scope

- Line-level text merge (separate plan; the decision recorded here is the
  prerequisite for ever doing it cleanly).
- Rename detection.
- Opaque-dir reconciliation via enumeration.
- Patch payloads or a `MaxDeltaBytes` knob (documented risk instead).
- Merge policy above the library — who may merge what is the broker's
  problem, not croft's.

## Landing notes (implementation deviations)

Recorded as landed in branch `phase3/snapshot-native-delta`; each is an
amendment by implementation, consistent with the roadmap's consolidation
rules:

- **Content comes from the blobs, not `ReadFilesAt`** (Task 3's plan text
  predates phase 2's final shape). Both legs parse with
  `parseChangeTarWithContent`, so theirs' and ours' post-images are the
  diff blobs' own entries: no reader task, no second read of anything, and
  the identical-bytes check compares blob-to-blob. The pre-fetch plan's
  property survives — all content is in hand before the first mutation.
- **`mergeChanges` takes the two content maps** as extra parameters; the
  plan's `([]Change, []Change)` signature cannot express the
  identical-bytes rule. Still pure, still table-tested first (Task 2's
  gate).
- **The cross-path conflict rule generalizes beyond deletes**: any rigid
  entry — a delete, or any non-directory change — conflicts with a
  non-delete change strictly underneath it on the other side. Delete-only
  missed the type-replacement collision (theirs turns `dir/` into a file
  while ours adds `dir/f`): the per-path walk cannot see it, and applying
  both would write through a path the other side made a file. Directories
  are the one flexible type; deletes underneath a rigid entry stay
  compatible (both sides removing).
- **Every applied write and symlink is preceded by `rm -rf` of its path**,
  not just type-changing ones: a diff blob cannot distinguish add from
  modify (the parser reports `ChangeModify` for all non-whiteout entries)
  and the delta runs no classification reader task, so the base's type
  under any written path is unknowable. `rm -rf` is uniformly correct and
  a no-op on absence. Directories instead use a conditional script
  (`[ -d ] || rm -rf`): an existing directory must survive — ours may hold
  kept children inside it.
- **The image contract gains `ln` alongside `rm`** (§1.4 named only `rm`):
  theirs' symlink entries apply as `rm -rf` + `ln -s`, with the target
  carried by the blob's `Linkname`. BusyBox and coreutils both carry it —
  the same argument the plan made for `rm`.
- **`writeFileImpl` formats modes via `effectivePerm`, not `Perm()`**:
  `Perm()` drops setuid/setgid/sticky, and a merge that applied a setuid
  file without its special bit would report a fresh diff against the very
  state it merged. Ordinary modes format identically.
- **No size guard** (Task 3's `maxDiffInlineBytes` reference): the constant
  never landed in phase 2, and the roadmap rules `MaxDeltaBytes` a
  documented risk until a host hits the ceiling. Documented in `planDelta`.
- **`TestLive_ApplyDeltaAfterBaseReleasedFails` uses the ghost-base key**:
  a released layer survives until the daemon's GC runs (and here it is also
  a chain parent), exactly the nondeterminism phase 2's unavailable-state
  test solved the same way. `ReleaseState` is still exercised.
