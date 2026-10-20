package worker

import (
	"os"
	"path/filepath"
	"testing"
)

// modCacheTree builds the shape `go mod download` leaves behind: a file inside
// a directory with no write bit, so the entry cannot be unlinked.
func modCacheTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mod := filepath.Join(root, ".poc", "gopath", "pkg", "mod", "golang.org", "x", "exp@v0.0.0")
	if err := os.MkdirAll(filepath.Join(mod, "slices"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "slices", "slices.go"), []byte("package slices\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Innermost first: a parent made read-only would block the chmod below.
	for _, d := range []string{filepath.Join(mod, "slices"), mod} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = removeAllForce(root) })
	return root
}

// The bug: plain removal cannot unlink out of a read-only directory, which is
// how ~200MB per Go-touching scan survived every cleanup pass.
func TestOsRemoveAllFailsOnModuleCache(t *testing.T) {
	root := modCacheTree(t)
	if err := os.RemoveAll(root); err == nil {
		t.Skip("os.RemoveAll removed a read-only module tree; " +
			"running as root or on a permissive filesystem")
	}
}

func TestRemoveAllForceClearsModuleCache(t *testing.T) {
	root := modCacheTree(t)
	if err := removeAllForce(root); err != nil {
		t.Fatalf("removeAllForce: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("tree survived: stat err = %v", err)
	}
}

// resetWorkspace runs before every skill and hit the same wall
// ("reset work: unlinkat …/gopath/pkg/mod/…").
func TestResetWorkspaceClearsModuleCache(t *testing.T) {
	root := modCacheTree(t)
	if err := resetWorkspace(root); err != nil {
		t.Fatalf("resetWorkspace: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("workspace not readable after reset: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace not empty after reset: %v", entries)
	}
}

func TestRemoveAllForceIsNoopOnMissingPath(t *testing.T) {
	if err := removeAllForce(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("removeAllForce on missing path: %v", err)
	}
}
