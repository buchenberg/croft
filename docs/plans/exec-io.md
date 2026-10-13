# Exec I/O Plan — croft

Close the practical 80% of croft's exec I/O gap: **stdin as data**, **bounded
output**, and a **documented and live-tested recipe for long-running
background processes**. Ships as croft v0.5.0, after the v0.4.0 trio (lean
core, snapshot-native diff, snapshot-native delta); the broker inherits all
three pieces and drops its own output-capping shim.

## Part 0 — Baseline and scope boundaries

Assumes v0.4.0 has landed (the release train order is the only dependency —
none of this work touches the diff/delta machinery, and `Exec`'s signature
survives the lean reshape unchanged).

**Deliberate non-goals, decided (2026-10-13):**

- **Interactive bidirectional exec sessions** (attach, write mid-exec,
  live-read) — collides head-on with the upstream runc-v2 behavior that blocks
  stdin EOF: `execProcess.openStdin` opens the exec's stdin FIFO `O_WRONLY`
  and keeps the write end for the process's lifetime, so a client closing its
  end never propagates EOF (client.go:47-52, the `ErrStdinUnsupported`
  rationale). A session API would need its own lifecycle object and a
  workaround for that upstream bug, and its use cases are covered by the
  pieces this plan does ship. Revisit only when a real consumer needs a TTY.
- **Process-preserving capture** — permanent scope boundary, not a roadmap
  item. The tempting fix (containerd `Task.Pause`/`Resume`, the cgroup
  freezer) fails structurally: after `Commit`, the running task's mount *is*
  the committed layer, which immediately becomes the successor's lowerdir —
  resuming writes would corrupt the successor's OverlayFS view. The processes
  hold fds and inodes on the old mount; only CRIU checkpoint/restore could
  migrate them, and CRIU on arbitrary process trees (execs, reparented
  children, sockets) is experimental and permanently fragile. Document as a
  boundary with the same honesty as the containment caveats; do not attempt.

## Part 1 — Mechanism

### 1.1 Stdin as data, via temp-file redirect

`ExecRequest.Stdin []byte` exists and is rejected today (`ErrStdinUnsupported`,
client.go:634). The fix avoids the FIFO entirely:

1. Write the bytes to `/tmp/croft-stdin-<rand>` through the existing
   `WriteFile` path (chunked, handles arbitrary sizes — the delta machinery
   already stages patches at `/tmp/croft-delta-*` this way, so absolute
   paths outside the workdir are proven).
