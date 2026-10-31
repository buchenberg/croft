# ADR 0002 — Lane-aware shell authority for sub-agents

- **Status:** Proposed — pending acceptance (implementation + tests)
- **Tracking:** croft-ubz
- **Date:** 2026-10-31
- **Decides:** the `destructive_guard` shell denial in yaah sub-agent scopes
  (follow-on to croft-ku0 / ADR 0001)
- **Scope:** yaah `internal/agent/supervision`, `internal/agent/pipeline`,
  `internal/agent/runner`, `internal/config`, model-facing prompts;
  goatherd-runtime `sandbox/croft`; croft OCI spec
- **Related:** ADR 0001 (grant-shaped authority in the container tier),
  yaah epic yaah-c3z3 (container-lane prompt parity), yaah-9n0 (tool
  availability per environment), croft-ku0

## Context

yaah's supervisor denies the shell tools in **every** sub-agent dispatch.
The rule is a name match, not an argument analysis:

```go
// internal/agent/supervision/service.go:127
if toolName == "bash" || toolName == "powershell" || toolName == "background_process" {
    ...
    return "Blocked destructive operation. Supervisor denied."   // :153
}
```

It is wired as ordinary pipeline middleware
(`pipeline/config.go:269` → `NewSupervisorGuardMiddleware`), whose check
function has the signature `func(ownerID, toolName, argsJSON string) string`
(`pipeline/supervisor_guard.go:35`). **There is no world, lane or realm
parameter.** The guard cannot distinguish the two dispatch worlds, so the
same rule is applied to a host shell and to a container shell.

That distinction is the entire question, because the two lanes put the shell
in different places.

**Worktree lane — the shell is the host.** `bash` calls `ws.Exec`
(`internal/tools/bash.go:76`); for a `localWorkspace` that resolves to
`confinedExec`, which builds a `confinement.ConfinementSpec{WritableRoots,
Network}` (`internal/tools/workspace.go:326`) and launches through
`containment.LaunchConfined` (`:344`). So it is a host process, at the host
uid, with libc, `/proc` and every host binary inside reach, held in only by
Landlock/winacl. Worse, the jail is conditional: `runner.go:1339` calls
`resolveSubAgentConfinement` only `if workdir != ""`, and its own comment
records that "the orchestrator's workspace and a workdir-less (shared-host)
sub-agent keep running unconfined". `runner.go:1776,1784` refuse restrictive
grants without a jail, which is the honest posture — but it means the
unconfined host shell is a reachable configuration. Denying shell *there* is
defensible; it is also blunt, since read-only commands such as `gofmt -l` are
refused too.

**Container lane — the shell is in the sandbox.** The dispatch body hands the
runner the container workspace, so "the runner builds the sub-agent's registry
against the container" (`internal/tools/dispatch_container.go:135`); `bash` →
`ws.Exec` → in-band goatherd exec → the command executes **inside** the
croft world. Against the worktree lane:

| Axis | Worktree lane | croft container lane |
|---|---|---|
| Who executes | host process, host uid | process inside the world's namespaces |
| Filesystem reach | whole host FS; writes kern-denied outside the root | rootfs overlay + shaped binds; staging is `nosuid,nodev,noexec`; secrets are read-only binds |
| Host write path | writes land in the worktree directly | none — `apply_out`, dry-run first and approval-gated |
| Network | the host's, unless jailed | netns unshared by default; `host` is an explicit opt-in (`client.go:563-570`) |
| Privilege | user's uid | `CAP_SYS_ADMIN` only in the shaping init; user processes drop to `croftExecCapabilities` (`authority.go`) |
| Lifetime | worktree persists until settled | world destroyed at seal |

Every axis the guard exists to protect is strictly narrower in the container
lane. The guard is therefore applying a worktree rule to a lane where its
premise does not hold.

yaah already knows how to reason per world elsewhere: `runner.go:1367`
computes `restrictiveEgress := opts.workspace == nil && restrictivePosture(...)`,
and `subAgentUnavailableTools` / `LeafToolHostOnly` / `isolated`
(`runner.go:1360-1391`, bead yaah-9n0) omit tools a world cannot offer. The
guard was never joined to that knowledge.

Two gaps mean the container lane is not yet a blanket licence for a shell:

1. **No seccomp profile and no `NoNewPrivileges`.** croft's spec is containerd's
   default plus `WithEnv`, `WithDefaultPathEnv`, `WithProcessCwd`, user, mounts
   and limits (`client.go:509-570`); it adds neither. Whatever the base spec
   provides is the whole syscall story.
