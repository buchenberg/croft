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
		"capsh --drop=cap_sys_admin --inh= --noamb -- -c",
		`exec "$@"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "/workspace'") > strings.Index(script, "/workspace/src'") {
		t.Errorf("workspace root must be mounted before a nested grant:\n%s", script)
	}
}

func TestShapedInitArgsWrapsOnlyWhenRootsExist(t *testing.T) {
	base := []string{"/bin/sh", "-c", "sleep infinity"}
	if got := shapedInitArgs("/workspace", nil, base); strings.Join(got, "\x00") != strings.Join(base, "\x00") {
		t.Errorf("unshaped init = %v, want %v", got, base)
	}
	got := shapedInitArgs("/workspace", []string{"/workspace/src"}, base)
	if len(got) < len(base)+4 || got[0] != "/bin/sh" || got[1] != "-c" || got[3] != "croft-init" {
		t.Fatalf("shaped init = %v, want a shell wrapper", got)
	}
	if strings.Join(got[4:], "\x00") != strings.Join(base, "\x00") {
		t.Errorf("shaped init lost the configured command: %v", got)
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
