package main

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

// forbiddenOwnership enumerates transport and reconciler responsibilities a
// manifest-producing plugin must never carry. It renders deterministic
// Kubernetes output from normalized inputs; how that output is transported,
// reviewed, or reconciled belongs to a separate promotion driver. The module
// being hosted on github.com is not a violation, so the patterns target Git
// operations, GitHub/Argo/Flux clients, and GitOps source bindings — never the
// bare "github.com" import host.
var forbiddenOwnership = []*regexp.Regexp{
	regexp.MustCompile(`\bgit (clone|push|commit|checkout|fetch|tag|remote|rev-parse)\b`),
	regexp.MustCompile(`(?i)go-git|libgit2|src-d/go-git`),
	regexp.MustCompile(`(?i)go-github|githubv4`),
	regexp.MustCompile(`(?i)pull request`),
	regexp.MustCompile(`(?i)argoproj|argo-?cd`),
	regexp.MustCompile(`(?i)fluxcd`),
	regexp.MustCompile(`\bAppProject\b`),
	regexp.MustCompile(`\brepoURL\b`),
	regexp.MustCompile(`\btargetRevision\b`),
	regexp.MustCompile(`(?m)^\s*kind:\s*Application\s*$`),
	regexp.MustCompile(`(?i)kubeconfig`),
	regexp.MustCompile(`(?i)kubectl apply`),
}

// TestManifestBoundary enforces the manifest-only plugin boundary: neither the
// runtime source nor the plugin-owned Kubernetes output may take on Git,
// GitHub, Argo, Flux, or direct-cluster ownership. Test files are excluded so
// this contract can name the forbidden concepts without tripping itself.
func TestManifestBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read module directory: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		assertNoForbiddenOwnership(t, name)
	}

	err = fs.WalkDir(deploymentFS, "templates/deployment", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		content, readErr := fs.ReadFile(deploymentFS, path)
		if readErr != nil {
			return readErr
		}
		assertContentNoForbiddenOwnership(t, path, content)
		return nil
	})
	if err != nil {
		t.Fatalf("walk deployment templates: %v", err)
	}
}

func assertNoForbiddenOwnership(t *testing.T, path string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	assertContentNoForbiddenOwnership(t, path, content)
}

func assertContentNoForbiddenOwnership(t *testing.T, path string, content []byte) {
	t.Helper()
	for _, pattern := range forbiddenOwnership {
		if match := pattern.Find(content); match != nil {
			t.Errorf("%s carries forbidden transport/reconciler ownership: %q", path, match)
		}
	}
}
