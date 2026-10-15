# Network Policy Plan — croft

> **Correction (2026-10-15):** this plan's premise was inverted — croft's
> containers have never shared the host's network namespace; containerd's
> default spec already unshares it. The ruled semantics are in
> "Correction: the premise was inverted" at the end. Parts 0–2 below keep
> their intent and task shapes, but the mechanism is flipped: `host` is the
> mode that changes the spec, and the isolated default needs nothing.

Give a workspace a network posture: **`Config.Network`** with two modes —
`host` (today's behavior, the default) and `none` (a fresh network namespace
with only loopback: a true off-switch). This is the biggest honesty gap in an
"isolated workspace" — croft's containers currently share the host's network
namespace, so an agent workspace can reach everything the host can. Ships as
croft v0.6.0, after exec I/O (v0.5.0); the broker surfaces it as per-tenant
policy.

## Part 0 — Baseline and scope boundaries

Assumes v0.4.0 (lean) has landed — the work touches `initSpecOpts`
(client.go:436+), which the lean reshape does not alter. Independent of the
diff, delta, and exec I/O phases.

**Deliberate scope boundary, decided (2026-10-13):** v1 is all-or-nothing.
`none` is a real off-switch; controlled egress (allowlist proxies, per-host
firewall rules, a broker-side egress proxy) is a follow-on — see the roadmap's
follow-on section — because every form of it drags in CNI machinery or a
userspace proxy that is its own design problem. A workspace that needs
package installs uses `host`; a workspace that must not exfiltrate uses
`none`; nothing in between ships.

## Part 1 — Mechanism

### 1.1 The spec change

The OCI runtime spec controls network isolation through
`linux.namespaces`: adding a `NetworkNamespace` entry gives the container a
fresh, empty network namespace. With no CNI plugin, no veth pair, and no
configuration, that namespace contains exactly the `lo` interface runc brings
up — no routes, no DNS, no sockets reachable beyond loopback.

```go
type Config struct {
    // ...
    // Network selects the workspace's network posture.
    //   "" or "host": share the host's network namespace (the default;
    //                unchanged behavior — package installs, git remotes,
    //                and egress all work, and so does everything the host
    //                can reach).
    //   "none":       a fresh network namespace with only loopback — no
    //                egress, no ingress, DNS fails fast. The off-switch.
    Network string
}
```

Implementation: in `initSpecOpts`, when `cfg.Network == "none"`, append
`specs.LinuxNamespace{Type: specs.NetworkNamespace}` to the spec's namespaces;
`host` adds nothing (runc's default without a NEWNET entry is the host
namespace — the current behavior). Validation rejects any other value
loudly at `New`/`Create`, the config-typo convention everywhere else in croft.

### 1.2 What does not need doing

- **No DNS work**: with no interfaces, `resolv.conf` is irrelevant; lookups
  fail on unreachability. Do not touch the file.
- **No CNI, no bridge, no veth**: the empty namespace is complete. This is
  why the feature is small — it is one spec entry, not networking machinery.
- **InitArgs unaffected**: `sleep infinity` needs no network.
- **Exec unaffected**: processes inherit the task's namespaces.

### 1.3 Broker integration

The broker maps tenant policy onto `Config.Network` per sandbox — a tenant
that runs untrusted generated code wants `none` by default with `host` as an
explicit opt-in per `create_sandbox` call; a tenant doing routine dev work
wants the reverse. The broker's `create_sandbox` tool grows an optional
`network` argument, validated against the tenant's allowlist.

## Part 2 — Tasks (one task per commit, bisectable)

### Task 1 — Config + spec wiring
- `Config.Network`, validation in `Create` (reject unknown values loudly),
  the namespace entry in `initSpecOpts`.
- Fake test: the assembled spec carries the namespace entry iff `none`;
  unit tests for the validation error.

### Task 2 — Live proof
- `TestLive_NetworkNoneIsLoopbackOnly`: with `Network: "none"`,
  `ls /sys/class/net` inside the sandbox lists exactly `lo` — an assertion
  using only the image contract's tools (`ls`).
- `TestLive_NetworkHostSeesInterfaces`: with the default, the same listing
  shows the host's interfaces (the honest statement of what `host` means).
- A no-egress assertion without curl/wget (not in the image contract):
  `exec` a `sh -c 'echo > /dev/tcp/…'`-free probe is impossible in POSIX sh;
  the `lo`-only listing is the contract-honest proof, and the docs say so
  plainly rather than staging a curl image for the test.

### Task 3 — Docs + CHANGELOG v0.6.0
- README contracts section: the two postures stated with their consequences
  ("host" means the workspace can reach everything the host can — that is
  the default, and that is what `none` turns off); the broker policy note.
- `docs/overview.md`: configuration section gains `Network`.
- CHANGELOG: **Added** `Config.Network` (`host` default, `none`).

## Risks

- **Userns interactions**: under a uid-remapped daemon, a fresh netns is
  created the same way; verified by the live test, flagged if the deployment
  matrix ever adds remapping.
- **`host` is a footgun by default** — unchanged behavior, but now it is
  *documented* behavior with an alternative. The docs treat `none` as the
  recommendation for workloads running generated code.
- **No middle ground** — a workspace that needs exactly one registry
  reachable must use `host` for now. Deliberate (see Part 0); the egress
  proxy is the follow-on.

## Validation

1. `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` green;
   `go test ./... && go test -race ./...` green.
2. Live suite including both network tests against a real daemon.
3. Contract check: README names both postures and their consequences.

## Out of scope

- Controlled egress / allowlist proxy (follow-on; roadmap records it).
- Custom bridges, CNI integration, per-sandbox IP allocation.
- Inbound port publishing (a workspace is not a service).

## Correction: the premise was inverted (ruled 2026-10-15)

Part 0 called host networking "the biggest honesty gap" on the claim that
croft's containers share the host's network namespace. They do not, and
never did: containerd's default spec (`pkg/oci`'s `defaultUnixNamespaces`)
carries a `NetworkNamespace` entry, and an empty-path NEWNET entry makes
runc unshare into a fresh netns — loopback-only, since croft wires no CNI
(the same reason bare `ctr run` containers have no networking). No live
test exposed this because nothing in the suite needed container-side
networking.

The ruling keeps the default behavior unchanged in fact, which means
unchanged as *isolated*:

- `""` / `"none"`: today's posture — the default spec's private netns,
  untouched. The right default for untrusted code, and the one croft has
  always shipped.
- `"host"`: the mode that must act — `oci.WithHostNamespace` removes the
  NEWNET entry, runc skips the unshare, and the container joins the daemon
  host's namespace. An explicit opt-in for trusted workloads; this is what
  makes package installs and git remotes work, which Part 0 wrongly assumed
  they already did.
- Flipping the default to `host` (D6 as originally written) would silently
  grant every existing sandbox network access it never had — a security
  regression shipped as a "no default change" phase. Rejected.

Mechanism and task deltas from Parts 1–2 as landed: validation runs at
`Create` *and* `Apply` (a resumed sandbox never calls Create); the spec opt
is `WithHostNamespace` for `host`, nothing at all for `none`; and the live
proofs are `TestLive_NetworkNoneIsLoopbackOnly` (`lo`-only under `none` and
the identical `""`) plus `TestLive_NetworkHostSeesInterfaces` (the
container's listing equals the host's own `/sys/class/net`, skipping
honestly on a loopback-only host). Roadmap D6 carries the same correction.