2. **Nothing forces limits or egress discipline on a dispatch.** `Config.Limits`
   supports CPU, memory and **pids** (`withLinuxResources`, `client.go:623`),
   and `Config.Network` accepts `host` — but neither is required of a sub-agent
   container world. Read-only-mounted secrets are readable by any in-container
   process, so egress is what turns a readable secret into an exfiltrated one.

## Decision

**Make the shell decision a function of the actual world, not of the tool name
alone.** Keep the guard; give it the world.

1. **The guard becomes world-aware.** The dispatch resolves its world before the
   registry is built, so the world is bound into the guard's check rather than
   passed at call time (the `(ownerID, toolName, argsJSON)` signature can stay
   if the world is captured when the middleware is constructed). The middleware
   stays the enforcement point: it is the same place in both lanes, so the
   policy is one code path with one test seam.
2. **Worktree lane: unchanged.** `bash` and `powershell` remain denied. The
   premise of the denial — a host shell behind a possibly-absent jail — is
   intact.
3. **Container lane: permitted, subject to prerequisites.** `bash` and
   `powershell` are allowed **only** when all of the following hold, and denied
   with a distinguishing message otherwise:
   - the **actual** world is `container` (a worktree world that was *requested*
     to be a container does not qualify — see 4);
   - the world's network posture is `deny_all` or broker-mediated, **never**
     `host`;
   - resource limits are set for the world (pids and memory at minimum).
4. **Degraded worlds fail closed.** When a container was requested and the host
   could not provide one, the dispatch reports `warning` and runs elsewhere —
   and the guard keys on the actual world, so a degrade converts the container
   shell allowance back into a denial. It must never silently become a host
   shell.
5. **`background_process` stays denied in both lanes.** An orphaned background
   shell outlives the world it was authorised in; the container lane's teardown
   guarantee does not extend to a process the harness stopped tracking. This is
   out of scope for the shell allowance and should be decided separately.
6. **No command allowlist.** Do not replace the name-level rule with a matcher
   for "safe" commands. The guard's existing design already declines to inspect
   arguments, and correctly: `sh -c`, a base64 payload or a generated script
   defeats any pattern list, so a matcher would buy the appearance of control
   and deliver a false negative on the path that matters. The lane is the
   soundly-checkable property; the command line is not.
7. **Config surface, default off.** A single explicit key under the existing
   rule block (shape: `agents.default.supervisor.rules.destructive_guard.
   container_shell: deny | allow`, defaulting to `deny`), consistent with the
   supervisor surface's existing "config default off, runtime opt-in" pattern.
   The existing `...destructive_guard.enabled` switch remains the global stop.
   No per-role dimension in this ADR — role-specific authority already has a
   vocabulary (grants) and should not grow a second one for shells.

### Prerequisites that must land before the allowance is enabled

Ordered; each is a croft or goatherd-runtime change, not a yaah one:

- **P1 — default seccomp profile and `NoNewPrivileges`** in croft's spec opts.
  Without a profile the container's syscall surface is the kernel's. This is the
  cheapest large reduction in what an in-container shell can do.
- **P2 — mandatory limits for sub-agent container worlds.** croft can carry
  pids/memory today; yaah must require them rather than leave them optional, so
  a shell cannot fork-bomb or OOM the daemon. Forks and memory are the two
  amplification primitives a shell hands the model for free.
- **P3 — refuse `Network: host` for sub-agent container worlds** at the
  `sandbox/croft` lowering seam (goatherd-runtime), failing loudly rather than
  downgrading. Broker or `none` only. This is what contains the read-only
  secrets mounts, so P3 gates the allowance.
- **P4 — bounded exec output inside the sandbox.** yaah already truncates what
  the model sees (`truncateOutput`, `commandOutputMaxBytes`,
  `internal/tools/result.go:88`), which bounds the transcript but not the
  sandbox-side buffer. The in-band exec path should carry its own bound.

## Consequences

**Positive.** The container lane stops being the lane you cannot build in. Code-
changing roles get `go test`, `gofmt`, `staticcheck`, `git status` and real
builds inside the world that is designed to contain them, which is the single
largest fidelity gap in dispatched work today — a read-only audit lane cannot
produce test or formatting evidence, so the parent session has to re-run every
gate. It also makes the container lane the preferred lane for changing roles,
which is the direction ADR 0001 already set and the reason grant-shaped
authority was made affordable.

