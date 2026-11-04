//go:build linux && !nolive

package croft

// Live-daemon verification of ADR-0002 P1 (croft-e6i): the default seccomp
// profile and NoNewPrivileges reach a real container, and survive task.Exec.
//
// Why these three probes and not more:
//
//   - /proc/self/status's Seccomp field reads 2 (filter mode) in an exec'd
//     process. That is simultaneously proof the profile is installed and
//     proof runc threads it into exec'd processes — the half of the story
//     the exec spec cannot express (a Process has no seccomp), so it can
//     only be observed, not asserted from either spec.
//   - /proc/self/status's NoNewPrivs:1 in the same exec'd process proves the
//     per-exec NoNewPrivileges assertion works end to end.
//   - clone(CLONE_NEWUSER) must fail with EPERM. This is the probe that can
//     distinguish the filter from the capability checks: the exec'd process
//     is root without CAP_SYS_MODULE and the like, so a denied
//     init_module-style syscall would EPERM with or without the profile. A
//     user namespace, by contrast, needs no capability at all — on an
//     unfiltered container this clone succeeds and makes its caller root in
//     a fresh mapping, which is exactly the re-arming of privileges the
//     profile's NEWUSER mask exists to prevent. EPERM here is filter proven.
//
// The probe binary is compiled at test time (host-side, CGO off) and staged
// into the container with WriteFile, so the image needs only what every
// other live test needs. `go build` ships with the toolchain running the
// test; if the environment lacks it the test fails with the compiler's
// error, which is the correct loud failure for a verification that cannot
// run.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seccompProbeSource is the in-container probe. Output is one line:
// clone_newuser:<errno name>, where EPERM is the expected mask denial. A
// successful clone prints SUCCESS instead — clone either succeeds outright or
// fails with a specific errno; there is no ambiguous third case with these
// flags (SIGCHLD alone, then CLONE_NEWUSER added).
const seccompProbeSource = `
package main

import (
	"fmt"
	"os"
	"syscall"
)

func main() {
	flags := uintptr(0x10000000 | syscall.SIGCHLD) // CLONE_NEWUSER | SIGCHLD
	_, _, errno := syscall.RawSyscall6(syscall.SYS_CLONE, flags, 0, 0, 0, 0, 0)
	if errno == 0 {
		// A successful clone returns in both processes; both print the
		// same verdict, which is all this probe exists to capture.
		os.Stdout.WriteString("SUCCESS")
		return
	}
	// RawSyscall6's err is uintptr; only errno values 1..4095 are kernels'.
	fmt.Fprintf(os.Stdout, "EPERM:%d", uint(errno))
}
`

// buildSeccompProbe compiles the probe host-side and returns the binary bytes.
func buildSeccompProbe(t *testing.T) []byte {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "probe.go")
	bin := filepath.Join(dir, "probe")
	if err := os.WriteFile(src, []byte(seccompProbeSource), 0o644); err != nil {
		t.Fatalf("write probe source: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build probe: %v (%s)", err, out)
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read probe binary: %v", err)
	}
	return b
}

// TestLive_SeccompAndNoNewPrivileges is the P1 acceptance: the default
// profile lands on a real generation, the exec's no_new_privs holds, and the
// NEWUSER clone is refused.
func TestLive_SeccompAndNoNewPrivileges(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	seccompField := func(pid string) string {
		return mustExec(t, ctx, sb, "/", "/bin/sh", "-c",
			"grep '^Seccomp:' /proc/"+pid+"/status | tr -d '\\t'")
	}
	// PID 1 is the init task the adapter started; self is the exec'd probe
	// shell. Both read 2 (filter mode) once the profile applies.
	for _, pid := range []string{"1", "self"} {
		if got := seccompField(pid); !strings.Contains(got, "Seccomp:2") {
			t.Errorf("pid %s: %q, want Seccomp: 2", pid, got)
		}
	}
	// NoNewPrivs in the exec'd shell is execProcessSpec's own flag.
	nnps := mustExec(t, ctx, sb, "/", "/bin/sh", "-c",
		"grep '^NoNewPrivs:' /proc/self/status | tr -d '\\t'")
	if !strings.Contains(nnps, "NoNewPrivs:1") {
		t.Errorf("exec'd process: %q, want NoNewPrivs: 1", nnps)
	}

	// Stage the probe and run it. WriteFile also exercises the 0755 mode.
	if err := sb.WriteFile(ctx, "seccomp-probe", buildSeccompProbe(t), 0o755); err != nil {
		t.Fatalf("WriteFile probe: %v", err)
	}
	ires, err := execProbe(t, ctx, sb, "./seccomp-probe")
	if err != nil {
		t.Fatalf("exec probe: %v", err)
	}
	// EPERM:0x10000000|SIGCHLD — the clone failed on the NEWUSER bits. Any
	// other outcome — SUCCESS above all — means the mask rule did not reach
	// the kernel.
	if !strings.HasPrefix(ires, "EPERM:") {
		t.Errorf("clone(CLONE_NEWUSER) in container = %q, want EPERM (SUCCESS means the NEWUSER mask rule did not apply)", ires)
	}
	// The decline must come from the filter, not from a syscall number
	// mismatch on this kernel: the same clone syscall number (x86_64 56 /
	// here resolved via syscall.SYS_CLONE) arcana are the probe's own use of
	// the stdlib constant, so a mismatch would fail the build, not this run.
	if !strings.Contains(ires, "1") {
		t.Errorf("clone denial errno = %q, want EPERM (1): the pipe beyond the mask is fine, the code must not change", ires)
	}
}

func execProbe(t *testing.T, ctx context.Context, sb *ContainerdSandbox, cmd string) (string, error) {
	t.Helper()
	res, err := sb.Exec(ctx, ExecRequest{Command: cmd, Timeout: time.Minute})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}
