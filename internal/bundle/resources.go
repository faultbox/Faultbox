package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const ResourceManifestName = ".faultbox-resources.json"

type Resource struct {
	Source    string `json:"source"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	Directory bool   `json:"directory,omitempty"`
}

type ResourceManifest struct {
	Version   int               `json:"version"`
	BaseDir   string            `json:"base_dir"`
	Binaries  map[string]string `json:"binaries,omitempty"`
	Resources []Resource        `json:"resources"`
}

// PrepareResources validates captured inputs before any replayed code runs.
// Paths must stay inside the extracted tree, including after symlink resolution.
func PrepareResources(root string) (*ResourceManifest, error) {
	data, err := os.ReadFile(filepath.Join(root, ResourceManifestName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m ResourceManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("unsupported resource manifest version %d", m.Version)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	for _, r := range m.Resources {
		if !filepath.IsLocal(r.Path) || r.Path == "." && !r.Directory {
			return nil, fmt.Errorf("unsafe resource path %q", r.Path)
		}
		p := filepath.Join(root, r.Path)
		// Reject existing symlink components before creating directories or
		// chmod'ing files, not only after resolving the final path.
		current := root
		for _, part := range strings.Split(filepath.Clean(r.Path), string(filepath.Separator)) {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("symlink in resource path: %s", r.Path)
			}
		}
		if r.Directory {
			if err := os.MkdirAll(p, 0755); err != nil {
				return nil, err
			}
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, fmt.Errorf("resource %s: %w", r.Source, err)
		}
		if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
			return nil, fmt.Errorf("resource escapes replay directory: %s", r.Path)
		}
		if r.Directory {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != r.SHA256 {
			return nil, fmt.Errorf("resource digest mismatch: %s", r.Source)
		}
		if err := os.Chmod(p, os.FileMode(r.Mode)&0777); err != nil {
			return nil, err
		}
	}
	return &m, nil
}
