package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// forbiddenOwnership enumerates transport and reconciler responsibilities a
// manifest-producing plugin must never carry. It renders deterministic
// Kubernetes output from normalized inputs; how that output is transported,
// reviewed, or reconciled belongs to a separate promotion driver. The module
// being hosted on github.com is not a violation, so the patterns target Git
// operations, GitHub/Argo/Flux clients, direct-cluster clients, and GitOps
// source bindings — never the bare "github.com" import host.
var forbiddenOwnership = []*regexp.Regexp{
	regexp.MustCompile(`\bgit (clone|push|commit|checkout|fetch|tag|remote|rev-parse)\b`),
	regexp.MustCompile(`exec\.Command(?:Context)?\([^)]*"git"`),
	regexp.MustCompile(`(?i)go-git|libgit2`),
	regexp.MustCompile(`(?i)go-github|githubv4`),
	regexp.MustCompile(`(?i)pull request`),
	regexp.MustCompile(`(?i)argoproj|argo-?cd`),
	regexp.MustCompile(`(?i)fluxcd`),
	regexp.MustCompile(`\bAppProject\b`),
	regexp.MustCompile(`\brepoURL\b`),
	regexp.MustCompile(`\btargetRevision\b`),
	regexp.MustCompile(`(?m)^\s*kind:\s*Application\s*$`),
	regexp.MustCompile(`(?i)k8s\.io/client-go`),
	regexp.MustCompile(`(?i)clientcmd|inclusterconfig`),
	regexp.MustCompile(`(?i)kubeconfig`),
	regexp.MustCompile(`(?i)kubectl apply`),
}

// TestManifestBoundary enforces the manifest-only plugin boundary: neither the
// runtime source nor the plugin-owned Kubernetes output may take on Git,
// GitHub, Argo, Flux, or direct-cluster ownership. Test files are excluded so
// this contract can name the forbidden concepts without tripping itself.
func TestManifestBoundary(t *testing.T) {
	sources, err := runtimeGoFiles(".")
	if err != nil {
		t.Fatalf("collect runtime source: %v", err)
	}
	for _, path := range sources {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		assertNoForbiddenOwnership(t, path, content)
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
		assertNoForbiddenOwnership(t, path, content)
		return nil
	})
	if err != nil {
		t.Fatalf("walk deployment templates: %v", err)
	}
}

// runtimeGoFiles returns every non-test Go source path under root, descending
// into subpackages so a forbidden dependency cannot hide below the module root.
// Hidden directories (.git, .github) hold no compiled runtime source and are
// skipped.
func runtimeGoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// findForbiddenOwnership returns each forbidden-ownership token present in
// content, in pattern order.
func findForbiddenOwnership(content []byte) []string {
	var matches []string
	for _, pattern := range forbiddenOwnership {
		if match := pattern.Find(content); match != nil {
			matches = append(matches, string(match))
		}
	}
	return matches
}

func assertNoForbiddenOwnership(t *testing.T, path string, content []byte) {
	t.Helper()
	for _, match := range findForbiddenOwnership(content) {
		t.Errorf("%s carries forbidden transport/reconciler ownership: %q", path, match)
	}
}

// TestRuntimeGoFilesIsRecursive proves the runtime scan reaches subpackages and
// ignores test files and hidden directories, so a forbidden dependency added
// below the module root cannot slip past TestManifestBoundary.
func TestRuntimeGoFilesIsRecursive(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("root.go", "package main\n")
	write("internal/pkg/leak.go", "package pkg\n\nimport _ \"k8s.io/client-go/kubernetes\"\n")
	write("internal/pkg/leak_test.go", "package pkg\n")
	write(".hidden/ignored.go", "package hidden\n")

	files, err := runtimeGoFiles(root)
	if err != nil {
		t.Fatalf("runtimeGoFiles: %v", err)
	}
	got := make(map[string]bool, len(files))
	for _, f := range files {
		rel, relErr := filepath.Rel(root, f)
		if relErr != nil {
			t.Fatal(relErr)
		}
		got[rel] = true
	}

	want := filepath.Join("internal", "pkg", "leak.go")
	if !got[want] {
		t.Errorf("recursive scan missed nested runtime source %q; found %v", want, files)
	}
	if got[filepath.Join("internal", "pkg", "leak_test.go")] {
		t.Errorf("scan must exclude test files")
	}
	if got[filepath.Join(".hidden", "ignored.go")] {
		t.Errorf("scan must skip hidden directories")
	}
}

// TestFindForbiddenOwnership pins the forbidden-ownership concepts the boundary
// must reject and the legitimate module host it must accept.
func TestFindForbiddenOwnership(t *testing.T) {
	forbidden := []string{
		"exec.Command(\"git\", \"clone\", repo)",
		"import _ \"github.com/go-git/go-git/v5\"",
		"import _ \"github.com/google/go-github/v60/github\"",
		"import _ \"github.com/argoproj/argo-cd/v2/pkg/apis\"",
		"import _ \"github.com/fluxcd/flux2\"",
		"import _ \"k8s.io/client-go/kubernetes\"",
		"config, _ := rest.InClusterConfig()",
		"clientcmd.BuildConfigFromFlags(\"\", kubeconfig)",
		"kind: Application",
		"repoURL: https://example.com/repo.git",
		"targetRevision: main",
	}
	for _, sample := range forbidden {
		if len(findForbiddenOwnership([]byte(sample))) == 0 {
			t.Errorf("expected %q to be rejected", sample)
		}
	}

	allowed := []string{
		"import basev0 \"github.com/codefly-dev/core/generated/go/codefly/base/v0\"",
		"import \"github.com/neo4j/neo4j-go-driver/v5/neo4j\"",
	}
	for _, sample := range allowed {
		if matches := findForbiddenOwnership([]byte(sample)); len(matches) != 0 {
			t.Errorf("expected %q to be accepted, got %v", sample, matches)
		}
	}
}
