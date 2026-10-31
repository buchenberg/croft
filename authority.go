package croft

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// croftExecCapabilities is the default containerd capability set minus
// CAP_SYS_ADMIN. User execs receive it explicitly, so a shaped workspace's
// read-only mounts cannot be remounted from inside the container. The init
// process runs the shaping script with CAP_SYS_ADMIN added; it never becomes a
// user-controlled exec.
var croftExecCapabilities = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FSETID",
	"CAP_FOWNER",
	"CAP_MKNOD",
	"CAP_NET_RAW",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETFCAP",
	"CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT",
	"CAP_KILL",
	"CAP_AUDIT_WRITE",
}

// withWritableRootsCapability grants the init process only the capability
// needed to perform the bind/remount shaping. Exec processes do not inherit it:
// containerdTasks.Exec sets croftExecCapabilities explicitly.
func withWritableRootsCapability() oci.SpecOpts {
	return oci.WithAddedCapabilities([]string{"CAP_SYS_ADMIN"})
}

// shapedInitArgs wraps the configured init command in a mount-shaping shell.
// The wrapper runs before the user's init, so every generation (Create, Apply,
// and a recovery restart) gets the same shaped workspace. The original init
// command is exec'd after the mounts land.
func shapedInitArgs(workdir string, roots []string, init []string) []string {
	if len(roots) == 0 {
		return init
	}
	script := writableRootsScript(workdir, roots)
	args := []string{"/bin/sh", "-c", script, "croft-init"}
	return append(args, init...)
}

// writableRootsScript builds the in-container shaping step. The workspace root
// is bind-mounted over itself and made read-only; each granted subtree is then
// bind-mounted over itself and made read-write. Because the mounts alias the
// same overlay, writes still land in the one writable layer Capture/Compare
// consume.
//
// Grants are ordered shallowest-first so a nested grant is mounted after its
// parent and wins. Paths are quoted as shell single-quoted words.
func writableRootsScript(workdir string, roots []string) string {
	cleanRoots := normalizeRoots(roots)
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("command -v mount >/dev/null 2>&1 || { echo 'croft: mount is required for writable-root shaping' >&2; exit 127; }\n")
	fmt.Fprintf(&b, "mkdir -p %s\n", shellQuote(workdir))
	// Create every grant before the workspace view becomes read-only; a grant
	// that names a new subtree would otherwise fail its own mkdir.
	for _, root := range cleanRoots {
		fmt.Fprintf(&b, "mkdir -p %s\n", shellQuote(root))
	}
	fmt.Fprintf(&b, "mount --bind %s %s\n", shellQuote(workdir), shellQuote(workdir))
	fmt.Fprintf(&b, "mount -o remount,bind,ro %s\n", shellQuote(workdir))
	for _, root := range cleanRoots {
		fmt.Fprintf(&b, "mount --bind %s %s\n", shellQuote(root), shellQuote(root))
		fmt.Fprintf(&b, "mount -o remount,bind,rw %s\n", shellQuote(root))
	}
	// The shaping init is the only process with CAP_SYS_ADMIN. Drop it before
	// handing control to InitArgs: the shell's child must not be able to
	// remount the read-only mask. Require a real capability-dropping utility
	// rather than silently leaving the init privileged.
	b.WriteString(`if command -v capsh >/dev/null 2>&1; then
  exec capsh --drop=cap_sys_admin --inh= --noamb -- -c 'exec "$@"' croft-init "$@"
fi
if command -v setpriv >/dev/null 2>&1; then
  exec setpriv --bounding-set=-sys_admin --inh-caps=-sys_admin --ambient-caps=-sys_admin --no-new-privs -- "$@"
fi
echo 'croft: capsh or setpriv is required to drop CAP_SYS_ADMIN after writable-root shaping' >&2
exit 127
`)
	return b.String()
}

// normalizeRoots sorts by path depth (shallow first, so deeper mounts land
// later) and removes duplicate paths.
func normalizeRoots(roots []string) []string {
	seen := make(map[string]bool, len(roots))
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		clean := path.Clean(root)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], "/"), strings.Count(out[j], "/")
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// startTaskAndProbe starts a generation and runs the writable-root probe
// before returning it to the caller.
func (s *ContainerdSandbox) startTaskAndProbe(ctx context.Context, id, snapshotKey string) error {
	if err := s.tasks.StartTask(ctx, id, snapshotKey); err != nil {
		return err
	}
	if err := s.probeWritableRoots(ctx, id); err != nil {
		// The task is live; stop it before callers remove the snapshot it is
		// rooted at. Use a cancel-free context: the probe may have failed
		// because the caller's deadline expired.
		_ = s.tasks.StopTask(context.WithoutCancel(ctx), id)
		return err
	}
	return nil
}

// probeWritableRoots checks the shaped workspace before it is handed to a
// caller: a canary write outside the grants must fail, and one inside each
// grant must succeed. It is a launch-time fail-closed check, not a policy
// probe the model can observe.
func (s *ContainerdSandbox) probeWritableRoots(ctx context.Context, id string) error {
	roots := normalizeRoots(s.cfg.WritableRoots)
	if len(roots) == 0 {
		return nil
	}
	workdir := path.Clean(s.cfg.workdir())
	for _, root := range roots {
		probe := path.Join(root, ".croft-authority-probe")
		res, err := s.probeExec(ctx, id, probe)
		if err != nil {
			return fmt.Errorf("containerd sandbox: writable-root probe inside %s: %w", root, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("containerd sandbox: writable-root probe inside %s failed (exit %d: %s)", root, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		if err := s.cleanupProbe(ctx, id, probe); err != nil {
			return fmt.Errorf("containerd sandbox: clean writable-root probe in %s: %w", root, err)
		}
	}

	// If the whole workspace is granted, there is no path outside the grants
	// to probe. Otherwise the workspace root itself is read-only and supplies
	// the negative canary.
	if !pathCovered(workdir, roots) {
		probe := path.Join(workdir, ".croft-authority-probe-outside")
		res, err := s.probeExec(ctx, id, probe)
		if err != nil {
			return fmt.Errorf("containerd sandbox: writable-root probe outside the grants: %w", err)
		}
		if res.ExitCode == 0 {
			_ = s.cleanupProbe(ctx, id, probe)
			return fmt.Errorf("containerd sandbox: writable-root probe wrote outside the grants at %s; the workspace mask is not enforced", probe)
		}
		if err := s.cleanupProbe(ctx, id, probe); err != nil {
			return fmt.Errorf("containerd sandbox: clean outside writable-root probe: %w", err)
		}
	}
	return nil
}

func (s *ContainerdSandbox) probeExec(ctx context.Context, id, p string) (ExecResult, error) {
	return s.tasks.Exec(ctx, id, ExecRequest{
		Command: "sh",
		Args:    []string{"-c", `touch "$1"`, "croft-probe", p},
		Cwd:     "/",
	})
}

func (s *ContainerdSandbox) cleanupProbe(ctx context.Context, id, p string) error {
	res, err := s.tasks.Exec(ctx, id, ExecRequest{
		Command: "sh",
		Args:    []string{"-c", `rm -f -- "$1"`, "croft-probe", p},
		Cwd:     "/",
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func pathCovered(p string, roots []string) bool {
	for _, root := range roots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

// execCapabilities returns a fresh copy so callers cannot mutate the package
// table through a process spec.
func execCapabilities() *specs.LinuxCapabilities {
	caps := append([]string(nil), croftExecCapabilities...)
	return &specs.LinuxCapabilities{
		Bounding:  caps,
		Effective: caps,
		Permitted: caps,
	}
}
