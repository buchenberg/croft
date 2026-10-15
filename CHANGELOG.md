# Changelog

## v0.6.0 (unreleased)

- Added: `Config.Network` — the workspace's network posture, with the
  default now documented as what it has always been: containerd's default
  spec unshares the network namespace, so croft containers run
  loopback-only — no egress, no ingress, DNS fails fast. `""`/`"none"` keep
  that posture; `"host"` is the explicit opt-in that removes the unshare so
  the container shares the daemon host's network. Unknown values are
  rejected loudly at `Create` and `Apply` alike.
- Corrected: the phase plan and roadmap decision D6 assumed today's default
  was host networking; the premise was inverted. The ruling (2026-10-15)
  keeps the default unchanged in fact — isolated — rather than silently
  granting every existing sandbox host network access.
- Live-pinned: the `lo`-only listing under `none` and the identical default;
  the host's own interface listing under `host`.

## v0.5.0

- Added: `Config.Limits` — CPU shares/quota, memory, and pids caps, threaded
  into the OCI spec of every container generation a sandbox starts (init and
  reader tasks alike). Nil keeps the unbounded default with an identical
  spec (roadmap decision D2, the broker's fairness floor).
- Added: `ExecRequest.Stdin` is honored — stdin as data, staged to a temp
  file inside the container and redirected into the process, which sees EOF.
  The shim's stdin FIFO, which never signals EOF, is bypassed entirely.
- Added: `Config.MaxOutputBytes` — a per-stream cap on public Exec
  stdout/stderr (data reads — `ReadFile`, the diff reader tasks — stay
  lossless). Over-cap streams keep their first and last half around a
  `[... N bytes truncated ...]` marker; zero (the default) is unbounded.
- Documented and live-pinned: the background-process recipe — `nohup … &`
  work survives its launching exec and is polled through its log file; it
  dies at `Capture`, the stated process-state cost of a coherent snapshot.
- Breaking: `ErrStdinUnsupported` removed — a non-empty `Stdin` no longer
  fails.
- Scope boundaries recorded: interactive exec sessions are deferred (stdin
  is data delivered once; output is bounded-but-buffered, not streamed), and
  process-preserving capture is a permanent boundary (CRIU-class
  checkpointing of arbitrary trees is experimental and fragile).

## v0.4.0

The roadmap's phases 1–3 ship as one release (decision D1): the lean core,
the snapshot-native diff, and the snapshot-native delta.

- Diff is now computed at the snapshotter layer: `Changes` and `Diff` compare
  a captured state against the current workspace through the daemon's diff
  service, and `ReadFilesAt` reads pre-images from a state without applying
  it. The git dependency is gone.
- `ApplyDelta` is a tree-level snapshot merge: a three-way merge of two
  daemon-side diffs against the state's recorded merge base (`BaseKey`),
  refusing atomically with `ErrDeltaConflict`. Divergent same-file edits
  conflict; line-level merge is a scoped follow-up.
- `WorkspaceState` is a typed four-field record (`SnapshotKey`, `ParentKey`,
  `BaseKey`, `Image`), replacing the `Data`-map envelope and the `StateKey*`
  constants.
- The `Sandbox` interface gains `Changes`, `Diff`, and `ApplyDelta` —
  source-breaking for external implementers, deliberately.
- The image contract is now `sh`, `cat`, `mkdir`, `dirname`, `printf`,
  `base64`, `chmod`, `ls`, `readlink`, `rm`, `ln`; git leaves the required
  set.

## v0.3.0

First release of croft as an independent project, extracted from the
`sandbox/containerd` nested module of shepherd-kernel-go (whose v0.1.x–v0.2.x
history this repository preserves).

- The module moves from
  `github.com/buchenberg/shepherd-kernel-go/sandbox/containerd` to
  `github.com/buchenberg/croft`, and the package from `containerd` to `croft`.
- Croft now defines its own interface and types: `croft.Sandbox`,
  `SandboxCapabilities`, `Containment`, `SandboxSpec`, `ExecRequest` /
  `ExecResult`, `WorkspaceState` (with its digest), `DeltaApplier`, and
  `StateReleaser`. The package has no dependency on shepherd-kernel-go, in
  code or in `go.mod`.
- Clean break from the old module: croft's own snapshot key prefix (`croft/`),
  lease and container labels (`croft.*`), states lease (`croft-states`), and
  digest version (`croft.state.canonical.v1`). States, leases, and snapshots
  from the old module are not interchangeable. `WorkspaceState` data keys and
  the backend identifier (`"containerd"`) are unchanged in shape and meaning.
- `ForkState` is removed: it was an uncalled alias for `Capture`, and the fork
  primitive is Capture on the parent plus Apply on the child.
- The backend registry (`registry.go`, registering into shepherd-kernel-go's
  `sandbox.OpenSandbox`) is removed; construct sandboxes with `croft.New`.
- The live test suite's environment variables rename from
  `SHEPHERD_CONTAINERD_*` to `CROFT_CONTAINERD_*`.
- The kernel-materialize live integration test moved out with the shepherd
  dependency; it belongs in the consumer's test suite now.

Behavior is otherwise unchanged from sandbox/containerd v0.2.2.
