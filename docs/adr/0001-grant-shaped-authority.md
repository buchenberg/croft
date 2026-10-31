# ADR 0001 — Grant-shaped authority in the container tier

- **Status:** Accepted
- **Date:** 2026-10-28
- **Decides:** croft-ku0 (architecture / design-decision)
- **Scope:** goatherd-croft, goatherd-runtime `sandbox/croft` adapter, yaah container worlds
- **Related:** goatherd-runtime bead shepherd-runtime-go-78r (item 4),
  yaah bead yaah-kit (container-lane seam work), croft-shepherd-fit.md §6/§8,
  yaah-shepherd-gap-report §5.2 items 1–2

## Context

The authority vocabulary shared by all containment tiers is
`confinement.ConfinementSpec`: writable roots plus a network posture
(`allow_all` / `deny_all` / `broker`), resolved by
`confinement.ResolveSpec` and enforced fail-closed by a
`containment.Backend` (`probe` before `launch`, never run unconfined).

The syscall tier (Landlock / Seatbelt / winacl) already lowers this
completely: writable roots become filesystem-access rules, `deny_all`
and `broker` become real network postures.

The container tier does not. `sandbox/croft/exec.go` refuses every
authority request (`refuseUnsupportedAuthority`): the container rootfs is
wholly writable and network is only `""`/`none` (loopback-only) or
`host`. The single writable overlay layer is the whole tier's
change-set/capture model. yaah already speaks per-binding grants
(`agents.subagent.grants`, per-role overrides) and refuses to lower them
onto container worlds (`world.go`: *"per-binding grants are not lowered
onto container worlds yet"*). croft-ku0 framed the choice as:

- **Option A — shaped authority:** read-only rootfs plus explicit
  writable mounts; also grow `broker` egress. Cost: mount ordering and
  proxy lifecycle; capture plumbing feared to have to gather several
  bind-mounted roots instead of one writable layer.
- **Option B — coarse but honest:** keep the binary boundary, document
  the posture mapping, refuse `broker` loudly, authority stays
  whole-workspace `ReadWrite` in this tier.

Since then both croft and goatherd have been declared greenfield: the
substrate can be redesigned, so Option A's cost is an engineering cost,
not a constraint.

## Decision

**The container tier speaks the same grant-shaped authority as the
syscall tier.** Option A is accepted, implemented as *read-only masking
over the single writable layer* (A′) rather than as separate writable
stores — which removes Option A's capture-plumbing cost entirely.

### Writable roots as mount shaping (A′)

The workspace keeps one writable overlay layer. Shaping is done with
bind-mount views *aliasing that same layer*:

1. Bind-mount the workspace root over itself and remount that view
   read-only. This does not set the overlay superblock read-only — it
   sets only that vfsmount's flags (the same mechanism behind Docker's
   `:ro` binds).
2. For each granted writable root, bind-mount that subtree over itself
   and remount that view read-write.

Writability of a path is decided by the deepest mount covering it
(granted roots are subtree grants, matching the syscall tier's
writable-roots semantics; yaah already requires grants to be disjoint,
and deepest-wins is defined for overlaps anyway). Writes through a
read-write view land in the overlay upper dir exactly as before —
`Capture`/`Compare` are **unchanged**, the change-set is still one
writable layer, and it naturally contains only granted-root changes
because everything else is `EROFS` at the VFS.

The shaping is expressed as OCI mounts in the task's process spec when
the runtime honors ordered parent-before-child mounts (deepest mounts
last, so specificity wins); otherwise croft performs the same shaping
inside the container's mount namespace in an init step before the task
starts. If neither path is available, the launch **refuses** — the same
probe-first fail-closed contract as `containment.LaunchConfined`. The
The shaping init is the one privileged process: it receives `CAP_SYS_ADMIN` to perform the bind/remount step. User exec processes are given the default capability set **without** `CAP_SYS_ADMIN`, so the masks are not remountable from the model's processes.

Per-root writable mounts with separate backing stores (the naive Option
A) are explicitly **not** used: they would scatter the change-set across
several roots and force `Capture`/`Compare` to aggregate and filter, the
problem the croft-ku0 body anticipated.

