package worker

import (
	"log/slog"
	"os"
	"testing"

	"scrutineer/internal/db"
	"scrutineer/internal/db/dbtest"
)

// cleanupScanWorkspace must delete only a workspace nothing else still needs.
// The workspace is keyed by the resume lineage root, so a scan reaching a
// terminal status while a resume of the same lineage is still queued or
// paused must leave the tree alone — the resumed harness needs that source.
func TestCleanupScanWorkspace(t *testing.T) {
	gdb := dbtest.Open(t)
	repo := db.Repository{URL: "https://example.com/repo", Name: "repo"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	create := func(scan db.Scan) db.Scan {
		t.Helper()
		scan.RepositoryID, scan.Kind = repo.ID, JobSkill
		if err := gdb.Create(&scan).Error; err != nil {
			t.Fatal(err)
		}
		return scan
	}
	w := &Worker{DB: gdb, DataDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	plant := func(id uint) string {
		t.Helper()
		root := w.workRoot(id)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("finished scan sheds its workspace", func(t *testing.T) {
		scan := create(db.Scan{Status: db.ScanDone})
		root := plant(scan.ID)
		w.cleanupScanWorkspace(&scan)
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("workspace survived a finished scan: %v", err)
		}
	})

	for _, tc := range []struct {
		name   string
		status db.ScanStatus
	}{
		{"queued resume", db.ScanQueued},
		{"running resume", db.ScanRunning},
		{"paused resume", db.ScanPaused},
	} {
		t.Run("kept while a "+tc.name+" shares the lineage", func(t *testing.T) {
			root := create(db.Scan{Status: db.ScanFailed})
			create(db.Scan{Status: tc.status, ResumedFromScanID: &root.ID})
			dir := plant(root.ID)
			w.cleanupScanWorkspace(&root)
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("workspace removed while a %s still needed it: %v", tc.name, err)
			}
		})
	}

	// The resumed scan finishing last must shed the lineage's workspace, not
	// its own id's — otherwise the tree outlives every scan that used it.
	t.Run("last scan in a lineage sheds the root workspace", func(t *testing.T) {
		root := create(db.Scan{Status: db.ScanFailed})
		resume := create(db.Scan{Status: db.ScanDone, ResumedFromScanID: &root.ID})
		dir := plant(root.ID)
		w.cleanupScanWorkspace(&resume)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("lineage workspace survived its last scan: %v", err)
		}
	})

	// The read-only module cache must not defeat the hot-path cleanup either.
	t.Run("clears a read-only module cache", func(t *testing.T) {
		scan := create(db.Scan{Status: db.ScanDone})
		root := plant(scan.ID)
		mod := root + "/.poc/gopath/pkg/mod/golang.org/x/exp@v0.0.0"
		if err := os.MkdirAll(mod, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mod+"/exp.go", []byte("package exp\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(mod, 0o555); err != nil {
			t.Fatal(err)
		}
		w.cleanupScanWorkspace(&scan)
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("workspace with a module cache survived: %v", err)
		}
	})
}
