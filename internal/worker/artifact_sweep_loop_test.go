package worker

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"scrutineer/internal/db"
	"scrutineer/internal/db/dbtest"
)

// StartArtifactSweep must keep reclaiming workspaces while the process stays
// up — the startup-only pass meant a worker alive for days never did.
func TestStartArtifactSweepReapsOnATick(t *testing.T) {
	gdb := dbtest.Open(t)
	repo := db.Repository{URL: "https://example.com/repo", Name: "repo"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	// Terminal, no session, no resumes: the sweep's reap case.
	orphan := db.Scan{RepositoryID: repo.ID, Kind: JobSkill, Status: db.ScanFailed}
	if err := gdb.Create(&orphan).Error; err != nil {
		t.Fatal(err)
	}
	w := &Worker{DB: gdb, DataDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	root := w.workRoot(orphan.ID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.StartArtifactSweep(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			return // reaped by a tick, not by a restart
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("orphan workspace survived the sweep loop")
}

// The first pass belongs to Register, so the loop must not fire immediately —
// otherwise every start does the same work twice.
func TestStartArtifactSweepWaitsBeforeItsFirstPass(t *testing.T) {
	gdb := dbtest.Open(t)
	repo := db.Repository{URL: "https://example.com/repo", Name: "repo"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	orphan := db.Scan{RepositoryID: repo.ID, Kind: JobSkill, Status: db.ScanFailed}
	if err := gdb.Create(&orphan).Error; err != nil {
		t.Fatal(err)
	}
	w := &Worker{DB: gdb, DataDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	root := w.workRoot(orphan.ID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.StartArtifactSweep(ctx, time.Hour)
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("swept before the first interval elapsed: %v", err)
	}
}

// A cancelled context must stop the loop.
func TestStartArtifactSweepStopsOnContextCancel(t *testing.T) {
	w := &Worker{DB: dbtest.Open(t), DataDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.StartArtifactSweep(ctx, 10*time.Millisecond) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep loop ignored context cancellation")
	}
}