2. Wrap the process: `["/bin/sh", "-c", "exec \"$@\" < /tmp/croft-stdin-…",
   req.Command, req.Args...]`. The `exec "$@"` pattern passes the command and
   arguments through verbatim — no quoting or escaping of user arguments,
   and the redirection happens in the shell. The image contract already
   requires `/bin/sh` (it is `InitArgs`'s default).
3. Remove the temp file after the exec completes — registered before the
   exec runs, executed cancel-free on every path, the same house rule as the
   delta patch cleanup.

Zero-containerd-interaction; no upstream bugs in play. The exposure caveat
inherits from `WriteFile`: content travels base64 in argv, visible in
`/proc/*/cmdline` — stdin-shaped data, never secrets. `ErrStdinUnsupported`
is deleted (breaking, v0.5.0).

### 1.2 Bounded output

`containerdTasks.Exec` collects stdout and stderr into unbounded
`bytes.Buffer`s (client.go:655 area). Replace with a capped writer:

- `Config.MaxOutputBytes` — per-stream ceiling; `0` means unbounded (the
  current behavior, and the documented default).
- Over-cap streams keep the first and last half of the budget with a
  `[... N bytes truncated ...]` marker between them — heads show the command
  banner, tails show the actual error, which is what agents read for.
- Byte-based cap (a UTF-8 sequence may split at the boundary; documented, not
  corrected — output is diagnostic text).
- The same buffer wraps both streams; the cap applies to each independently.

This moves the broker's output-capping policy from broker-side shims into the
library, where direct consumers (yaah) get it too. The broker sets
`Config.MaxOutputBytes` from tenant policy.

### 1.3 Long-running background processes — recipe, proven

Long-running processes already work today, undocumented: an exec of
`sh -c 'nohup <server> > /tmp/server.log 2>&1 &'` returns immediately, the
child survives the exec (reparented to the container's init), and clients
tail the log with `ReadFile`. That covers dev servers, test watchers, and
fleet daemons — the standard `Exec`-and-poll pattern agent workloads want.

This plan's job is to make it a *product* behavior: document the recipe in
the README and `docs/overview.md` (including the boundary — background
processes die at `Capture`, which is the known process-state cost of a
coherent snapshot), and pin it with a live test.

## Part 2 — Target shape

```go
// ExecRequest.Stdin — field exists; semantics change from rejected to
// data staged via temp file and redirected into the process.
type ExecRequest struct { /* unchanged shape */ }

// Config gains:
type Config struct {
    // ...
    // MaxOutputBytes caps each of an exec's stdout and stderr in memory.
    // The first and last MaxOutputBytes/2 bytes survive, with a truncation
    // marker between. Zero means unbounded.
    MaxOutputBytes int
}
```

No `Sandbox` interface changes. Errors: `ErrStdinUnsupported` deleted;
everything else unchanged.

## Part 3 — Tasks (one task per commit, bisectable)

### Task 1 — Stdin staging
- The 1.1 mechanism in `containerdTasks.Exec`; `ErrStdinUnsupported`
  deleted everywhere (check yaah for references at execution time).
- Failure paths: temp-file write fails → no exec runs, cleanup best-effort;
  exec runs → cleanup cancel-free regardless of outcome.
- Tests (fake + live): stdin round-trip (`wc -c` over staged bytes);
  a binary payload; stdin with a command that ignores it; the cleanup
  leaves no `/tmp/croft-stdin-*` residue (assert via a follow-up exec).
- Live: `TestLive_ExecStdinIsData`.

### Task 2 — Capped output
- `cappedBuffer` (head/tail retention, overflow marker); `Config` knob;
  fake and unit tests for head-only, tail-only, exactly-at-cap, and
  zero-cap semantics; live test asserting the marker appears and the
  unstreamed middle is gone.

### Task 3 — Background-process recipe
- README + `docs/overview.md`: the recipe, its polling pattern, and the
  Capture-kills-processes boundary stated plainly.
- Live: `TestLive_BackgroundProcessSurvivesExec` — start a background
  writer, exec returns, poll `ReadFile` until output appears, then assert
  the process is still alive (its log still grows) — and a second test
  asserting the boundary honestly: after `Capture`, the process is gone.

### Task 4 — Docs + CHANGELOG v0.5.0
- Added: stdin-as-data (`ExecRequest.Stdin` honored), `Config.MaxOutputBytes`,
  the background-process recipe.
- Breaking: `ErrStdinUnsupported` removed.
- The exec I/O scope boundaries (interactive sessions deferred; process-
  preserving capture permanent) recorded in the README's contracts section.

## Risks

- **Argv exposure of staged stdin** — identical to `WriteFile`'s documented
  exposure; stated in the stdin doc, not papered over.
- **`/bin/sh` dependency** for the redirect wrapper — already in the image
  contract; no new requirement.
- **Truncation surprises** — a host debugging an over-cap exec sees a marker,
  not silent loss; the marker names the dropped byte count.
- **The recipe is process-semantics-dependent** (reparenting to PID 1) —
  pinned by the live tests, so a containerd/runc behavior change that breaks
  it fails CI on the next live run, not in a tenant's workload.
- **Streaming output is still absent** — bounded-but-buffered, not live.
  Deliberate; a follow-up plan if a consumer needs live tails (the `cio`
  plumbing already streams, so the cost is API design, not mechanics).

## Validation

1. `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` green;
   `go test ./... && go test -race ./...` green.
2. Live suite against a real daemon including `TestLive_ExecStdinIsData`,
   the capped-output assertions, and both background-process tests.
3. Straggler check: `grep -rn "ErrStdinUnsupported" --include=*.go .` empty.
4. Contract check: README's exec section documents stdin-as-data, the
   output cap, the background recipe, and the two scope boundaries.

## Out of scope

- Interactive exec sessions / TTY (revisit on consumer demand).
- Streaming (chunked) output — follow-up plan.
- CRIU / process-preserving capture — permanent boundary.
- Secrets-safe input paths (a real secrets story is its own cross-cutting
  design; this plan documents the exposure instead of pretending).
