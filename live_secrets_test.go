//go:build linux && !nolive

// Live-daemon acceptance for Config.Secrets and Config.StagingDir (phase 6):
// secret mounts must be readable inside the container and staging writes must
// round-trip without leaving residue. The spec-level mapping is pinned by
// TestInitSpecOpts_Secrets and TestInitSpecOpts_StagingDir; this closes the
// loop through containerd and runc.
//
// Run (root, like the rest of the live suite):
//
//	sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test -count=1 -v -run TestLive_Secret ./...
package croft

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLive_SecretMountReadable starts a sandbox with a secret bind mount and
// reads the file back from inside the container: content must match exactly
// and the file must be readable at the configured destination.
func TestLive_SecretMountReadable(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	// Write the secret to a host temp file.
	secretContent := "ghp_test-token-12345\n"
	secretPath := filepath.Join(t.TempDir(), "git-token")
	if err := os.WriteFile(secretPath, []byte(secretContent), 0o600); err != nil {
		t.Fatalf("write host secret: %v", err)
	}

	c := dial(t, cfg.Addr)
	ensureNamespace(t, ctx, c, cfg.Namespace)
	if err := c.Close(); err != nil {
		t.Logf("close setup client: %v", err)
	}

	sb := New(Config{
		Address:     cfg.Addr,
		Namespace:   cfg.Namespace,
		Image:       cfg.Image,
		Snapshotter: cfg.Snapshotter,
		Workdir:     cfg.Workdir,
		FIFODir:     t.TempDir(),
		Secrets: []SecretMount{{
			Source:      secretPath,
			Destination: "/run/secrets/git-token",
		}},
	})
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sb.Destroy(dctx); err != nil {
			t.Errorf("Destroy during cleanup: %v", err)
		}
	})
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := mustExec(t, ctx, sb, "/", "cat", "/run/secrets/git-token")
	if got != secretContent {
		t.Errorf("secret file content = %q, want %q", got, secretContent)
	}
}

// TestLive_WriteViaStagingRoundTrip starts a sandbox with StagingDir set and
// writes a file through the argv-free path, then reads it back: the content
// must round-trip and no staging residue must remain.
func TestLive_WriteViaStagingRoundTrip(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	staging := t.TempDir()
	c := dial(t, cfg.Addr)
	ensureNamespace(t, ctx, c, cfg.Namespace)
	if err := c.Close(); err != nil {
		t.Logf("close setup client: %v", err)
	}

	sb := New(Config{
		Address:     cfg.Addr,
		Namespace:   cfg.Namespace,
		Image:       cfg.Image,
		Snapshotter: cfg.Snapshotter,
		Workdir:     cfg.Workdir,
		FIFODir:     t.TempDir(),
		StagingDir:  staging,
	})
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sb.Destroy(dctx); err != nil {
			t.Errorf("Destroy during cleanup: %v", err)
		}
	})
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	content := []byte("staged write round-trip\n")
	if err := sb.WriteFile(ctx, "output.txt", content, 0o644); err != nil {
		t.Fatalf("WriteFile via staging: %v", err)
	}

	got := mustExec(t, ctx, sb, "/", "cat", "/workspace/output.txt")
	if got != string(content) {
		t.Errorf("file content = %q, want %q", got, string(content))
	}

	// No staging residue: the host staging directory must be empty.
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("staging dir has leftover entries: %v", strings.Join(names, ", "))
	}

	// No croft-staging residue inside the container.
	got = mustExec(t, ctx, sb, "/", "ls", "/run/croft-staging")
	if strings.TrimSpace(got) != "" {
		t.Errorf("container staging mount has residue: %q", got)
	}
}
