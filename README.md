# croft

A croft is a small enclosed plot of land one party works on its own. This
library gives a program the same thing: an isolated, snapshot-backed workspace
on [containerd](https://containerd.io)'s overlayfs snapshotter — provisioned
from an image, worked through exec and file I/O, and checkpointed into states
that outlive the sandbox that captured them.

Croft is Linux-only (OverlayFS, a containerd daemon, root or user namespaces)
and is deliberately not a container runtime: it is the library a host uses
when it wants a real filesystem boundary per workspace, with cheap
checkpoints, rollback, and forks over that boundary.

## What it does

```
Create       resolve the image rootfs, prepare the base snapshot, start the task
Exec,
ReadFile,
WriteFile    work the workspace through the boundary
Capture      commit the active layer, pin it as a state, continue on a fresh layer
Apply        resume this (or any) sandbox from a captured state
Changes      list the differences between a state and the current workspace
Diff         render those differences as a unified text diff
ApplyDelta   merge a state's changes onto the current workspace (tree-level)
ReleaseState drop a state's pin; the daemon's GC reclaims the layer
Destroy      stop the task, remove the active layer; committed states survive
```

The properties, honestly:

- **Layered** — every Capture commits an immutable overlayfs layer and
  continues on a new one; lineage is recorded in the state.
- **States outlive sandboxes** — a captured state survives Destroy, crashes,
  and process restarts, and any equally configured sandbox can resume it.
- **Rollback** — Apply rebinds a sandbox to any captured state, exactly and
  O(1) (a re-point, not a replay).
- **Fork** — there is no fork orchestration; Capture on the parent plus Apply
  on the child is the whole recipe, at layer-sharing cost.
- **Serialized** — lifecycle operations are exclusive, workspace operations
  shared: an in-flight Exec finishes before a Capture commits the layer it is
  writing to, by construction.
- **A state's identity is its snapshot key** — sequence-derived, so two
  captures of identical content are distinct states. Identity, not content
  equality; croft has no digest, and a host that wants a record identity
  hashes the four-field struct itself.
- **Network-isolated by default** — the container gets its own network
  namespace with only loopback: no egress, no ingress, DNS fails fast.
  `Config.Network: "host"` is the explicit opt-in that shares the daemon
  host's network instead — package installs, git remotes, and everything
  else the host can reach (the namespace is shared, not the resolver:
  `/etc/resolv.conf` stays the image's). Untrusted code keeps the default;
  trusted workloads opt in.

## Diffing states

Two states can be compared without applying either. `Changes` returns the
sorted difference between a captured state and the current workspace as a
`[]Change` (add / modify / delete, with mode and size); `Diff` renders that
difference as a unified text diff — `---`/`+++` headers, `@@` hunks, binary
files as "Binary files … differ", mode-only changes as `old mode`/`new mode`
pairs, symlinks as target lines. Both are read-only: they never stop or
restart the task, never commit, never write.

```go
state, _ := sb.Capture(ctx)
// ... work happens ...
changes, _ := sb.Changes(ctx, state)  // what differs?
text, _ := sb.Diff(ctx, state)        // unified diff, for display
files, _ := sb.ReadFilesAt(ctx, state, []string{"config.yaml"}) // pre-images
```

`ReadFilesAt` is a concrete method on `ContainerdSandbox` (not on the
`Sandbox` interface): it reads files from a state's committed layer without
applying it, via a throwaway reader task. A state whose layer has been
released makes `Changes`, `Diff`, and `ReadFilesAt` return
`ErrStateUnavailable`.

## Merging states

`ApplyDelta` merges a state's changes into the current workspace without
rebinding to it — the git-era delta's successor, with no git anywhere. It is a
tree-level three-way merge: two daemon-side diffs against the state's recorded
merge base (`BaseKey`, the layer its capturing sandbox last Applied), one from
the base to the incoming state ("theirs") and one from the base to the current
workspace ("ours"). Theirs-only changes apply, ours-only changes are kept,
edits both sides made identically are kept once, and everything else —
divergent edits to the same file, mode disagreement, modify-vs-delete, or one
side deleting a subtree the other wrote into — is a conflict:
`ErrDeltaConflict` names the offending paths and the workspace is completely
untouched. The refusal is atomic; a merge that starts applying is not
transactional, matching the git-era version's clean path.

```go
// sandbox B worked on from the same base as A:
theirs, _ := sbB.Capture(ctx)      // theirs.BaseKey = the shared base
err := sbA.ApplyDelta(ctx, theirs) // merge B's work into A's diverged tree
```

Granularity is per-file, not per-line: two divergent edits to the same file
conflict even when they touch different lines (line-level merge is a scoped
follow-up). The merge needs the base layer still pinned — a released base
surfaces as `ErrStateUnavailable`, and a state captured from a sandbox that
never Applied has an empty `BaseKey` and cannot be merged.

## Quick start

```go
sb := croft.New(croft.Config{
    Namespace: "croft",
    Image:     "docker.io/library/golang:1.26",
})
if err := sb.Create(ctx); err != nil { ... }   // the caller's ctx bounds it
defer sb.Destroy(ctx)

res, _ := sb.Exec(ctx, croft.ExecRequest{Command: "go", Args: []string{"version"}})
state, _ := sb.Capture(ctx)   // pinned against the daemon's GC
_ = sb.Apply(ctx, state)      // resume from it — rollback, crash-resume, fork
_ = sb.ReleaseState(ctx, state) // disk comes back when the caller says so
```

## Exec I/O

`ExecRequest.Stdin` is honored as data: the bytes are staged to a temp file
in the container (chunked like `WriteFile`, mode 0600, removed on every
outcome path) and redirected into the process, which sees EOF when they run
out — the shim's stdin FIFO, which never signals EOF, is bypassed entirely.
The staged payload inherits `WriteFile`'s exposure: base64 in argv, visible
in `/proc/*/cmdline` while staging runs — stdin-shaped data, never secrets.
Output is bounded-but-buffered: `Config.MaxOutputBytes` caps stdout and
stderr independently, keeping the first and last half of the budget around a
`[... N bytes truncated ...]` marker (zero, the default, is unbounded). Two
deliberate boundaries: interactive exec sessions — writing to a running
process's stdin, live-tailing its output — are deferred until a consumer
needs a TTY, and process-preserving `Capture` is permanently out of scope
(CRIU-class checkpointing of arbitrary process trees is experimental and
fragile).

## Long-running processes

Exec waits for its command to exit — and a command that backgrounds its work
returns immediately. That is the whole recipe for dev servers, test watchers,
and daemons:

```go
_, _ = sb.Exec(ctx, croft.ExecRequest{
    Command: "/bin/sh",
    Args:    []string{"-c", `nohup ./dev-server > /tmp/server.log 2>&1 &`},
})
// The exec returns at once; the child is reparented to the container's init
// and keeps running across subsequent Execs. Poll its log like any file:
log, _ := sb.ReadFile(ctx, "/tmp/server.log")
```

The boundary, plainly: **background processes die at Capture**. A coherent
snapshot stops the task, commits the layer, and continues on a fresh one —
files persist, processes do not. A process-preserving capture would need
CRIU-class checkpointing of an arbitrary process tree and is a permanent
scope boundary, not a gap awaiting a fix; restart the server after Capture if
you want it running (the workspace, including its log, is intact).
Interactive sessions are out of scope by decision as well: stdin is data
delivered once (`ExecRequest.Stdin`), and output is bounded-but-buffered
(`Config.MaxOutputBytes`), not streamed.

## The image contract

The workspace's file operations shell out inside the container, so the image
must provide a shell (`/bin/sh` — also the keep-alive command's default) and
the small coreutils croft itself uses: `mkdir` (Create provisions the
workdir), and `cat`, `dirname`, `printf`, `base64`, and `chmod` (ReadFile and
WriteFile). The diff machinery adds `ls` and `readlink` (reader tasks read
pre-images, enumerate directories, and resolve symlink targets from a
captured state); the delta machinery adds `rm` and `ln` (ApplyDelta clears
replaced and deleted paths and recreates symlinks). BusyBox and coreutils
both carry all of them.

## Scale envelope

Workspaces are source-tree-sized. `ReadFile` and `WriteFile` buffer whole
files; `WriteFile` costs roughly one container exec per 512 KiB (configurable
via `WriteChunkBytes`) and carries its payload base64-encoded in exec
arguments, visible in `/proc/*/cmdline` while the exec runs — so large
artifacts are out of the design scope and secrets never travel through
`WriteFile` (or through `ExecRequest.Stdin`, which stages through the same
path). Exec output is bounded-but-buffered: `Config.MaxOutputBytes` caps each
stream in memory, keeping head and tail around a truncation marker; live
streaming is a scoped follow-up.

## Configuration

`croft.Config` carries the containerd address, namespace, image, runtime,
snapshotter, init command, in-container workdir, write chunking, the
client-side FIFO directory (set it to a user-writable path to drive
containerd without root), resource limits (`Config.Limits`: CPU
shares/quota, memory, and pids caps applied to every container generation
the sandbox starts; nil means unbounded), and the network posture
(`Config.Network`: the default keeps the container's own loopback-only
namespace; `"host"` opts into the daemon host's network). See `Config` in
`sandbox.go` for the full contract of each field.

## State format

`WorkspaceState` is a plain four-field record — `SnapshotKey`, `ParentKey`,
`BaseKey` (the last applied layer the state's changes are relative to: the
delta's merge base), `Image` — so it can be stored, logged, and handed back by
value. Daemon-visible identifiers: snapshot keys use the `croft/` prefix;
leases and containers are labeled `croft.lease` and `croft.sandbox`; the states
lease is `croft-states`.

## Testing

```sh
go test ./...          # unit suite; no daemon required

# live suite, against a real containerd (root is required for the socket):
sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
  go test -count=1 -v -run TestLive_ ./...
```

Croft's direct dependencies beyond the Go standard library are
`github.com/containerd/containerd/v2` (the daemon client),
`github.com/containerd/errdefs` (canonical daemon error kinds),
`github.com/sergi/go-diff` (the Myers diff engine), and the
`github.com/opencontainers/*` spec packages (`go-digest`, `image-spec`,
`runtime-spec`). What
the live suite verifies — and how to run it against a real daemon — is in
[docs/verification.md](docs/verification.md); how the pieces fit together is
in [docs/overview.md](docs/overview.md).

## License

MIT — see [LICENSE](LICENSE).
