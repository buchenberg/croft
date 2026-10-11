package containerd

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
func generateInitSpec(t *testing.T, cfg v1.ImageConfig) *oci.Spec {
	t.Helper()
	// GenerateSpec requires a containerd namespace (the default spec's
	// cgroup path resolves against it).
	ctx := namespaces.WithNamespace(context.Background(), "shepherd-test")
	img := newConfigImage(t, cfg)
	tasks := &containerdTasks{}
	opts, err := tasks.initSpecOpts(ctx, img)
	if err != nil {
		t.Fatalf("initSpecOpts: %v", err)
	}
	opts = append(opts, oci.WithProcessArgs(tasks.initArgs()...))
	spec, err := oci.GenerateSpec(ctx, nil, &containers.Container{}, opts...)
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}
	return spec
}

// TestInitSpecOpts pins the WithImageConfig parity this translation keeps:
// image env applied over the default spec env, the image working directory
// (with the "/" fallback), a numeric uid[:gid] user, and the init args.
func TestInitSpecOpts(t *testing.T) {
	spec := generateInitSpec(t, v1.ImageConfig{
		Env:        []string{"PATH=/opt/bin", "GOLANG=1"},
		WorkingDir: "/workdir",
		User:       "1000:1000",
	})

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
	spec := generateInitSpec(t, v1.ImageConfig{})
	if !strings.Contains(strings.Join(spec.Process.Env, "\n"), "PATH=") {
		t.Errorf("Process.Env = %q, want the default spec PATH preserved", spec.Process.Env)
	}
	if spec.Process.Cwd != "/" {
		t.Errorf("Process.Cwd = %q, want the / fallback", spec.Process.Cwd)
	}
}

// TestInitSpecOpts_UserForms pins how image user forms map onto the spec.
// Named users and malformed pairs are ignored as a whole (spec default
// root), a uid-only user sets the uid and leaves the gid at 0, and ids
// beyond runc's MaxInt32 bound are rejected even though the field is
// uint32 — containerd's own WithUser enforces the same bound.
func TestInitSpecOpts_UserForms(t *testing.T) {
	cases := []struct {
		user    string
		wantUID uint32
		wantGID uint32
	}{
		{user: "1000", wantUID: 1000},
		{user: "1000:1000", wantUID: 1000, wantGID: 1000},
		{user: "appuser"},    // named: unresolved by design
		{user: "1000:staff"}, // malformed pair: applied as a whole or not at all
		{user: "4294967295"}, // MaxUint32: parses as uint32, runc rejects
		{user: "2147483648"}, // MaxInt32 + 1: runc rejects
	}
	for _, tc := range cases {
		spec := generateInitSpec(t, v1.ImageConfig{User: tc.user})
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
