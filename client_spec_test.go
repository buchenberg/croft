package croft

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// configImage is a containerd.Image whose config descriptor carries the
// blob inline: content.ReadBlob serves Descriptor.Data directly (after
// checking the digest), so no content store is needed.
type configImage struct {
	containerd.Image
	desc v1.Descriptor
}

func (i configImage) Config(context.Context) (v1.Descriptor, error) {
	return i.desc, nil
}

// ContentStore satisfies the interface for the inline-data path: ReadBlob
// serves the descriptor's Data without consulting the store, so nil is
// never dereferenced.
func (configImage) ContentStore() content.Store { return nil }

// newConfigImage wraps an image config as the inline-blob image the
// init-spec tests read from.
func newConfigImage(t *testing.T, cfg v1.ImageConfig) configImage {
	t.Helper()
	blob, err := json.Marshal(v1.Image{
		Platform: v1.Platform{
			Architecture: "amd64",
			OS:           "linux",
		},
		Config: cfg,
	})
	if err != nil {
		t.Fatalf("marshal image config: %v", err)
	}
	return configImage{desc: v1.Descriptor{
		MediaType: v1.MediaTypeImageConfig,
		Digest:    digest.FromBytes(blob),
		Size:      int64(len(blob)),
		Data:      blob,
	}}
}

// generateInitSpec runs the adapter's init spec opts through the same
// GenerateSpec call NewContainer's WithNewSpec performs, so the tests pin
// the composition production actually uses — default spec, then image
// settings, then the init args — rather than each opt in isolation.
func generateInitSpec(t *testing.T, cfg v1.ImageConfig) (*oci.Spec, error) {
	t.Helper()
	return generateInitSpecWithConfig(t, Config{}, cfg)
}

// generateInitSpecWithConfig is generateInitSpec with the adapter's own
// Config set — the path Config.Limits (and any future spec-affecting knob)
// travels.
func generateInitSpecWithConfig(t *testing.T, scfg Config, cfg v1.ImageConfig) (*oci.Spec, error) {
	t.Helper()
	// GenerateSpec requires a containerd namespace (the default spec's
	// cgroup path resolves against it).
	ctx := namespaces.WithNamespace(context.Background(), "croft-test")
	img := newConfigImage(t, cfg)
	tasks := &containerdTasks{cfg: scfg}
	opts, err := tasks.initSpecOpts(ctx, img)
	if err != nil {
		return nil, err
	}
	opts = append(opts, oci.WithProcessArgs(tasks.initArgs()...))
	return oci.GenerateSpec(ctx, nil, &containers.Container{}, opts...)
}

// TestInitSpecOpts_Limits pins the D2 threading: a full Config.Limits
// arrives in the generated spec's Linux resources, which is exactly what
// NewContainer's WithNewSpec hands the runtime.
func TestInitSpecOpts_Limits(t *testing.T) {
	spec, err := generateInitSpecWithConfig(t, Config{Limits: &Limits{
		CPUShares:   512,
		CPUQuota:    50000,
		MemoryBytes: 192 << 20,
		Pids:        64,
	}}, v1.ImageConfig{})
	if err != nil {
		t.Fatalf("generateInitSpecWithConfig: %v", err)
	}

	res := spec.Linux.Resources
	if res == nil {
		t.Fatal("spec.Linux.Resources = nil, want the configured limits")
	}
	// Fatalf, not Errorf: the checks below dereference res.CPU, so a
	// regressed CPU mapping must stop here rather than panic on the very
	// assertion targeting it.
	if res.CPU == nil || res.CPU.Shares == nil || *res.CPU.Shares != 512 {
		t.Fatalf("CPU.Shares = %+v, want 512", res.CPU)
	}
	if res.CPU.Quota == nil || *res.CPU.Quota != 50000 {
		t.Errorf("CPU.Quota = %+v, want 50000", res.CPU.Quota)
	}
	if res.Memory == nil || res.Memory.Limit == nil || *res.Memory.Limit != 192<<20 {
		t.Errorf("Memory.Limit = %+v, want %d", res.Memory, int64(192<<20))
	}
	if res.Pids == nil || res.Pids.Limit == nil || *res.Pids.Limit != 64 {
		t.Errorf("Pids = %+v, want limit 64", res.Pids)
	}
	// The merge must preserve the default spec's device allowlist: a memory
	// cap is not a license to drop device containment.
	if len(res.Devices) == 0 {
		t.Error("Resources.Devices = empty, want the default spec's device allowlist preserved by the merge")
	}
}

