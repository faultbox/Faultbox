package bundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResourceManifestRejectsEscapesBeforeWriting(t *testing.T) {
	for _, kind := range []string{"parent", "absolute", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			path := "../escaped"
			if kind == "absolute" {
				path = filepath.Join(outside, "created")
			}
			if kind == "symlink" {
				if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				path = "link/created"
			}
			data, err := json.Marshal(ResourceManifest{Version: 1, Resources: []Resource{{Source: "fixture", Path: path, Directory: true}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ResourceManifestName), data, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := PrepareResources(root); err == nil {
				t.Fatal("accepted escaping resource path")
			}
			if _, err := os.Stat(filepath.Join(outside, "created")); !os.IsNotExist(err) {
				t.Fatalf("wrote outside extraction directory: %v", err)
			}
		})
	}
}
