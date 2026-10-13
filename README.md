# croft

A croft is a small enclosed plot of land one party works on its own. This
library gives a program the same thing: an isolated, snapshot-backed
workspace on [containerd](https://containerd.io)'s overlayfs snapshotter —
provisioned from an image, worked through exec and file I/O, and checkpointed
into states that outlive the sandbox that captured them.

Croft is Linux-only (OverlayFS, a containerd daemon, root or user namespaces)
and is deliberately not a container runtime: it is the substrate a host uses
when it wants a real filesystem boundary per workspace, with commands executed
in-band inside that boundary.

## What it does

```
Create       resolve the image rootfs, prepare the base snapshot, start the task
Exec,
ReadFile,
WriteFile    work the workspace through the boundary
Capture      commit the active layer, pin it as a state, continue on a fresh layer
Apply        resume this (or any) sandbox from a captured state
ReleaseState drop a state's pin; the daemon's GC reclaims the layer
Destroy      stop the task, remove the active layer; committed states survive
```

Capabilities, honestly:

- **Layered** — every Capture commits an immutable overlayfs layer and continues
  on a new one; lineage is recorded.
- **Rollback** — Apply rebinds a sandbox to any captured state, including from
  a fresh process after a crash (the resume path).
- **State sharing** — a captured state can be applied by a different, equally
  configured sandbox; that is the fork primitive. Note there is no fork
  *orchestration* here: Capture on the parent, Apply on the child is the whole
  recipe.
- **Merge without reset** — `DeltaApplier` (optional interface) merges a
  captured delta onto a workspace that moved past the delta's base, refusing
  conflicts atomically.

## Quick start

```go
sb := croft.New(croft.Config{
    Namespace: "croft",
    Image:     "docker.io/library/golang:1.26",
})
if err := sb.Create(ctx, croft.SandboxSpec{}); err != nil { ... }
defer sb.Destroy(ctx)

res, _ := sb.Exec(ctx, croft.ExecRequest{Command: "go", Args: []string{"version"}})
state, _ := sb.Capture(ctx)   // pinned against the daemon's GC
_ = sb.Apply(ctx, state)      // resume from it
if r, ok := sb.(croft.StateReleaser); ok { r.ReleaseState(ctx, state) }
```

## The image contract

`Diff` and `ApplyDelta` run git inside the container, so the image must provide
`sh`, `stat`, GNU `find`, and `git` (a typical host also probes for whatever its
workload needs). A workspace that is not a git repository degrades to
snapshot-only states: Capture and Apply still work; Diff and ApplyDelta have
nothing semantic to say.

## Configuration

`croft.Config` carries the containerd address, namespace, image, runtime,
snapshotter, init command, in-container workdir, write chunking, and the
client-side FIFO directory (set the last to a user-writable path to drive
containerd without root). See `Config` in `croft.go` for the full contract of
each field.

## Compatibility and provenance

Croft was extracted from the `sandbox/containerd` nested module of
[shepherd-kernel-go](https://github.com/buchenberg/shepherd-kernel-go) (its
v0.1.x–v0.2.x history is preserved here) and now defines its own interface,
types, and state format. It is a clean break: croft uses its own snapshot key
prefix (`croft/…`), lease and container labels (`croft.*`), states lease
(`croft-states`), and digest version (`croft.state.canonical.v1`). States,
leases, and snapshots created by the old module are not interchangeable with
croft's, and its digests are not comparable with other ecosystems'
workspace-state formats. `WorkspaceState` data keys (`snapshot_key`,
`parent_key`, `image`, `git_head`, `delta_base`, `delta_patch`) and the
backend identifier (`"containerd"`) are unchanged in shape and meaning.

Known gap, inherited and recorded: a state's identity is its sequence-derived
snapshot key, so two captures of identical content produce different digests.
Digest equality means identity, not content equality; a content-derived state
identity is a planned follow-up.

## Testing

```sh
go test ./...          # unit suite; no daemon required

# live suite, against a real containerd:
sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
     CROFT_CONTAINERD_IMAGE=docker.io/library/debian:bookworm \
     go test -count=1 -v -run TestLive_ ./...
```

Croft has no dependencies beyond containerd and the Go standard library.

## License

MIT — see [LICENSE](LICENSE).
