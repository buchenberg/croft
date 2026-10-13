# Verification

Croft's correctness is pinned on two layers, and the live one matters because
the fakes cannot reach everything a real daemon does:

- **Unit suite** — runs against containerd's real `snapshots.Snapshotter`
  interface using an in-memory fake that enforces the overlayfs invariants: a
  key cannot be prepared twice, a parent must be committed, only an active
  layer can be committed (exactly once — an already committed layer is
  rejected), a committed layer is read-only, and a layer with children
  cannot be removed.
- **Live suite** — the whole lifecycle against a real daemon, env-gated so
  `go test ./...` stays green without one.

## What the live suite verifies

- **Delta merges.** `ApplyDelta` merges a captured delta onto a workspace
  that moved past the delta's base: `Apply` records the applied state's git
  HEAD as the sandbox's delta base, every `Capture` carries a git binary
  patch from that base to the captured tree, and the merge runs at the tree
  level (`git merge-tree --write-tree`) in the object database — a
  conflicting delta refuses with the working tree untouched, and a clean
  one lands as unstaged changes that leave the caller's staged work alone.
- **State durability.** Every `Capture` pins its committed layer under the
  namespace-scoped states lease that outlives any sandbox, `Destroy` removes
  only the sandbox's private active layer, and `ReleaseState` is the
  deliberate reclaim path — disk comes back when the caller says so, not
  when a sandbox happens to die. The recycle path — capture, destroy, then
  apply the captured state, and the pre-capture write is present in the
  re-provisioned workspace — is pinned live
  (`TestLive_ApplyAfterDestroyRecyclesState`).
- **Leases and non-mutation.** Leases hold snapshots against the daemon's
  garbage collector, and Diff never mutates the caller's git index.

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

- Unsupported operations return `ErrUnsupported` rather than pretending;
  `Capabilities` reports the support matrix.
- "Runs in a container" is not a security claim on its own: containment is
  namespaces plus a separate rootfs, strong but not a VM boundary.
