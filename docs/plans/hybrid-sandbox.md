# Hybrid sandbox plan — git worktrees + croft

A detailed plan for composing shepherd-runtime-go's git worktree backend
with croft's containerd backend into a unified workspace architecture:
git for source control and lightweight forking, croft for isolated
execution and resource-limited agents.

## 1. Problem statement

Shepherd-runtime-go's `Sandbox` interface has two natural backends that
are individually incomplete:

- **Git worktree** (`sandbox/git`): cheap forks (`git worktree add
  --detach`), content-derived capture digests, line-level diff, cross-platform.
  But `Exec` and `FileIO` return `ErrUnsupported`. The worktree is a
  directory, not an isolation boundary. Agents run on the host kernel.

- **Croft** (`github.com/buchenberg/goatherd-croft`): real isolation (namespaces +
  separate rootfs), exec inside containers, file I/O across the boundary,
  resource limits, network policy, secret delivery. But it's Linux-only,
  needs containerd, and its state digests are sequence-derived (violating
  shepherd's digest-stability contract).

Neither alone covers the full workload: **agents need git-native source
control (branches, PRs, diffs) AND isolated execution (run code without
host exposure)**. Today they're used in separate processes or by
copying files between them.

## 2. Design principles

1. **Complementary, not competitive.** Git is the control plane (source
   control, human-readable diffs, PR workflows). Croft is the data plane
   (execution, isolation, resource limits). Each does what it's best at.

2. **The worktree is the bridge.** Files flow through the shared
   worktree path. No sync protocol, no merge tool — git sees what croft
   wrote and vice versa.

3. **Capability-based routing.** The scope manager already routes by
   `SandboxCapabilities`. A scope that needs `Diff` uses git. A scope
   that needs `Exec` uses croft. A scope that needs both uses the hybrid
   adapter.

4. **The adapter is thin.** Croft's `ContainerdSandbox` wraps into
   shepherd's `Sandbox` interface with minimal translation. The worktree
   mount is a bind-mount, which croft already supports.

5. **Fail honest.** When a capability is unavailable, report it via
   `Capabilities()` — never pretend.

## 3. Architecture

```
shepherd ScopeManager
    │
    ├── git backend (sandbox/git)
    │     ├── NewWorktreeSandbox(repo, path)  → lightweight forking
    │     ├── Capture → git commit/tree state
    │     ├── Diff → git diff
    │     └── ApplyDelta → git stash/merge
    │
    └── croft backend (sandbox/croft)          ← NEW
          ├── NewCroftSandbox(cfg, worktreePath)
          ├── Create → provision container with worktree bind-mount
          ├── Capture → croft snapshot
          ├── Exec → run in container
          ├── ReadFile/WriteFile → across boundary
          ├── Apply → rebind container
          └── Diff → snapshot-native diff

              ┌──────────────────────────┐
              │  git worktree directory  │
              │  (shared mount point)    │
              │  ←── bind-mount ──→      │
              │  container /workspace    │
              └──────────────────────────┘
```

### 3.1 The worktree mount

Croft already bind-mounts host files (`Config.Secrets`). The hybrid
adapter uses the same mechanism to bind-mount the git worktree directory
read-write into the container:

```
Host: /repos/my-project-wt1/     →  Container: /workspace/
Host: /repos/my-project-wt2/     →  Container: /workspace/
```

Each sandbox gets its own worktree. The container's `/workspace` is the
worktree. Writes in the container appear in the worktree. Git commands
run against the worktree see them immediately.

### 3.2 Two execution modes

**Mode A: Shared worktree (default).** The worktree is bind-mounted into
the container. Git and croft operate on the same files simultaneously.
The agent writes code in the container; the human reviews diffs in the
worktree. No sync step.

**Mode B: Croft-native workspace.** The container has its own overlayfs
workspace (no worktree mount). Full isolation — nothing leaks to the host.
Capture produces croft snapshots. Git integration requires an explicit
export step (copy out to a worktree). Use this when the workload is
untrusted or the source tree is not git-managed.

### 3.3 State identity bridge

Shepherd's `Capture` contract requires content-derived digests. Croft's
are sequence-derived. The hybrid adapter resolves this by computing the
digest from the git tree when operating on a shared worktree:

- **Shared worktree**: capture records the git tree SHA (content-derived,
  digest-stable). A capture of unchanged content produces the same SHA.
  This satisfies shepherd's contract.

- **Croft-native workspace**: capture records the croft snapshot key
  (sequence-derived). The adapter reports `ContentStable: false` in
  capabilities, and callers know not to use digest equality for
  settlement.

## 4. The adapter interface

```go
// package sandbox/croft
type CroftSandbox struct {
    sb       *croft.ContainerdSandbox
    worktree string // host path (shared mode) or "" (native mode)
    repo     string // git repo path for digest computation (shared mode)
    mode     Mode
}

type Mode int
const (
    ModeSharedWorktree Mode = iota  // bind-mount worktree, git digests
    ModeNativeWorkspace             // overlayfs workspace, croft digests
)

func NewSharedWorktree(cfg croft.Config, repo, worktree string) *CroftSandbox
func NewNativeWorkspace(cfg croft.Config) *CroftSandbox

// Implements sandbox.Sandbox
func (c *CroftSandbox) Backend() string { return "containerd" }
func (c *CroftSandbox) Capabilities() sandbox.SandboxCapabilities {
    if c.mode == ModeSharedWorktree {
        return sandbox.SandboxCapabilities{
            Lifecycle:   true,
            Exec:        true,
            FileIO:      true,
            Diff:        true,
            Isolated:    true,
            Containment: trace.ContainContained,
        }
    }
    return sandbox.SandboxCapabilities{
        Lifecycle:   true,
        Exec:        true,
        FileIO:      true,
        Diff:        true,
        Isolated:    true,
        Containment: trace.ContainContained,
    }
}

// Capture in shared mode: commit the worktree, return git tree state.
// Capture in native mode: croft snapshot.
func (c *CroftSandbox) Capture(ctx context.Context) (trace.WorkspaceState, error)

// Diff in shared mode: git diff (line-level, rich).
// Diff in native mode: croft snapshot diff (tree-level, Myers).
func (c *CroftSandbox) Diff(ctx context.Context, ws trace.WorkspaceState, maxLines int) (string, []string, error)
```

## 5. Detailed tasks

### Task 1 — `sandbox/croft` adapter skeleton (1 day)

- `CroftSandbox` struct wrapping `*croft.ContainerdSandbox`
- `Backend()`, `Capabilities()` — report the capability matrix
- `Create(ctx, spec)` — provision the container; in shared mode, add the
  worktree bind-mount to the OCI spec
- `Destroy(ctx)` — teardown the container
- Stub `Capture`, `Apply`, `Diff`, `Exec`, `ReadFile`, `WriteFile` with
  `ErrUnsupported` until each is wired

Tests: capability matrix matches the mode; Create/Destroy lifecycle;
worktree bind-mount appears in the generated OCI spec.

### Task 2 — File I/O bridge (1 day)

- `ReadFile` → croft `ReadFile` (same API)
- `WriteFile` → croft `WriteFile` (same API)
- In shared mode: verify writes appear in the host worktree (live test)
- In native mode: verify writes stay inside the container

Tests: write in container → read from worktree; write to worktree →
read from container; mode boundaries.

### Task 3 — Exec bridge (1 day)

- `Exec` → croft `Exec` (same API shape; shepherd's `ExecRequest` maps
  to croft's `ExecRequest` field-for-field)
- Wire `Stdin`, `Timeout`, `Env`, `Cwd`
- In shared mode: commands see the worktree as their working directory

Tests: exec returns stdout/stderr/exit-code; stdin data delivery;
timeout; env/cwd propagation; worktree files visible to commands.

### Task 4 — Capture/Apply with git digests (2 days)

Shared worktree mode:
- `Capture`: run `git add -A && git write-tree` to get the tree SHA
  (or `git stash create` for uncommitted changes), return as
  `WorkspaceState` with the tree SHA as the digest
- `Apply`: `git reset --hard <tree-sha>` (or `git stash apply`)
- Digest stability: two captures of identical content → identical SHAs

Native workspace mode:
- `Capture` → croft `Capture` (snapshot key as state)
- `Apply` → croft `Apply`
- Report `ContentStable: false` in capabilities

Tests: digest stability (capture twice, same SHA); capture → mutate →
apply restores; native mode round-trip.

### Task 5 — Diff bridge (1 day)

Shared worktree mode:
- `Diff`: `git diff <tree-sha>` — line-level, binary detection,
  mode changes (shepherd's git backend already does this)
- Return `(diff string, files []string, err error)` per shepherd's signature
- `maxLines` cap enforced

Native workspace mode:
- `Diff` → croft `Diff` (snapshot-native) + `Changes` for the file list
- Map croft's `[]Change` to shepherd's `[]string` (paths only)

Tests: diff correctness in both modes; maxLines cap; changed file paths.

### Task 6 — ApplyDelta bridge (1 day)

Shared worktree mode:
- `ApplyDelta` → croft's `ApplyDelta` (tree-level 3-way merge)
  operating on the worktree's overlayfs view
  OR `git merge` for line-level (configurable)
- Conflict handling: atomic refusal (croft's contract)

Native workspace mode:
- `ApplyDelta` → croft `ApplyDelta`

Tests: merge over diverged worktree; conflict refusal; clean merge.

### Task 7 — Worktree mount plumbing (1 day)

- Add `Config.BindMounts` to croft: a `[]BindMount{Source, Target,
  Options}` alongside the existing `Config.Secrets`
- The adapter populates it with the worktree mount
- Validation: source must exist and be a directory; target must be
  absolute and not collide with the workdir root

Tests: bind-mount appears in the spec; validation rejects bad mounts.

### Task 8 — Registration + examples (1 day)

- `sandbox/croft` self-registers as `"containerd"` via
  `RegisterSandbox`
- Example: fork two worktree-backed croft sandboxes from one parent,
  run commands in each, diff results
- Example: best-of-N with isolated execution

Tests: `OpenSandbox("containerd", cfg)` returns a working sandbox;
end-to-end example passes.

### Task 9 — Live tests + docs (1 day)

- Live tests against a real containerd daemon (gated like croft's
  live suite)
- README: hybrid architecture diagram, capability matrix, mode
  comparison table
- Shepherd integration guide: when to use git vs croft vs hybrid

## 6. What we gain (summary)

| Workload | Git alone | Croft alone | Hybrid |
|---|---|---|---|
| Speculative code forks | ✅ | ✅ | ✅ (git for fork, croft for run) |
| Agent execution | ❌ (host kernel) | ✅ | ✅ |
| PR review / diff | ✅ | ⚠️ (snapshot diff) | ✅ (git diff) |
| Untrusted code | ❌ | ✅ | ✅ |
| Resource limits | ❌ | ✅ | ✅ |
| Multi-tenant SaaS | ❌ | ✅ (with broker) | ✅ (with broker) |
| Cross-platform | ✅ | ❌ (Linux) | ⚠️ (git on all, croft on Linux) |
| Content-stable digests | ✅ | ❌ | ✅ (shared mode) |
| Non-git workspaces | ❌ | ✅ | ✅ (native mode) |
| Secrets delivery | ❌ | ✅ | ✅ |

## 7. Risks and open questions

1. **Bind-mount performance.** Overlayfs-over-overlayfs (worktree is
   git's working tree, mounted into an overlayfs container) can be slow
   for large trees. Mitigate by testing with 10K+ file repos; consider
   a copy-in/copy-out mode if benchmarks demand it.

2. **Git index contention.** If both the host and the container run
   `git` commands simultaneously, the index can lock. Mitigate by
   documenting "git on the host, build/test in the container" and
   using `git --no-optional-locks` in the container if needed.

3. **Digest stability bridge.** Computing git tree SHAs from a
   container's filesystem view requires running `git` inside the
   container (it's in the image contract already) or reading the
   worktree from the host. The host-side read is simpler but breaks for
   native-mode workspaces. This is Task 4's core decision.

4. **State portability.** A state captured in shared mode (git SHA)
   cannot be applied in native mode (croft snapshot) and vice versa.
   The adapter should reject this loudly rather than silently
   misapplying.

5. **shepherd's digest contract.** The `Capture` doc says "a
   snapshot-backed backend whose captures carry a per-capture sequence
   key does not [satisfy digest stability] and must record that as its
   own follow-up gap." Croft is exactly that case. Shared mode resolves
   it; native mode keeps it as a documented gap.

## 8. Sequencing

```
Task 1 (skeleton) ──► Task 2 (file I/O) ──► Task 3 (exec) ──► Task 4 (capture/apply)
                                                                        │
                    Task 7 (bind mounts) ◄──────────────────────────────┘
                                                                        │
                    Task 5 (diff) ──► Task 6 (delta) ──► Task 8 (register) ──► Task 9 (live + docs)
```

Tasks 1–3 are the critical path: they make the adapter functional for
the core agent loop (write file → exec → read file). Tasks 4–6 add
state management. Tasks 7–9 productionize.

**Total: ~9 working days** for a working hybrid backend with live
verification.