### Network postures

| `ConfinementSpec.NetMode` | container lowering |
|---|---|
| `allow_all` | netns `host` (unchanged) |
| `deny_all` | netns `none`, loopback-only (unchanged) |
| `broker` | netns `none` plus a netns-local egress allowlist proxy pinned to one port |

`broker` is lowered to a netns-local allowlist proxy (the same posture
the kernel egress broker provides on the syscall tier, expressed
container-natively). The proxy's lifecycle is tied to the sandbox: if
the proxy cannot be established, the launch refuses — no silent
downgrade to `deny_all` or `allow_all`.

### Probe and evidence

After shaping, croft probes before the task runs: write a canary outside
the granted roots (expect `EROFS`) and inside (expect success). Failure
refuses the launch. Evidence normalization maps `EROFS` to the same
denied-access vocabulary the Landlock probe produces
(`ErrPathAccessDenied` / `Denied`), so `AuthorityEvidence` and
`GrantMatch` mean the same thing on every tier.

## Consequences

- **goatherd-croft:** `Config` grows a writable-roots list mirroring
  `ConfinementSpec.WritableRoots` (container-path strings; `Secrets`
  stay a delivery concern but share the mount plumbing). OCI mount
  lowering + probe + `broker` proxy. `SecretMount`-style read-only binds
  already prove the mount vocabulary works.
- **goatherd-runtime `sandbox/croft`:** `refuseUnsupportedAuthority` is
  replaced by lowering for grants and `broker`; `SupportsNetwork`
  reports `broker`; `LaunchConfined` runs probe-first. Gates
  shepherd-runtime-go-78r item 4.
- **yaah:** the `world.go` refusal for `RestrictiveGrants` on container
  worlds is removed and `World.RestrictiveGrants` flows through; the
  dispatch path already computes `rec.Writable` from grants.
- **goatherd-kernel:** no vocabulary change — the same spec, the same
  decision/verdict shapes; container placements simply gain
  grant-conformant outcomes to record.
- The tier story becomes uniform: **the same `ConfinementSpec`, the same
  fail-closed probe, the same evidence — two enforcement substrates**
  (syscall confinement and container mount shaping). Granular authority
  no longer implies "must not be a container session", so multi-binding
  container tasks (yaah-kit) are unblocked.

## Alternatives considered

- **Option B (coarse but honest).** Cheap and truthful, but it
  institutionalizes the authority-granularity gap just as the substrate
  is free to close it, and it keeps container sessions second-class for
  per-binding grants. Rejected.
- **In-container syscall confinement (Landlock inside the guest).**
  Attractive (`EACCES` semantics identical to the syscall tier) but
  depends on the guest kernel and on seccomp not filtering the Landlock
  syscalls — a second enforcement mechanism per tier. Mount shaping is
  fundamental to the container substrate and works regardless of guest
  kernel. Rejected as the primary mechanism; revisitable later as a
  defense-in-depth layer if mount shaping proves insufficient.
- **Naive Option A (separate writable stores per root).** Rejected for
  the capture-aggregation cost above; A′ preserves the single-layer
  change-set.

## Acceptance

- [x] Decision recorded with rationale (this ADR).
- [ ] Chosen option implemented with tests (croft mount shaping +
  probe + `broker` proxy; runtime adapter lowering; yaah refusal
  removal).
- [ ] goatherd-runtime `sandbox/croft` adapter plan (shepherd-runtime-go-78r
  Phase 0) updated to this vocabulary.

## References

- croft-ku0 and its 2026-10-22 comment (decision gates the adapter's
  Confinement lowering vocabulary)
- croft-shepherd-fit.md §6/§8 (Frameworks dir) — composition and gap
  analysis
- yaah-shepherd-gap-report §5.2 items 1–2 (authority granularity,
  macOS/advisory)
- goatherd-runtime `sandbox/confinement` (`ConfinementSpec`,
  `ResolveSpec`), `sandbox/containment` (`LaunchConfined`)
- goatherd-croft `sandbox.go` (OCI spec, `SecretMount`, staging mount),
  `sandbox/croft/exec.go` (`refuseUnsupportedAuthority`)
