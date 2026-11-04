package croft

// defaultSeccompProfile is the seccomp filter croft applies to every container
// generation — init task, reader task, and everything exec'd inside them —
// unless Config.Seccomp names one explicitly.
//
// Why deny-list, not Docker's allowlist. Docker's default profile enumerates
// every syscall a container may use and sets DefaultAction to SCMP_ACT_ERRNO.
// That list is hundreds of entries long, must be rewritten when the kernel grows
// a syscall, and a forgotten entry breaks the workload croft actually hosts:
// arbitrary shells doing arbitrary dev work. croft inverts it: allow everything
// and deny the syscalls whose only use inside a sandbox is as an exploit
// primitive. The kernel's own capability checks stay as the second gate they
// always were — mount, for example, is allowed by this filter and still fails
// with EPERM without CAP_SYS_ADMIN, which is exactly the setpriv-dropped posture
// every post-shaping process has (authority.go). An allowlist remains available
// to callers who want Docker-grade narrowing: set Config.Seccomp.
//
// Everything denied here fails with EPERM (or ENOSYS where noted) rather than
// killing the process: a shell sandbox should observe "not permitted", not be
// SIGSYS'd out of existence, and libseccomp's ERRNO action keeps -1/errno
// semantics the caller already handles.
//
// Only syscall names that resolve on every architecture libseccomp supports are
// denied. x86-only legacies (iopl, ioperm, vm86, modify_ldt) and long-removed
// syscalls (create_module, uselib, _sysctl) are deliberately absent: an entry
// the daemon's libseccomp cannot resolve fails every container start, and on
// those architectures the names either do not exist or the syscalls are already
// gone.

import (
	"context"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// cloneNewUser is Linux's CLONE_NEWUSER flag (0x10000000): the argument bits
// that ask for a new user namespace. No workload croft hosts needs to mount a
// user namespace, and one granted later would hand back privileges the spec
// deliberately scatters — a UID-mapped namespace makes its owner root *inside*
// it, re-arming every capability check this profile and croftExecCapabilities
// deny. clone and clone3 are filtered below so user-namespace creation fails
// while thread and process creation keep working.
const cloneNewUser = 0x10000000

// errnoPerm is the uint form of EPERM, and errnoEnosys the uint form of ENOSYS;
// seccomp's ErrnoRet fields carry plain uints.
const (
	errnoPerm   = 1
	errnoEnosys = 38
)

// deniedSeccompSyscalls are the syscalls always refused inside a croft
// container. Grouped by what an exploit with them buys:
//
//   - Kernel-code injection / privilege escalation: module loading, kexec,
//     BPF, perf, userfaultfd (the page-fault race window), io_uring (the
//     submission path, once setup exists, is exploitable independently).
//   - Cross-process interference inside the sandbox: ptrace and the
//     process_vm cross-memory family. YAMA already narrows these kernel-side;
//     refusing them at the filter removes the policy dependency.
//   - Cryptographic-key material: the keyctl family, so an in-container
//     process cannot enumerate or read host keyrings the way it can't read
//     host files.
//   - Namepace escape or traversal: unshare and setns, so no new namespace
//     can be mounted from inside and no existing one joined. clone keeps
//     working for the namespaceless cases; see cloneNewUser.
//   - Host-state disruption from a shared daemon: swap and accounting
//     control, reboot, process accounting, by-handle file access (which
//     smuggles file descriptors across overlayfs inodes).
var deniedSeccompSyscalls = []string{
	"init_module",
	"finit_module",
	"delete_module",
	"kexec_load",
	"kexec_file_load",
	"bpf",
	"perf_event_open",
	"userfaultfd",
	"io_uring_setup",
	"ptrace",
	"process_vm_readv",
	"process_vm_writev",
	"keyctl",
	"add_key",
	"request_key",
	"unshare",
	"setns",
	"open_by_handle_at",
	"swapon",
	"swapoff",
	"acct",
	"reboot",
}

// defaultSeccompProfile builds the profile croft applies when Config.Seccomp is
// unset. DefaultAction is ALLOW — everything not named below passes — with
// three denial rules:
//
//   - deniedSeccompSyscalls, EPERM;
//   - clone, whose flags are argument 0, masked against CLONE_NEWUSER, EPERM:
//     any clone asking for a user namespace is refused, any other clone passes;
//   - clone3, ENOSYS. clone3 is the glibc ≥2.34 fast path for every thread and
//     process creation; its flags live behind a pointer seccomp cannot inspect,
//     so it must not be allowed unconditionally and must not be refused with
//     EPERM either — with EPERM glibc stops; with ENOSYS it falls back to the
//     filtered clone above and the NEWUSER mask still holds. This mirrors
//     Docker's handling.
func defaultSeccompProfile() *specs.LinuxSeccomp {
	perm := uint(errnoPerm)
	enosys := uint(errnoEnosys)
	cloneArgs := specs.LinuxSeccompArg{
		Index:    0,
		Value:    cloneNewUser,
		ValueTwo: cloneNewUser,
		Op:       specs.OpMaskedEqual,
	}
	return &specs.LinuxSeccomp{
		DefaultAction: specs.ActAllow,
		Syscalls: []specs.LinuxSyscall{
			{
				Names:    deniedSeccompSyscalls,
				Action:   specs.ActErrno,
				ErrnoRet: &perm,
			},
			{
				Names:    []string{"clone"},
				Action:   specs.ActErrno,
				ErrnoRet: &perm,
				Args:     []specs.LinuxSeccompArg{cloneArgs},
			},
			{
				Names:    []string{"clone3"},
				Action:   specs.ActErrno,
				ErrnoRet: &enosys,
			},
		},
	}
}

// withSeccompProfile assigns the profile onto the spec, replacing whatever was
// there. Assigning (not merging) is the whole knob: callers who need narrowing
// beyond the default profile bring their own and the default's deny rules are
// subsumed into theirs.
func withSeccompProfile(profile *specs.LinuxSeccomp) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		s.Linux.Seccomp = profile
		return nil
	}
}
