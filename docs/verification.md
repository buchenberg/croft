# Verification

Croft's correctness is pinned on two layers, and the live one matters because
the fakes cannot reach everything a real daemon does:

- **Unit suite** — runs against containerd's real `snapshots.Snapshotter`
  interface using an in-memory fake that enforces the overlayfs invariants: a
  key cannot be prepared twice, a parent must be committed, only an active
  layer can be committed (exactly once — an already committed layer is
  rejected), a committed layer is read-only, and a layer with children cannot
  be removed. Concurrency runs under `-race`.
- **Live suite** — the whole lifecycle against a real daemon, env-gated so
  `go test ./...` stays green without one.

## What the live suite verifies

- **Full lifecycle.** Create from a public image, write, exec, capture, apply
  (rollback), destroy — the acceptance sequence, on a real daemon, with
  process state lost at Capture stated and observed.
- **State durability.** Every Capture pins its committed layer under the
  namespace-scoped states lease that outlives any sandbox, Destroy removes
  only the sandbox's private active layer, and `ReleaseState` is the
  deliberate reclaim path — disk comes back when the caller says so, not when
  a sandbox happens to die.
- **The recycle path.** Capture → destroy → apply, with the pre-capture write
  present in the re-provisioned workspace — states outlive sandboxes, proven
  end to end (`TestLive_ApplyAfterDestroyRecyclesState`).
- **Leases.** The sandbox lease holds snapshots against the daemon's garbage
  collector across the whole lifecycle; namespace isolation is asserted
  against the daemon itself (the fakes have no namespaces).
- **Exec semantics.** Streams are separated, exit codes surface without
  errors, duration is populated, and `ExecRequest.Stdin` is delivered as
  data — staged to a temp file and redirected, so the process sees EOF and
  no residue survives (`TestLive_ExecStdinIsData`). Over-cap streams keep
  their head and tail around a truncation marker, reconstructed
  byte-for-byte against the expected rendering
  (`TestLive_ExecOutputCapTruncatesMiddle`).
- **The background-process recipe.** A `nohup … &` workload survives its
  launching exec and keeps producing across later client traffic
  (`TestLive_BackgroundProcessSurvivesExec`), and dies at `Capture` while
  its file output persists in the snapshot — the boundary pinned as
  honestly as the recipe
  (`TestLive_BackgroundProcessDiesAtCapture`).
- **Resource limits.** `Config.Limits` reaches the real cgroup: under a
  cgroup-v2 daemon, `memory.max`, `pids.max`, and `cpu.max` inside the
  container show exactly the configured values, and `cpu.weight` carries
  runc's shares conversion (`TestLive_LimitsReachTheCgroup`, which skips
  honestly on a host with no v2 view).
- **Network posture.** The default (`""`) and `"none"` show exactly `lo` in
  the container's `/sys/class/net` — the private netns croft has always
  shipped — and `"host"` shows the daemon host's own interface listing,
  compared against the host's
  (`TestLive_NetworkNoneIsLoopbackOnly`,
  `TestLive_NetworkHostSeesInterfaces`).
- **Snapshot-native diff.** `Changes` reports the full change matrix (add,
  modify, delete, mode-only, new directory, opaque directory, symlink)
  through the production API against a real daemon-side diff service;
  `Diff` renders a unified text diff; `ReadFilesAt` reads pre-images from a
  captured state; and a released state surfaces `ErrStateUnavailable`
  (`TestLive_Changes*`, `TestLive_Diff*`, `TestLive_ReadFilesAt`).
- **Snapshot-native delta.** `ApplyDelta` merges two sandboxes' divergent
  work over a shared base — add, delete, identical edit, and ours-kept
  classes — refuses a divergent same-file edit atomically (the workspace
  stays bit-identical), applies a symlink retarget and a new directory on a
  workspace that was never a git repository, maps a released base to
  `ErrStateUnavailable`, and leaves no diff lease or diffview snapshot
  behind (`TestLive_ApplyDelta*`).

## Running the live suite

```bash
sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
  go test -count=1 -v -run TestLive ./...
```

A stock image and the overlayfs snapshotter suffice. The suite runs as root
(the daemon socket usually requires it); the tests set `FIFODir` to a temp
directory because the default is a root-owned path the client writes to
locally — see "Unprivileged operation" for driving the daemon without root.

## Unprivileged operation

Any user sharing only the containerd socket group can start sandboxes: the
init spec is built from the image config blob read through the daemon's
content store (env, working directory, numeric `uid[:gid]`), never from the
client process's filesystem, so the daemon's 0700-root snapshot directories
are never read, and `Config.FIFODir` gives the client's stdio FIFOs a
user-writable directory. This is a design property of the daemon adapter,
exercised by its construction paths — not something the live suite asserts,
because that suite runs as root.

## Honesty notes

- Unsupported operations fail loudly rather than pretending.
- "Runs in a container" is not a security claim on its own: containment is
  namespaces plus a separate rootfs, strong but not a VM boundary.