**Negative.** A larger in-container blast radius: the container image must
carry a toolchain, so "what is in the image" becomes security-relevant, and
egress discipline becomes load-bearing rather than cosmetic (P3). One more
config surface and a two-world guard to test. Docs and prompts must move in the
same change — the tool docs in `internal/prompts` are go:embed'd and
`internal/tools/doc_parity_test.go` pins schema↔doc agreement, so a behaviour
change without the matching prompt edit fails the gate or, worse, passes it and
mis-teaches the model. yaah-c3z3 already tracks the container-lane prompt
deficit this would deepen.

**Neutral.** The guard's semantics become two-valued rather than global; the
`enabled` switch keeps its meaning and its rationale (an emergency stop).

**Explicitly out of scope.** `memory_*` tools mutate `~/.yaah/state.db` from
container sessions with no host-only marker, `ReadOnly` flag or omission — an
existing unguarded host-reaching path that this ADR neither fixes nor worsens.
Nor does it revisit in-container Landlock as defence in depth (rejected as
primary in ADR 0001, still revisitable).

## Alternatives considered

- **Turn the rule off globally** (`...destructive_guard.enabled: false`). Rejected:
  it is the one-line answer and it hands a host shell to the worktree lane,
  which is where the denial is load-bearing.
- **Command allowlist for read-only commands.** Rejected — see decision 6. It
  fixes the symptom that prompted this ADR (read-only commands refused) while
  leaving the actual variable — the lane — unmodelled.
- **Express the allowance as tool availability** (omit `bash` from worktree-lane
  registries instead of refusing the call). Rejected as the primary mechanism,
  for a reason worth recording: omission says *this tool cannot work here*,
  refusal says *this tool must not be used here*. `bash` works perfectly well in
  a container, so describing it as unavailable would be a lie of the same class
  the tool-honesty work has been removing. Availability is already used for
  genuinely-environment-bound tools (host-network leaves); the guard is the
  right home for a policy denial.
- **Allow shell in both lanes and rely on the syscall jail.** Rejected:
  `runner.go:1760-1784` establishes that the jail is optional and can be absent
  (shared-host, workdir-less), so this trades a worktree rule for a host shell
  in the exact configuration the jail does not cover.
- **Per-role shell authority in config.** Rejected for now: a second authority
  vocabulary competing with grants. If per-role control is wanted later, it
  belongs in grants, not in a parallel shell list.
- **Implement in croft/goatherd instead of yaah.** Rejected: croft can shape the
  world but cannot know which lane a dispatch used; only yaah holds both facts
  (world and sub-agent scope). croft's share of this ADR is the prerequisites,
  not the decision.

## Implementation sketch

1. **yaah, world binding.** Introduce the resolved world at the point the
   dispatch decides it (`dispatch_container.go` / `runner.go` around
   `opts.workspace`) and bind it into the guard middleware construction
   (`pipeline/config.go:269`). Keep the deny message distinct per lane so a
   model that was refused can tell whether to change approach or change lane.
2. **yaah, policy.** `bash`/`powershell` permitted iff actual world is container
   ∧ network ∈ {none, broker} ∧ limits present; `background_process` denied
   everywhere; worktree lane denied everywhere. Default `deny`; the allowance is
   opt-in per config.
3. **yaah, tests.** Table test over {worktree, container, container-degraded} ×
   {bash, powershell, background_process} × {permitted/refused config}, plus a
   degrade test proving a requested-container/warning dispatch stays denied, and
   a test that the refusal message names the lane. Follow the existing
   conformance kit rather than a new harness.
4. **croft / goatherd-runtime, prerequisites.** P1 seccomp + `NoNewPrivileges`
   in `initSpecOpts`; P2 limits required at the adapter seam; P3 netns refusal
   for sub-agent worlds; P4 sandbox-side exec bound.
5. **docs.** The isolation-vs-egress matrix in `docs/configuration.md`
   (yaah-2uwu section) gains the shell row; `SECURITY.md`'s two-axis statement
   gains the container-shell sentence; the model-facing tool docs and
   `subagent_tools.md` state which lanes offer a shell.

## Open questions

- Should a container-lane shell also be granted the ability to *reach* the
  session's own container (as opposed to its per-dispatch world)? Today the
  dispatch world is what the registry is built against, so this is presumably
  no — worth pinning in a test rather than in prose.
- Do secrets need to be withheld from roles granted a shell, or is P3 (no host
  netns) sufficient? The current answer is P3; it deserves an explicit check
  because it is the only remaining path from a readable secret to the network.
- Is `apply_out` still the sole host write path once shell is allowed, or can a
  shell write into shaper-mounted rw roots that the harness treats as
  host-visible? ADR 0001's shaping says writes land in the overlay upper, so
  this should already hold — but it becomes worth a probe-first canary now that
  arbitrary commands run in the world.
