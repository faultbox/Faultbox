package docs

// String-grep gates for RFC-036 documentation. The feature can't ship
// without these landing pages — these tests fail fast if a future doc
// shuffle drops the section.
//
// Lives under docs/ as its own package so it can `go test` without
// importing the runtime. The package itself has no other purpose.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func docsRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return wd
}

func mustGrep(t *testing.T, path string, fragments ...string) {
	t.Helper()
	abs := filepath.Join(docsRoot(t), path)
	body, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := string(body)
	for _, f := range fragments {
		if !strings.Contains(s, f) {
			t.Errorf("%s missing fragment %q", path, f)
		}
	}
}

func TestDocs_SpecLanguageRemoteSection(t *testing.T) {
	mustGrep(t, "spec-language.md",
		"### Remote Services",
		"`service(remote=...)`",
		"healthcheck=` is **required**",
		"@faultbox/discovery/k8s.star",
		"`remotes(dict)`",
		"RFC-037",
	)
}

func TestDocs_ConnectivityGuide(t *testing.T) {
	mustGrep(t, "guides/connectivity.md",
		"Telepresence connect",
		"In-cluster execution",
		"kubectl port-forward",
		"VPN",
		"RFC-036",
		"RFC-037",
	)
}

func TestDocs_FeatureManifestRemoteRow(t *testing.T) {
	mustGrep(t, "feature-manifest.md",
		"`service(remote=...)` (RFC-036)",
		"`remotes()` typed per-interface override (RFC-036)",
		"`@faultbox/discovery/k8s.star` (RFC-036)",
	)
}