// TestInitSpecOpts_NoLimitsLeavesResourcesUnset pins the unbounded default:
// no Limits (or an all-zero/non-positive one) adds no resource opt, so the
// spec's limit sections stay exactly as the default spec left them — CPU,
// Memory, and Pids unset, runtime defaults in force. (The default spec does
// carry a Resources stub with the device allowlist; that is
// GenerateSpec's own, not croft's.)
func TestInitSpecOpts_NoLimitsLeavesResourcesUnset(t *testing.T) {
	for name, scfg := range map[string]Config{
		"nil limits":   {},
		"zero limits":  {Limits: &Limits{}},
		"non-positive": {Limits: &Limits{CPUQuota: -1, MemoryBytes: -1, Pids: -1}},
	} {
		spec, err := generateInitSpecWithConfig(t, scfg, v1.ImageConfig{})
		if err != nil {
			t.Fatalf("%s: generateInitSpecWithConfig: %v", name, err)
		}
		res := spec.Linux.Resources
		if res == nil {
			continue // nothing set at all — trivially unbounded
		}
		if res.CPU != nil || res.Memory != nil || res.Pids != nil {
			t.Errorf("%s: resources = CPU %+v, Memory %+v, Pids %+v; want all unset (unbounded default)",
				name, res.CPU, res.Memory, res.Pids)
		}
	}
}

// TestLinuxResources pins the partial-limits mapping: only set fields are
// carried, so a memory-only Limits must not clobber CPU or pids defaults
// with zeros.
func TestLinuxResources(t *testing.T) {
	res := linuxResources(&Limits{MemoryBytes: 64 << 20})
	if res == nil {
		t.Fatal("linuxResources = nil, want memory-only resources")
	}
	if res.CPU != nil {
		t.Errorf("CPU = %+v, want nil (unset field passes nothing through)", res.CPU)
	}
	if res.Pids != nil {
		t.Errorf("Pids = %+v, want nil", res.Pids)
	}
	if res.Memory == nil || res.Memory.Limit == nil || *res.Memory.Limit != 64<<20 {
		t.Errorf("Memory = %+v, want limit %d", res.Memory, int64(64<<20))
	}

	// Quota alone yields a CPU section without shares.
	res = linuxResources(&Limits{CPUQuota: 25000})
	if res == nil || res.CPU == nil || res.CPU.Shares != nil || res.CPU.Quota == nil || *res.CPU.Quota != 25000 {
		t.Errorf("quota-only resources = %+v, want CPU{Quota: 25000} without shares", res)
	}
}

// TestInitSpecOpts_Network pins the posture mapping — and with it the
// corrected premise of the network phase: containerd's default spec already
// unshares the network namespace, so the isolated default ("", "none") needs
// no opt at all, and "host" is the mode that must change the spec, by
// removing the NEWNET entry so runc skips the unshare.
func TestInitSpecOpts_Network(t *testing.T) {
	hasNetNS := func(spec *oci.Spec) bool {
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type == specs.NetworkNamespace {
				return true
			}
		}
		return false
	}

	for name, scfg := range map[string]Config{
		"default": {},
		"none":    {Network: "none"},
	} {
		spec, err := generateInitSpecWithConfig(t, scfg, v1.ImageConfig{})
		if err != nil {
			t.Fatalf("%s: generateInitSpecWithConfig: %v", name, err)
		}
		if !hasNetNS(spec) {
			t.Errorf("%s: spec carries no network-namespace entry, want the default spec's private netns kept", name)
		}
	}

	spec, err := generateInitSpecWithConfig(t, Config{Network: "host"}, v1.ImageConfig{})
	if err != nil {
		t.Fatalf("host: generateInitSpecWithConfig: %v", err)
	}
	if hasNetNS(spec) {
		t.Errorf("host: spec still carries a network-namespace entry (%v), want it removed so runc shares the host's netns",
			spec.Linux.Namespaces)
	}
}

