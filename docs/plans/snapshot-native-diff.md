# Snapshot-Native Diff Plan — croft

Replace the in-container git diff with a diff computed at the snapshotter
layer: the daemon's diff service compares the committed snapshot a state
refers to against the current active layer, so `Diff` (and a new `Changes`
primitive) work with **no git, no find, no stat in the image** — and work on
workspaces that were never git repositories at all. This finishes what the
lean plan starts: its image contract shrinks to "a shell and the file tools
croft's own I/O already needs".

## Part 0 — Baseline and dependency

**Assumes lean-clean-break.md has landed.** That plan deletes `Diff`,
`StateKeyGitHead`, `diffScript`, and the delta machinery, and reshapes
`WorkspaceState` to the typed struct `{SnapshotKey, ParentKey, Image}`. This
plan adds diff back as a snapshot-level operation on that lean surface. Do
not run the two plans concurrently; see Part 4 for the pre-lean variant.

Why not keep git diff: the image contract (croft.go `# The image contract`)
exists *only* for Diff/delta. Every host that wants snapshot states pays for
git it may never use, non-git workspaces silently degrade, and the diff is a
content comparison against a recorded `git_head` rather than against the
state's actual layer — meaningless for non-git workspaces by construction.

## Part 1 — Mechanism

### 1.1 The daemon-side diff service

containerd ships a diff service (gRPC `services/diff/v1`, wrapped by
`client.DiffService()`) whose `DiffMounts(lower, upper)` mounts both mount
sets **daemon-side** and walks the two trees, writing the change set as an
OCI layer blob into the content store. Key properties:

- Mount descriptors come from `s.snap.Mounts(ctx, key)` — the daemon mounts
  its own snapshot storage, so the *client* never mounts anything. Unprivileged
  and remote-daemon operation survive (the property `FIFODir` exists to
  protect; sandbox.go `FIFODir` doc).
- **Task 0 finding: committed snapshots cannot be mounted directly** —
  `Mounts()` refuses them ("not active or view: failed precondition"). The
  blob helper prepares a `View` of the state's layer, mounts that, and
  removes it cancel-free on the way out. Active snapshots (the upper side)
  mount directly.
- Mounting the **active** snapshot a second time while the task runs on it is
  a read-only overlay multi-mount — safe on overlayfs. Other snapshotters are
  out of scope (same caveat as commit semantics, sandbox.go `Snapshotter` doc).
- Non-mutation is trivial: the walk reads; nothing in the workspace changes.
  The non-mutating contract (croft.go `Sandbox.Diff` doc) carries over.

The returned blob is a tar of *complete changed files* (not deltas) plus
whiteout entries for deletions. It is unreferenced content: the daemon's GC
may collect it at any time unless leased, so every diff pins it under a
short-lived per-call lease and drops it on the way out (Part 3, Task 2).

### 1.2 Parsing the blob into a change list

- Added/modified: ordinary tar entries; path, mode, and size come from the
  header.
- **Task 0 finding: the blob is a gzipped OCI layer**
  (`application/vnd.oci.image.layer.v1.tar+gzip`), not a plain tar. The
  reader sniffs the gzip magic and decompresses, whatever media type the
  daemon chose. The blob is also **timestamp-nondeterministic** — the upper
  files' wall-clock mtimes ride in the headers, so identical change sets
  digest differently; nothing downstream (caching, identity) may key on it.
- Deleted: whiteout entries for files present in `lower` and absent in
  `upper` — so `Delete` is accurate without consulting the old tree.
  **Task 0 finding: the form is `.wh.`-prefixed regular files** (mode 0,
  size 0, mtime 0 — AUFS-style), not char devices. The parser accepts both
  forms defensively.
- Opaque directories (`rm -rf` + recreate): **Task 0 finding: the walking
  differ emits per-file whiteouts for the old contents**
  (`opaque-dir/.wh.a.txt`, `opaque-dir/.wh.b.txt`) plus the new files — no
  opaque marker appeared. The `.wh..wh..opq` marker and its reader-task
  enumeration are handled defensively but are not the expected path on this
  differ.
