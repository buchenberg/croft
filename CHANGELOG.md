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
  `StateReleaser`. The public API no longer mentions or depends on the
  shepherd-kernel-go sandbox or trace packages.
- `WorkspaceState.Digest` is byte-identical to the previous implementation, and
  all state data keys are unchanged, so states captured by either library are
  interchangeable.
- Daemon-visible identifiers (snapshot key prefix, lease labels, states lease
  id, container label) are inherited verbatim for cross-version compatibility;
  see the README's compatibility section.
- `ForkState` is removed: it was an uncalled alias for `Capture`, and the fork
  primitive is Capture on the parent plus Apply on the child.
- The backend registry (`registry.go`, registering into shepherd-kernel-go's
  `sandbox.OpenSandbox`) is removed; construct sandboxes with `croft.New`.
- The live test suite's environment variables rename from
  `SHEPHERD_CONTAINERD_*` to `CROFT_CONTAINERD_*`.

Behavior is otherwise unchanged from sandbox/containerd v0.2.2.
