package main

import (
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNewService_EmbedsBase(t *testing.T) {
	svc := NewService()
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	if svc.Base == nil {
		t.Fatal("Service.Base is nil — services.Base embedding broken")
	}
	if svc.Settings == nil {
		t.Fatal("Service.Settings is nil")
	}
}

func TestSettings_YAMLRoundTrip(t *testing.T) {
	src := []byte(`
databases:
  - app
  - analytics
`)
	var s Settings
	if err := yaml.Unmarshal(src, &s); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if got, want := len(s.Databases), 2; got != want {
		t.Errorf("Databases count: got %d, want %d", got, want)
	}
	if s.Databases[0] != "app" {
		t.Errorf("Databases[0]: got %q", s.Databases[0])
	}
}

func TestDefaultDataDirIncludesNamingScope(t *testing.T) {
	got := defaultDataDir("/home/alice", "mind-server", "neo4j", "minddev")
	want := filepath.Join("/home/alice", ".codefly", "data", "mind-server", "neo4j-minddev", "neo4j")
	if got != want {
		t.Fatalf("defaultDataDir() = %q, want %q", got, want)
	}
}

func TestDefaultDataDirPreservesUnscopedPath(t *testing.T) {
	got := defaultDataDir("/home/alice", "mind-server", "neo4j", "")
	want := filepath.Join("/home/alice", ".codefly", "data", "mind-server", "neo4j", "neo4j")
	if got != want {
		t.Fatalf("defaultDataDir() = %q, want %q", got, want)
	}
}
