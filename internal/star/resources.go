package star

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/faultbox/Faultbox/internal/bundle"
	"go.starlark.net/starlark"
)

// resource() declares a file/directory consumed by the SUT (config, fixtures,
// working directory, volume source). It returns its resolved local path.
func (rt *Runtime) builtinResource(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs("resource", args, kwargs, "path", &path); err != nil {
		return nil, err
	}
	p := rt.resolveCallerPath(thread, path)
	if err := rt.captureResource(p); err != nil {
		return nil, err
	}
	return starlark.String(p), nil
}

func (rt *Runtime) captureResource(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.IsDir() {
		rt.resourceMu.Lock()
		if rt.resourceDirs == nil {
			rt.resourceDirs = make(map[string]bool)
		}
		rt.resourceDirs[abs] = true
		rt.resourceMu.Unlock()
		return filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("resource directory contains symlink: %s; declare the target separately", p)
			}
			if d.IsDir() {
				return nil
			}
			if d.Name() == bundle.ResourceManifestName {
				return nil
			}
			return rt.captureResource(p)
		})
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("resource is not a regular file: %s", abs)
	}
	if info.Size() > 512*1024*1024 {
		return fmt.Errorf("resource exceeds 512 MiB: %s", abs)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	rt.resourceMu.Lock()
	defer rt.resourceMu.Unlock()
	if rt.loadedSpecs == nil {
		rt.loadedSpecs = make(map[string][]byte)
	}
	if rt.resourceModes == nil {
		rt.resourceModes = make(map[string]uint32)
	}
	rt.loadedSpecs[abs] = data
	rt.resourceModes[abs] = uint32(info.Mode().Perm())
	return nil
}

func (rt *Runtime) captureBinary(svc *ServiceDef) error {
	p := svc.Binary
	if rt.replayResources != nil {
		if source, ok := rt.replayResources.Binaries[svc.Name]; ok {
			p = source
		}
		p = rt.resolveSpecPath(p)
	}
	if !filepath.IsAbs(p) {
		var err error
		p, err = exec.LookPath(p)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				rt.events.Emit("resource_missing", svc.Name, map[string]string{"path": svc.Binary, "error": err.Error()})
				return nil
			}
			return err
		}
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	if err := rt.captureResource(p); err != nil {
		// Failed launches have no executable to archive. Keep the existing
		// launch/validation error precedence and make the missing input visible.
		if errors.Is(err, os.ErrNotExist) {
			rt.events.Emit("resource_missing", svc.Name, map[string]string{"path": p, "error": err.Error()})
			return nil
		}
		return fmt.Errorf("capture binary %s: %w", svc.Name, err)
	}
	rt.resourceMu.Lock()
	if rt.binarySources == nil {
		rt.binarySources = make(map[string]string)
	}
	rt.binarySources[svc.Name] = p
	rt.resourceMu.Unlock()
	svc.Binary = p
	return nil
}

func (rt *Runtime) resourceKey(path string) string {
	// Preserve the layout of explicitly declared external directories.
	var parents []string
	for dir := range rt.resourceDirs {
		if path != dir && strings.HasPrefix(path, dir+string(filepath.Separator)) {
			parents = append(parents, dir)
		}
	}
	sort.Slice(parents, func(i, j int) bool { return len(parents[i]) < len(parents[j]) })
	if len(parents) > 0 {
		dir := parents[0]
		rel, _ := filepath.Rel(dir, path)
		return filepath.ToSlash(filepath.Join(rt.bundleSpecKey(dir), rel))
	}
	return rt.bundleSpecKey(path)
}

func (rt *Runtime) resourceManifest() bundle.ResourceManifest {
	m := bundle.ResourceManifest{Version: 1, BaseDir: rt.baseDir, Binaries: rt.binarySources}
	var paths []string
	for path := range rt.loadedSpecs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		hash := sha256.Sum256(rt.loadedSpecs[path])
		mode := uint32(0644)
		if v, ok := rt.resourceModes[path]; ok {
			mode = v
		}
		m.Resources = append(m.Resources, bundle.Resource{Source: path, Path: rt.resourceKey(path), SHA256: hex.EncodeToString(hash[:]), Mode: mode})
	}
	for path := range rt.resourceDirs {
		m.Resources = append(m.Resources, bundle.Resource{Source: path, Path: rt.resourceKey(path), Directory: true})
	}
	if rt.replayResources != nil {
		m.BaseDir = rt.replayResources.BaseDir
		current := append([]bundle.Resource(nil), m.Resources...)
		for _, old := range rt.replayResources.Resources {
			resolved := rt.resolveSpecPath(old.Source)
			for _, r := range current {
				if r.Source == resolved && r.Source != old.Source {
					r.Source = old.Source
					m.Resources = append(m.Resources, r)
					break
				}
			}
		}
	}
	sort.Slice(m.Resources, func(i, j int) bool { return m.Resources[i].Source < m.Resources[j].Source })
	return m
}

func (rt *Runtime) bundledResources() map[string][]byte {
	out := make(map[string][]byte, len(rt.loadedSpecs)+1)
	for path, data := range rt.loadedSpecs {
		out[rt.resourceKey(path)] = data
	}
	data, _ := json.MarshalIndent(rt.resourceManifest(), "", "  ")
	out[bundle.ResourceManifestName] = data
	return out
}
