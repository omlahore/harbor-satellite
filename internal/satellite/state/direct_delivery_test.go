package state

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestTarballFilename(t *testing.T) {
	tests := []struct {
		name   string
		entity Entity
		want   string
	}{
		{
			name:   "simple",
			entity: Entity{Repository: "library", Name: "nginx", Tag: "latest"},
			want:   "library--nginx--latest.tar",
		},
		{
			name:   "nested repository",
			entity: Entity{Repository: "project/repo", Name: "app", Tag: "v1.0"},
			want:   "project_repo--app--v1.0.tar",
		},
		{
			name:   "deep path",
			entity: Entity{Repository: "harbor/satellite/images", Name: "worker", Tag: "sha-abc123"},
			want:   "harbor_satellite_images--worker--sha-abc123.tar",
		},
		{
			name:   "no collision with slash vs underscore",
			entity: Entity{Repository: "foo/bar", Name: "baz", Tag: "v1"},
			want:   "foo_bar--baz--v1.tar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tarballFilename(tt.entity)
			if got != tt.want {
				t.Errorf("tarballFilename() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDigestMapPersistence(t *testing.T) {
	dir := t.TempDir()
	d := &DirectDeliverer{imageDir: dir}

	// Initially empty
	m := d.loadDigestMap()
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %v", m)
	}

	// Save and reload
	m["test.tar"] = "sha256:abc123"
	if err := d.saveDigestMap(m); err != nil {
		t.Fatalf("saveDigestMap: %v", err)
	}

	loaded := d.loadDigestMap()
	if loaded["test.tar"] != "sha256:abc123" {
		t.Errorf("loaded digest = %q, want %q", loaded["test.tar"], "sha256:abc123")
	}

	// Verify file content
	data, err := os.ReadFile(filepath.Join(dir, digestMapFile))
	if err != nil {
		t.Fatalf("read digest file: %v", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["test.tar"] != "sha256:abc123" {
		t.Errorf("file content mismatch")
	}
}

func TestDeleteRemovesFileAndDigest(t *testing.T) {
	dir := t.TempDir()
	d := &DirectDeliverer{imageDir: dir}

	// Create a fake tarball file and digest entry
	filename := tarballFilename(Entity{Repository: "lib", Name: "app", Tag: "v1"})
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte("fake tar"), 0o644); err != nil {
		t.Fatalf("write fake tarball: %v", err)
	}

	digests := map[string]string{filename: "sha256:old"}
	if err := d.saveDigestMap(digests); err != nil {
		t.Fatalf("save digests: %v", err)
	}

	// Delete the entity
	ctx := testContext()
	err := d.Delete(ctx, []Entity{{Repository: "lib", Name: "app", Tag: "v1"}})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// File should be gone
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file to be removed, got err: %v", err)
	}

	// Digest entry should be gone
	loaded := d.loadDigestMap()
	if _, ok := loaded[filename]; ok {
		t.Errorf("expected digest entry to be removed")
	}
}

func TestDeleteNonexistentFileNoError(t *testing.T) {
	dir := t.TempDir()
	d := &DirectDeliverer{imageDir: dir}

	ctx := testContext()
	err := d.Delete(ctx, []Entity{{Repository: "lib", Name: "gone", Tag: "v1"}})
	if err != nil {
		t.Fatalf("Delete nonexistent: %v", err)
	}
}

func TestDeliverEmptyEntitiesIsNoop(t *testing.T) {
	dir := t.TempDir()
	d := &DirectDeliverer{imageDir: dir}

	ctx := testContext()
	err := d.Deliver(ctx, nil)
	if err != nil {
		t.Fatalf("Deliver(nil): %v", err)
	}
	err = d.Deliver(ctx, []Entity{})
	if err != nil {
		t.Fatalf("Deliver([]): %v", err)
	}
}

func TestDeliverPicksPlatformFromIndex(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	idx := v1.ImageIndex(empty.Index)
	for _, arch := range []string{"amd64", "arm64"} {
		img, err := random.Image(64, 1)
		if err != nil {
			t.Fatalf("random.Image: %v", err)
		}
		img, err = mutate.ConfigFile(img, &v1.ConfigFile{OS: "linux", Architecture: arch})
		if err != nil {
			t.Fatalf("mutate.ConfigFile: %v", err)
		}
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}},
		})
	}
	ref, err := name.ParseReference(host+"/library/app:v1", name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}

	dir := t.TempDir()
	d := NewDirectDeliverer(dir, "", "", host, true)
	d.platform = &v1.Platform{OS: "linux", Architecture: "arm64"}
	entity := Entity{Repository: "library", Name: "app", Tag: "v1", Digest: "sha256:idx"}
	if err := d.Deliver(testContext(), []Entity{entity}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	img, err := tarball.ImageFromPath(filepath.Join(dir, tarballFilename(entity)), nil)
	if err != nil {
		t.Fatalf("ImageFromPath: %v", err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	if cfg.Architecture != "arm64" {
		t.Fatalf("tarball architecture = %q, want arm64", cfg.Architecture)
	}
}