- **Task 0 findings on shape:** entries arrive prefixed with the
  in-container workdir (`workspace/...`) and in walk order, not sorted — the
  parser strips the `Config.Workdir` prefix, skips the workdir-root entry
  itself (its metadata is croft's provisioning, not user data), and sorts
  the change list by path. Mode-only changes are reported with unchanged
  content, so `Diff`'s content-identical filter compares content *and*
  mode. Symlink targets ride in the tar `Linkname` field. No PAX records
  were observed from this differ.
- **Known walker heuristic, resolved deliberately:** the walk compares mode +
  size + mtime, so `touch`-only changes report as Modified with identical
  content. `Changes` keeps walker truth and documents the caveat; `Diff`
  fetches both bodies anyway and filters content-identical entries before
  rendering, so its text and file list stay noise-free.

### 1.3 The reader task (pre-image access)

Rendering a unified text diff needs pre-images: file contents *as of the
state*. There is no "read a file from a snapshot" RPC, but croft already
knows how to run a throwaway task on any committed layer — that is what
`Apply` does. So: `Prepare` a reader snapshot from `ws.SnapshotKey`, start a
task, exec one `cat -- <path>` per file, stop, remove. Batched by caller,
one exec per file (robust over clever marker protocols; changed-file counts
in practice are small). This yields `ReadFilesAt`, which is independently
useful: inspect a checkpoint without applying it. Directory enumeration for
opaque dirs uses `ls -A1` in the same reader task.

Image contract consequence: the contract becomes `/bin/sh` + `cat` + `ls`
(coreutils or busybox — `cat` is already required by `ReadFile`,
sandbox.go:982). No git, no find, no stat.

### 1.4 Text rendering

The old `Diff` returned git-formatted text. Post-lean nothing consumes patch
format (delta machinery is gone), so the renderer targets plain `diff -u`
style: `---`/`+++` headers (`/dev/null` for adds/deletes), no timestamps,
3 context lines, `\ No newline at end of file`, hunks from a hand-rolled
Myers line diff (~150 LOC — README:106 keeps the zero-dependency rule).
Non-UTF8 or oversized bodies render as a `Binary files ... differ` line.
`maxLines` truncation keeps its existing semantics (sandbox.go:946-950,
`...[diff truncated]`).

## Part 2 — Target shape

```go
type ChangeKind int

const (
    ChangeAdd ChangeKind = iota
    ChangeModify
    ChangeDelete
)

type Change struct {
    Path  string      // workspace-relative, slash-separated; output sorted by Path
    Kind  ChangeKind
    IsDir bool
    Mode  fs.FileMode // new mode; 0 for ChangeDelete
    Size  int64       // new size in bytes; 0 for ChangeDelete
}

// Added to Sandbox (the lean interface is deliberately breaking-friendly):
//
// Changes reports the differences between ws and the current workspace.
// Non-mutating. Requires the overlayfs snapshotter; reports
// ErrStateUnavailable once ws's layer has been released.
Changes(ctx context.Context, ws WorkspaceState) ([]Change, error)

// Diff renders the change set as unified-diff text, content-identical
// modifies filtered. Same shape and maxLines semantics as the deleted
// git-era Diff, so hosts migrate mechanically.
Diff(ctx context.Context, ws WorkspaceState, maxLines int) (diff string, files []string, err error)

var ErrStateUnavailable = errors.New("croft: state is no longer available")

// Concrete method on ContainerdSandbox (not the interface — promote later
// if hosts need it through the interface, the StateReleaser precedent):
ReadFilesAt(ctx context.Context, ws WorkspaceState, paths []string) (map[string][]byte, error)
```

New collaborators: a `diffService` interface beside `taskService`
(sandbox.go:142), wired in `connect` (client.go:74) with namespace-wrapped
contexts like `containerdImages` (client.go:335), injectable via a
`WithDiffService` `BackendOption` mirroring `WithLeases` (sandbox.go:291).
`NewWithBackend` without one → `Changes`/`Diff` fail with a clear
"diff service not configured" error; `New` always wires the real one.

## Part 3 — Tasks (execution order; one task per commit, bisectable)

### Task 0 — Live spike (time-boxed, findings checked in)
Against a real containerd, confirm: (a) the exact v2 diff-service accessor
and `DiffMounts` signature/options (import paths from go.mod's containerd
version — `core/diff`, `api/services/diff/v1`); (b) `DiffMounts` with upper =
the active snapshot of a *running* task; (c) the produced tar format —
whiteout form (char device vs `.wh.` entry), opaque marker name, entry
ordering, dir/symlink/hardlink entries, pax headers; (d) leasing content
(resource type string for content blobs) and that an unleased blob is
GC-collectable; (e) the full unprivileged path (group-shared socket +
`FIFODir`). Deliverables: golden tars under `testdata/change-tar/` and a
findings note appended to this plan. Everything downstream keys off these
goldens, not off memory of containerd internals.

### Task 1 — `diffService` collaborator
- Declare the interface in sandbox.go beside `taskService`; adapt its shape
  to whatever Task 0 found.
- Wire it in `connect` (client.go:86-93 block) with namespace injection;
  add `WithDiffService` option; document the nil-collaborator error.
- Extend the test fake to a `fakeDiffService` that computes the change set
  from the fake snapshotter's in-memory trees and emits a tar in the golden
  format.

### Task 2 — Blob lifecycle
- `diffBlob(ctx, ws) (descriptor, drop func(), error)`: mount both sides
  (`Mounts` NotFound → wrap `ErrStateUnavailable`), `DiffMounts`, create
  per-call lease `croft-diff-<rand>` labeled `croft.lease: "diff"` (mirrors
  the existing label scheme, findable by a future reclaim sweep), add the
  content resource, return a cancel-free `drop` (DeleteResource +
  best-effort `content.Delete`).
- Pin hygiene is the house rule: `drop` runs on **every** failure path,
  cancel-free, exactly like WriteFile's patch cleanup (sandbox.go:748-750)
  and Capture's unpin-on-failure branches (sandbox.go:512, 536).
- Tests: drop-pairing on success, on `DiffMounts` failure, on parse failure,
  under cancelled contexts.

### Task 3 — Tar → `[]Change` parser
- `parseChangeTar(r io.Reader) ([]Change, []opaqueDir, error)`: streaming
  `archive/tar`; classify entries; whiteouts → `ChangeDelete` of the
  trimmed path; collect opaque dirs for Task 5; sort by path.
- Mode/size from headers; `IsDir` from entry type. Defensive: accept both
  whiteout forms Task 0 finds in the wild.
- Table tests driven by the Task 0 goldens.

### Task 4 — Reader task primitives
- Keys `croft/<id>/reader/<n>` from a dedicated `readSeq` counter — the
  capture `seq` must not move (state identity is sequence-derived,
  sandbox.go `Capture` contract).
- `readFilesAt`: Prepare → StartTask → one `cat -- <path>` exec per file →
  StopTask → `snap.Remove`, with Create's failure-cleanup shape
  (sandbox.go:357-361) and cancel-free teardown. A missing path fails the
  call (workspace raced; documented, retryable).
- `readDirAt(ws, dir)` via `ls -A1 --` for opaque-dir resolution.
- Interaction notes for the docs: the reader's container generation carries
  the sandbox label, so `StopTask`'s label sweep (client.go:582-611) reaps it
  on crash — and a concurrent `Capture`/`Apply` stops it mid-exec (error,
  retry). Post-lean locking makes `Changes`/`Diff` shared-lock holders, so
  exclusive-lock lifecycle ops serialize them away.
- Export `ReadFilesAt` on `ContainerdSandbox`; keep `readDirAt` unexported.

### Task 5 — `Changes` on the interface
- Validate `ws.SnapshotKey != ""`; `requireBackend`; read `id`/`activeKey`
  under `mu` (error "Changes before Create" when absent); resolve opaque
  dirs via `readDirAt`; emit the final sorted `[]Change`.
- Doc contract: ordering; mtime-noise caveat (§1.2); non-mutating;
  `ErrStateUnavailable` after `ReleaseState`; overlayfs-only; concurrent-exec
  consistency equal to the old git Diff (results as of some moment during
  the call).
- Unit tests over the fake (add/modify/delete/mode-change/type-change/
  opaque/empty — empty right after `Apply(ws)`); live tests:
  `TestLive_ChangesReportsAddModifyDelete`, `TestLive_ChangesHandlesOpaqueDir`,
  `TestLive_ChangesEmptyAfterApply`, `TestLive_ChangesAfterReleaseStateFails`,
  `TestLive_ChangesBlobLeavesNoContentResidue` (content store listing is
  empty after the call).

### Task 6 — `Diff` renderer (droppable milestone)
- `textdiff.go`: Myers line diff + unified renderer (context 3,
  `/dev/null` headers, no timestamps, `\ No newline` handling, binary/
  oversize line via `maxDiffInlineBytes` constant — promote to `Config`
  only if a host asks).
- Flow: `Changes` → partition → pre-images via `ReadFilesAt(ws, ...)`,
  post-images via one-exec-per-`cat` against the current task (plain
  `tasks.Exec`, no second container) → filter content-identical modifies →
  render → apply `maxLines` truncation verbatim from sandbox.go:946-950.
- Tests: golden fixtures under `testdata/unified/` (adds, deletes, modifies,
  mode-only-after-filter, binary, no-trailing-newline, truncation); a
  round-trip property test — applying each rendered hunk set to the
  pre-images must reproduce the post-images; the deleted-in-lean
  `TestLive_DiffPreservesPreStagedChanges` concern does not apply (nothing
  in the container is touched).

### Task 7 — Contract and docs
- README: image contract → "`/bin/sh`, `cat`, `ls`"; drop the git/degrade
  language; note `Changes` in the capability list; testing section mentions
  the new live coverage.
- `docs/overview.md`: add the diff service to the architecture diagram's
  daemon box; add `Changes`/`Diff` to the lifecycle verbs and a short
  "Inspecting a state" subsection covering `ReadFilesAt`; the Capture
  sequence's "record git HEAD (+ delta patch…)" step disappears in the lean
  rewrite this plan builds on — keep the diagram aligned with whatever lean
  landed.
- `docs/verification.md`: add change-set semantics (whiteouts, opaque dirs)
  and blob-cleanup to the live-suite list.
- `CHANGELOG.md`: single entry — Added (`Changes`, `ReadFilesAt`, `Diff`
  text renderer), Breaking (none beyond lean's own), the contract shrink.

## Part 4 — Pre-lean variant (only if executed before lean-clean-break)

- `Diff` is **replaced in place** (same signature, sandbox.go:917); `Changes`
  and `ReadFilesAt` are added alongside.
- State access: `stateSnapshotKey(ws)` (sandbox.go:1214) instead of
  `ws.SnapshotKey`.
- Delete only the Diff-side git machinery (`diffScript`, `diffNamesMarker`,
  `gitHead`, `StateKeyGitHead` reads — sandbox.go:59, 784-817, 921-950).
  **The delta machinery keeps git**: `deltaPatchScript`/`applyDeltaScript`
  stay, so the image contract does NOT shrink in this variant.
- Sequence strictly after lean lands to avoid two rewrites of the same
  function; this variant exists only so the plan is not blocked if lean
  slips.

## Risks

- **Format drift**: the blob's tar conventions (whiteout form, opaque
  marker, ordering) are containerd-internal. Task 0's goldens pin what we
  consume; the parser accepts both whiteout forms defensively; upgrade
  testing = re-run the live suite on containerd bumps.
- **Walker mtime heuristic**: `touch` noise in `Changes` is documented
  behavior; `Diff` filters it by content compare (bodies already fetched).
  Git-era semantics differed here; CHANGELOG says so.
- **Running-task multi-mount**: safe read-only on overlayfs, unverified
  elsewhere — same scope claim as the snapshotter commit-semantics caveat.
- **Blob GC race**: mitigated by the per-call lease; the failure-path drop
  pairing is directly tested (Task 2).
- **Concurrent lifecycle ops** kill the reader task mid-call: post-lean
  shared/exclusive locking serializes; pre-lean it is a documented
  retryable race (the old git Diff had the same exposure).
- **Blob disk cost**: the change set carries full file contents; a huge
  artifacts directory pays for it transiently. Document; a `MaxDiffBytes`
  knob is a follow-up only if a host hits it.
- **Reader task needs `cat`/`ls`**: effectively universal (busybox has
  both); the contract section states it rather than assuming it silently.

## Validation

1. `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` green;
   `go test ./... && go test -race ./...` green.
2. Live suite against a real daemon (README env-gated invocation) including
   the new `TestLive_Changes*` set; record the skip honestly if no daemon.
3. Straggler check: `grep -rn "diffScript\|diffNamesMarker\|StateKeyGitHead\|gitHead" --include=*.go .`
   returns nothing (post-lean baseline).
4. Contract check: README image-contract section names only shell/cat/ls;
   no "git" requirement remains for Diff.
5. Renderer: golden fixtures green; round-trip property test green.

## Out of scope

- Streaming or incremental change APIs (watch-style); `Changes` is a
  point-in-time report like the git Diff it replaces.
- Non-overlayfs snapshotters.
- A `MaxDiffBytes` budget knob (documented risk instead).
- Batched reader execs (one `cat` per file is the deliberate v1).
- Patch-apply primitives (the delta machinery is gone; nothing consumes
  patch format).
- The orphan-reclaim sweep that would consume the `croft.lease: "diff"`
  label — separate plan.
