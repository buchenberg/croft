package croft

import (
	"context"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestValidateWritableRoots(t *testing.T) {
	tests := []struct {
		name    string
		workdir string
		roots   []string
		wantErr bool
	}{
		{name: "none", workdir: "/workspace"},
		{name: "workspace itself", workdir: "/workspace", roots: []string{"/workspace"}},
		{name: "nested", workdir: "/workspace", roots: []string{"/workspace/src", "/workspace/pkg"}},
		{name: "custom workdir", workdir: "/work", roots: []string{"/work/a"}},
		{name: "relative", workdir: "/workspace", roots: []string{"src"}, wantErr: true},
		{name: "outside", workdir: "/workspace", roots: []string{"/etc"}, wantErr: true},
		{name: "escape", workdir: "/workspace", roots: []string{"/workspace/../etc"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Config{Workdir: tt.workdir, WritableRoots: tt.roots}).validateWritableRoots()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateWritableRoots() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeRootsOrdersShallowFirstAndDeduplicates(t *testing.T) {
	got := normalizeRoots([]string{"/workspace/a/b", "/workspace/a", "/workspace/a/b", "/workspace"})
	want := []string{"/workspace", "/workspace/a", "/workspace/a/b"}
	if len(got) != len(want) {
		t.Fatalf("normalizeRoots() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("normalizeRoots()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWritableRootsScriptShapesWorkspaceThenGrants(t *testing.T) {
	script := writableRootsScript("/workspace", []string{"/workspace/src", "/workspace"})
	for _, want := range []string{
		"mkdir -p '/workspace'",
		"mount --bind '/workspace' '/workspace'",
		"mount -o remount,bind,ro '/workspace'",
		"mount --bind '/workspace/src' '/workspace/src'",
		"mount -o remount,bind,rw '/workspace/src'",
		"setpriv --bounding-set=-sys_admin --inh-caps=-sys_admin --ambient-caps=-sys_admin --no-new-privs",
		`-- "$@"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "/workspace'") > strings.Index(script, "/workspace/src'") {
		t.Errorf("workspace root must be mounted before a nested grant:\n%s", script)
	}
}

func TestWritableRootsScriptMasksReadOnlyWorkspace(t *testing.T) {
	script := writableRootsScript("/workspace", nil)
	if !strings.Contains(script, "mount -o remount,bind,ro '/workspace'") {
		t.Fatalf("read-only mask missing from script:\n%s", script)
	}
	if strings.Contains(script, "mount -o remount,bind,rw") {
		t.Fatalf("a mask with no grants must not remount anything writable:\n%s", script)
	}
}

func TestShapedInitArgsWrapsWhenShapingIsEnabled(t *testing.T) {
	base := []string{"/bin/sh", "-c", "sleep infinity"}
	if got := shapedInitArgs("/workspace", nil, false, base); strings.Join(got, "\x00") != strings.Join(base, "\x00") {
		t.Errorf("unshaped init = %v, want %v", got, base)
	}
	for name, args := range map[string]struct {
		roots []string
		mask  bool
	}{
		"with a writable grant": {roots: []string{"/workspace/src"}},
		"read-only workspace":   {mask: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := shapedInitArgs("/workspace", args.roots, args.mask, base)
			if len(got) < len(base)+4 || got[0] != "/bin/sh" || got[1] != "-c" || got[3] != "croft-init" {
				t.Fatalf("shaped init = %v, want a shell wrapper", got)
			}
			if strings.Join(got[4:], "\x00") != strings.Join(base, "\x00") {
				t.Errorf("shaped init lost the configured command: %v", got)
			}
		})
	}
}

func TestShapingEnabledIncludesReadOnlyMask(t *testing.T) {
	if (Config{}).shapingEnabled() {
		t.Fatal("an empty config must keep the historical unshaped posture")
	}
	if !(Config{MaskWorkspace: true}).shapingEnabled() {
		t.Fatal("MaskWorkspace alone must enable shaping")
	}
	if !(Config{WritableRoots: []string{"/workspace/src"}}).shapingEnabled() {
		t.Fatal("a writable grant must enable shaping")
	}
}

func TestExecCapabilitiesDropSysAdmin(t *testing.T) {
	caps := execCapabilities()
	if caps == nil {
		t.Fatal("execCapabilities returned nil")
	}
	for _, list := range [][]string{caps.Bounding, caps.Effective, caps.Permitted} {
		for _, cap := range list {
			if cap == "CAP_SYS_ADMIN" {
				t.Fatal("user execs must not receive CAP_SYS_ADMIN")
			}
		}
	}
}

func TestWritableRootsCapabilityAddsSysAdmin(t *testing.T) {
	spec := &oci.Spec{Process: &specs.Process{}}
	if err := withWritableRootsCapability()(context.Background(), nil, nil, spec); err != nil {
		t.Fatalf("apply capability: %v", err)
	}
	found := false
	for _, cap := range spec.Process.Capabilities.Effective {
		if cap == "CAP_SYS_ADMIN" {
			found = true
		}
	}
	if !found {
		t.Fatal("init shaping capability was not added")
	}
}
