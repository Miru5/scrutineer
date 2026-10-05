package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"scrutineer/internal/db"
	"scrutineer/internal/db/dbtest"
)

// loadScan reads a row into a fresh struct: gorm's First treats a primary key
// already set on the destination as an extra condition, so reusing one
// variable across lookups silently keeps the previous row.
func loadScan(t *testing.T, w *Worker, id uint) db.Scan {
	t.Helper()
	var sc db.Scan
	if err := w.DB.First(&sc, id).Error; err != nil {
		t.Fatalf("load scan %d: %v", id, err)
	}
	return sc
}

func reaperWorker(t *testing.T, now time.Time) *Worker {
	t.Helper()
	return &Worker{
		DB:  dbtest.Open(t),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return now },
	}
}

var seededRepos atomic.Int64

func seedRunningScan(t *testing.T, w *Worker, instance, group string, started time.Time, beat *time.Time) db.Scan {
	t.Helper()
	n := seededRepos.Add(1)
	repo := db.Repository{URL: fmt.Sprintf("https://example.com/%s/%d", instance, n), Name: fmt.Sprintf("%s-%d", instance, n)}
	if err := w.DB.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	sc := db.Scan{RepositoryID: repo.ID, Instance: instance, Kind: JobSkill, SkillName: "s",
		Status: db.ScanRunning, StatusPriority: db.StatusPriorityFor(db.ScanRunning),
		StartedAt: &started, HeartbeatAt: beat, ScanGroup: group}
	if err := w.DB.Create(&sc).Error; err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestReapAbandonedScans_failsStaleHeartbeatsFleetWide(t *testing.T) {
	// The reaper is the fleet-wide counterpart of SweepRunning: a row whose
	// heartbeat stopped belongs to a dead process wherever it ran, so an
	// instance may fail another member's row. A fresh heartbeat is proof of
	// life and is left alone, whoever owns it.
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	w := reaperWorker(t, now)
	stale := now.Add(-DefaultReapAfter - time.Minute)
	fresh := now.Add(-time.Minute)
	dead := seedRunningScan(t, w, "alessia", "", now.Add(-3*time.Hour), &stale)
	alive := seedRunningScan(t, w, "ioana", "", now.Add(-3*time.Hour), &fresh)
	mine := seedRunningScan(t, w, "", "", now.Add(-3*time.Hour), &stale)

	var settled []uint
	w.OnScanGroupSettled = func(sc *db.Scan) { settled = append(settled, sc.ID) }
	var published []uint
	w.OnEvent = func(scanID, _ uint, name, data string) {
		if name == "scan-status" && data == string(db.ScanFailed) {
			published = append(published, scanID)
		}
	}

	n, err := w.ReapAbandonedScans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reaped %d, want 2 (the stale rows on both instances)", n)
	}
	got := loadScan(t, w, dead.ID)
	if got.Status != db.ScanFailed || got.FinishedAt == nil {
		t.Errorf("dead scan: status=%q finished_at=%v, want failed with a finish time", got.Status, got.FinishedAt)
	}
	if !strings.HasPrefix(got.Error, AbandonedPrefix) || !strings.Contains(got.Error, "instance alessia") {
		t.Errorf("dead scan error = %q, want the abandoned reason naming the owner", got.Error)
	}
	if got.StatusPriority != db.StatusPriorityFor(db.ScanFailed) {
		t.Errorf("status_priority = %d, want terminal", got.StatusPriority)
	}
	got = loadScan(t, w, mine.ID)
	if got.Status != db.ScanFailed || !strings.Contains(got.Error, "this instance") {
		t.Errorf("own stale scan: status=%q error=%q", got.Status, got.Error)
	}
	got = loadScan(t, w, alive.ID)
	if got.Status != db.ScanRunning {
		t.Errorf("scan with a fresh heartbeat was reaped: status=%q", got.Status)
	}
	if len(published) != 2 {
		t.Errorf("published failed status for %v, want both reaped scans", published)
	}
	if len(settled) != 0 {
		t.Errorf("ungrouped scans must not fire the group hook, got %v", settled)
	}

	var events int64
	w.DB.Model(&db.AuditEvent{}).Where("kind = ?", db.AuditEventScanFailed).Count(&events)
	if events != 2 {
		t.Errorf("audit events for reaped scans = %d, want 2", events)
	}

	// Idempotent: nothing left to reap.
	if n, err := w.ReapAbandonedScans(context.Background()); err != nil || n != 0 {
		t.Errorf("second pass reaped %d (err %v), want 0", n, err)
	}
}

