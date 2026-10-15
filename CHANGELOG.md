# Changelog

## v0.5.0 (unreleased)

- Added: `Config.Limits` — CPU shares/quota, memory, and pids caps, threaded
  into the OCI spec of every container generation a sandbox starts (init and
  reader tasks alike). Nil keeps the unbounded default with an identical
  spec (roadmap decision D2, the broker's fairness floor).

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