// TestInitSpecOpts pins the WithImageConfig parity this translation keeps:
// image env applied over the default spec env, the image working directory
// (with the "/" fallback), a numeric uid[:gid] user, and the init args.
func TestInitSpecOpts(t *testing.T) {
	spec, err := generateInitSpec(t, v1.ImageConfig{
		Env:        []string{"PATH=/opt/bin", "GOLANG=1"},
		WorkingDir: "/workdir",
		User:       "1000:1000",
	})
	if err != nil {
		t.Fatalf("generateInitSpec: %v", err)
	}

	env := strings.Join(spec.Process.Env, "\n")
	for _, want := range []string{"PATH=/opt/bin", "GOLANG=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("Process.Env = %q, want %s applied over the default env", spec.Process.Env, want)
		}
	}
	if spec.Process.Cwd != "/workdir" {
		t.Errorf("Process.Cwd = %q, want the image working directory", spec.Process.Cwd)
	}
	if spec.Process.User.UID != 1000 || spec.Process.User.GID != 1000 {
		t.Errorf("Process.User = %d:%d, want 1000:1000 from the image user",
			spec.Process.User.UID, spec.Process.User.GID)
	}
	wantArgs := []string{"/bin/sh", "-c", "sleep infinity"}
	if strings.Join(spec.Process.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("Process.Args = %q, want %q", spec.Process.Args, wantArgs)
	}
}

// TestInitSpecOpts_Defaults pins the fallbacks: no env leaves the spec's
// default env (which includes a PATH) untouched, and no working directory
// resolves to "/".
func TestInitSpecOpts_Defaults(t *testing.T) {
	spec, err := generateInitSpec(t, v1.ImageConfig{})
	if err != nil {
		t.Fatalf("generateInitSpec: %v", err)
	}
	if !strings.Contains(strings.Join(spec.Process.Env, "\n"), "PATH=") {
		t.Errorf("Process.Env = %q, want the default spec PATH preserved", spec.Process.Env)
	}
	if spec.Process.Cwd != "/" {
		t.Errorf("Process.Cwd = %q, want the / fallback", spec.Process.Cwd)
	}
}

// TestInitSpecOpts_UserForms pins how image user forms map onto the spec.
// Named users and malformed pairs are ignored as a whole (spec default
// root), and a uid-only user sets the uid and leaves the gid at 0. An id
// that is syntactically numeric but beyond runc's MaxInt32 bound fails
// the spec loudly — silently falling back to root would change the
// identity the image asked for — matching containerd's own WithUser
// range check.
func TestInitSpecOpts_UserForms(t *testing.T) {
	cases := []struct {
		user    string
		wantUID uint32
		wantGID uint32
		wantErr string // empty: the user must apply (or fall back) cleanly
	}{
		{user: "1000", wantUID: 1000},
		{user: "1000:1000", wantUID: 1000, wantGID: 1000},
		{user: "appuser"},                                  // named: unresolved by design
		{user: "1000:staff"},                               // malformed pair: applied as a whole or not at all
		{user: "4294967295", wantErr: "outside the range"}, // MaxUint32
		{user: "2147483648", wantErr: "outside the range"}, // MaxInt32 + 1
		{user: "1000:4294967295", wantErr: "outside the range"},
	}
	for _, tc := range cases {
		spec, err := generateInitSpec(t, v1.ImageConfig{User: tc.user})
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("user %q: error = %v, want %q", tc.user, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("user %q: generateInitSpec: %v", tc.user, err)
		}
		if spec.Process.User.UID != tc.wantUID || spec.Process.User.GID != tc.wantGID {
			t.Errorf("user %q: Process.User = %d:%d, want %d:%d",
				tc.user, spec.Process.User.UID, spec.Process.User.GID, tc.wantUID, tc.wantGID)
		}
	}
}

// TestInitSpecOpts_RejectsNonConfigMediaType pins WithImageConfig's own
// guard: a descriptor that is not an image config fails loudly instead of
// unmarshalling into a zero-value config.
func TestInitSpecOpts_RejectsNonConfigMediaType(t *testing.T) {
	img := configImage{desc: v1.Descriptor{
		MediaType: v1.MediaTypeImageManifest,
	}}
	tasks := &containerdTasks{}
	_, err := tasks.initSpecOpts(context.Background(), img)
	if err == nil {
		t.Fatal("a non-config media type must be rejected")
	}
	if !strings.Contains(err.Error(), "media type") {
		t.Errorf("error = %v, want the media type named", err)
	}
}