func TestReapAbandonedScans_toleratesPreHeartbeatRowsForADay(t *testing.T) {
	// A running row with no heartbeat at all was claimed by a binary that
	// predates the column. It cannot prove it is alive, so it is given the
	// long legacy grace before it is declared lost — and then it is.
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	w := reaperWorker(t, now)
	recent := seedRunningScan(t, w, "lukas", "", now.Add(-3*time.Hour), nil)
	ancient := seedRunningScan(t, w, "lukas", "", now.Add(-LegacyReapAfter-time.Hour), nil)

	n, err := w.ReapAbandonedScans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	got := loadScan(t, w, recent.ID)
	if got.Status != db.ScanRunning {
		t.Errorf("pre-heartbeat row inside the legacy grace was reaped: %q", got.Status)
	}
	got = loadScan(t, w, ancient.ID)
	if got.Status != db.ScanFailed || !strings.Contains(got.Error, "pre-heartbeat build") {
		t.Errorf("ancient pre-heartbeat row: status=%q error=%q", got.Status, got.Error)
	}
}

func TestReapAbandonedScans_settlesTheGroupOfAReapedBatchMember(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	w := reaperWorker(t, now)
	stale := now.Add(-time.Hour)
	member := seedRunningScan(t, w, "alessia", "focus-1", now.Add(-2*time.Hour), &stale)
	var settled []uint
	w.OnScanGroupSettled = func(sc *db.Scan) { settled = append(settled, sc.ID) }

	if _, err := w.ReapAbandonedScans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(settled) != 1 || settled[0] != member.ID {
		t.Errorf("group hook fired for %v, want [%d]", settled, member.ID)
	}
}

func TestHeartbeat_touchesOnlyARunningRow(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	w := reaperWorker(t, now)
	old := now.Add(-time.Hour)
	running := seedRunningScan(t, w, "", "", old, &old)
	finished := seedRunningScan(t, w, "", "", old, &old)
	w.DB.Model(&db.Scan{}).Where("id = ?", finished.ID).Updates(map[string]any{
		"status": db.ScanDone, "status_priority": db.StatusPriorityFor(db.ScanDone)})

	w.beat(running.ID)
	w.beat(finished.ID)

	got := loadScan(t, w, running.ID)
	if got.HeartbeatAt == nil || !got.HeartbeatAt.Equal(now) {
		t.Errorf("running heartbeat = %v, want %v", got.HeartbeatAt, now)
	}
	got = loadScan(t, w, finished.ID)
	if got.HeartbeatAt == nil || !got.HeartbeatAt.Equal(old) {
		t.Errorf("finished row's heartbeat moved to %v; a terminal row must not be touched", got.HeartbeatAt)
	}
}

func TestStartHeartbeat_beatsUntilStopped(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	w := reaperWorker(t, now)
	w.HeartbeatInterval = 5 * time.Millisecond
	old := now.Add(-time.Hour)
	sc := seedRunningScan(t, w, "", "", old, &old)

	stop := w.startHeartbeat(sc.ID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var got db.Scan
		got = loadScan(t, w, sc.ID)
		if got.HeartbeatAt != nil && got.HeartbeatAt.Equal(now) {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("heartbeat never advanced from %v", got.HeartbeatAt)
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	// After stop the goroutine is gone: move the clock and confirm no further
	// beat lands.
	later := now.Add(time.Minute)
	w.Now = func() time.Time { return later }
	time.Sleep(20 * time.Millisecond)
	got := loadScan(t, w, sc.ID)
	if !got.HeartbeatAt.Equal(now) {
		t.Errorf("heartbeat moved to %v after stop", got.HeartbeatAt)
	}
}
