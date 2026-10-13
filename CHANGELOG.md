# Changelog

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
