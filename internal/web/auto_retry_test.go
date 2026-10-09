package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"scrutineer/internal/db"
	"scrutineer/internal/worker"
)

type autoRetryFixture struct {
	s     *Server
	repo  db.Repository
	skill db.Skill
	now   time.Time
}

func newAutoRetryFixture(t *testing.T) (*autoRetryFixture, func()) {
	t.Helper()
	s, done := newTestServer(t)
	s.AutoRetryMax = 2
	s.AutoRetryDelay = 5 * time.Minute
	repo := db.Repository{URL: "https://example.com/retry", Name: "retry"}
	s.DB.Create(&repo)
	skill := db.Skill{Name: "critic", Description: "x", Body: "b", Active: true, Source: "ui", Version: 1}
	s.DB.Create(&skill)
	return &autoRetryFixture{s: s, repo: repo, skill: skill, now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}, done
}

// failed seeds a failed critic scan that finished `ago` before the fixture
// clock, at the given automatic-retry depth.
func (f *autoRetryFixture) failed(t *testing.T, ago time.Duration, retries int, errText string) db.Scan {
	return f.failedAt(t, "", ago, retries, errText)
}

// failedAt is failed with a sub-path, so two failures can coexist without one
// superseding the other under the newest-attempt-per-tuple rule.
func (f *autoRetryFixture) failedAt(t *testing.T, subPath string, ago time.Duration, retries int, errText string) db.Scan {
	t.Helper()
	finished := f.now.Add(-ago)
	sc := db.Scan{RepositoryID: f.repo.ID, Kind: worker.JobSkill, SkillID: &f.skill.ID, SkillName: f.skill.Name,
		SubPath: subPath, Status: db.ScanFailed, StatusPriority: db.StatusPriorityFor(db.ScanFailed),
		Error: errText, FinishedAt: &finished, AutoRetries: retries}
	if err := f.s.DB.Create(&sc).Error; err != nil {
		t.Fatal(err)
	}
	return sc
}

func (f *autoRetryFixture) queuedChildren(t *testing.T, parent uint) []db.Scan {
	t.Helper()
	var scans []db.Scan
	f.s.DB.Where("parent_scan_id = ?", parent).Find(&scans)
	return scans
}

func TestAutoRetry_reenqueuesALostScanOnceItsDelayPassed(t *testing.T) {
	f, done := newAutoRetryFixture(t)
	defer done()
	lost := f.failed(t, 10*time.Minute, 0, worker.AbandonedPrefix+"no heartbeat from instance alessia since 2026-09-30T11:40:00Z")

	if n := f.s.autoRetryTick(context.Background(), f.now); n != 1 {
		t.Fatalf("retried %d, want 1", n)
	}
	kids := f.queuedChildren(t, lost.ID)
	if len(kids) != 1 {
		t.Fatalf("children of the lost scan = %d, want 1", len(kids))
	}
	kid := kids[0]
	if kid.Status != db.ScanQueued || kid.AutoRetries != 1 || kid.SkillID == nil || *kid.SkillID != f.skill.ID {
		t.Errorf("retry = status %q retries %d skill %v; want queued, 1, %d", kid.Status, kid.AutoRetries, kid.SkillID, f.skill.ID)
	}
	// The retry is now the newest attempt for the tuple: a second pass must
	// not enqueue another one on top of it.
	if n := f.s.autoRetryTick(context.Background(), f.now); n != 0 {
		t.Errorf("second pass retried %d, want 0", n)
	}
}

func TestAutoRetry_waitsOutTheDelayAndBacksOffPerRetry(t *testing.T) {
	f, done := newAutoRetryFixture(t)
	defer done()
	fresh := f.failedAt(t, "a", 2*time.Minute, 0, "server restarted during run")
	// Second retry of a lineage waits 2×base = 10 minutes; 7 is not enough.
	backingOff := f.failedAt(t, "b", 7*time.Minute, 1, "scan timed out after 1h0m0s")

	if n := f.s.autoRetryTick(context.Background(), f.now); n != 0 {
		t.Fatalf("retried %d inside the delay windows, want 0", n)
	}
	if len(f.queuedChildren(t, fresh.ID)) != 0 || len(f.queuedChildren(t, backingOff.ID)) != 0 {
		t.Fatal("a scan inside its delay window was retried")
	}

	later := f.now.Add(4 * time.Minute) // fresh: 6 min ago, backingOff: 11 min ago
	if n := f.s.autoRetryTick(context.Background(), later); n != 2 {
		t.Fatalf("retried %d once both delays elapsed, want 2", n)
	}
	kids := f.queuedChildren(t, backingOff.ID)
	if len(kids) != 1 || kids[0].AutoRetries != 2 {
		t.Errorf("backed-off lineage retry: %+v", kids)
	}
}

func TestAutoRetry_stopsAtTheBudget(t *testing.T) {
	f, done := newAutoRetryFixture(t)
	defer done()
	exhausted := f.failed(t, time.Hour, 2, "stage skill: copy aux files: open /x: no such file or directory")

	if n := f.s.autoRetryTick(context.Background(), f.now); n != 0 {
		t.Fatalf("retried %d past the budget, want 0", n)
	}
	if len(f.queuedChildren(t, exhausted.ID)) != 0 {
		t.Error("a lineage at the budget was retried")
	}
}

func TestAutoRetry_leavesVerdictFailuresAndOtherInstancesAlone(t *testing.T) {
	f, done := newAutoRetryFixture(t)
	defer done()
	verdict := f.failedAt(t, "a", time.Hour, 0, "Critical-severity finding meets fail_on=High")
	theirs := f.failedAt(t, "b", time.Hour, 0, worker.AbandonedPrefix+"no heartbeat")
	f.s.DB.Model(&db.Scan{}).Where("id = ?", theirs.ID).Update("instance", "alessia")

	if n := f.s.autoRetryTick(context.Background(), f.now); n != 0 {
		t.Fatalf("retried %d, want 0", n)
	}
	if len(f.queuedChildren(t, verdict.ID)) != 0 {
		t.Error("a fail_on verdict failure was retried; a rerun cannot change it")
	}
	if len(f.queuedChildren(t, theirs.ID)) != 0 {
		t.Error("another instance's failure was retried here; it would run under this member's account")
	}
}

func TestAutoRetry_ignoresFailuresOlderThanTheWindow(t *testing.T) {
	f, done := newAutoRetryFixture(t)
	defer done()
	stale := f.failed(t, 3*24*time.Hour, 0, "server restarted during run")
	if n := f.s.autoRetryTick(context.Background(), f.now); n != 0 {
		t.Errorf("retried %d failures older than the window, want 0", n)
	}
	if len(f.queuedChildren(t, stale.ID)) != 0 {
		t.Error("a days-old failure was retried; that backlog is the operator's call")
	}
}

func TestAutoRetry_disabledWhenMaxIsZero(t *testing.T) {
	f, done := newAutoRetryFixture(t)
	defer done()
	f.s.AutoRetryMax = 0
	f.failed(t, time.Hour, 0, "server restarted during run")
	if n := f.s.autoRetryTick(context.Background(), f.now); n != 0 {
		t.Errorf("retried %d with the pass disabled, want 0", n)
	}
}

// An auto-retry must reproduce the failed scan, not a stripped copy of it.
// retryScanColumns and the ScanOpts built from it are two places the same field
// has to appear; a reflect scan missing TriageScanID cannot pass
// prepareReflection, so its retry could only fail again.
func TestAutoRetry_carriesTheTriageCohortLink(t *testing.T) {
	f, done := newAutoRetryFixture(t)
	defer done()

	triage := db.Scan{RepositoryID: f.repo.ID, Kind: worker.JobSkill, SkillName: "triage",
		Status: db.ScanDone, StatusPriority: db.StatusPriorityFor(db.ScanDone)}
	if err := f.s.DB.Create(&triage).Error; err != nil {
		t.Fatal(err)
	}

	lost := f.failed(t, 10*time.Minute, 0, worker.AbandonedPrefix+"no heartbeat")
	if err := f.s.DB.Model(&db.Scan{}).Where("id = ?", lost.ID).
		Update("triage_scan_id", triage.ID).Error; err != nil {
		t.Fatal(err)
	}

	if n := f.s.autoRetryTick(context.Background(), f.now); n != 1 {
		t.Fatalf("retried %d, want 1", n)
	}
	kids := f.queuedChildren(t, lost.ID)
	if len(kids) != 1 {
		t.Fatalf("children = %d, want 1", len(kids))
	}
	kid := kids[0]
	if kid.TriageScanID == nil || *kid.TriageScanID != triage.ID {
		t.Errorf("retry triage_scan_id = %v, want %d — the cohort link was dropped", kid.TriageScanID, triage.ID)
	}
}

// Losing the exploration fields turns an auto-retried adversarial sweep back
// into a plain deep dive. Exploration is only valid on security-deep-dive
// (worker.ValidateExploration), hence the separate skill here.
func TestAutoRetry_carriesTheExplorationContext(t *testing.T) {
	f, done := newAutoRetryFixture(t)
	defer done()

	// ExplorationInstructions reads references/<mode>.md from the skill's
	// source path, so an exploration-capable skill needs a reference pack.
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "references", worker.ExplorationAdversarialSweep+".md"),
		[]byte("sweep the target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := db.Skill{Name: deepDiveSkillName, Description: "x", Body: "b", Active: true,
		Source: "ui", SourcePath: source, Version: 1}
	if err := f.s.DB.Create(&deep).Error; err != nil {
		t.Fatal(err)
	}
	finished := f.now.Add(-10 * time.Minute)
	lost := db.Scan{RepositoryID: f.repo.ID, Kind: worker.JobSkill, SkillID: &deep.ID, SkillName: deep.Name,
		Status: db.ScanFailed, StatusPriority: db.StatusPriorityFor(db.ScanFailed),
		Error: worker.AbandonedPrefix + "no heartbeat", FinishedAt: &finished,
		ExplorationMode: worker.ExplorationAdversarialSweep, ExplorationPath: "cmd/server"}
	if err := f.s.DB.Create(&lost).Error; err != nil {
		t.Fatal(err)
	}

	if n := f.s.autoRetryTick(context.Background(), f.now); n != 1 {
		t.Fatalf("retried %d, want 1", n)
	}
	kids := f.queuedChildren(t, lost.ID)
	if len(kids) != 1 {
		t.Fatalf("children = %d, want 1", len(kids))
	}
	if kid := kids[0]; kid.ExplorationMode != worker.ExplorationAdversarialSweep || kid.ExplorationPath != "cmd/server" {
		t.Errorf("retry exploration = %q/%q, want %q/cmd/server",
			kid.ExplorationMode, kid.ExplorationPath, worker.ExplorationAdversarialSweep)
	}
}

// Without the feedback the verification re-runs without the note that prompted
// it. A scan only persists feedback after passing normalizeVerificationOpts, so
// a retry carrying SkillID and FindingID always satisfies that guard again.
func TestAutoRetry_carriesVerificationFeedback(t *testing.T) {
	f, done := newAutoRetryFixture(t)
	defer done()

	verify := db.Skill{Name: verifySkillName, Description: "x", Body: "b", Active: true, Source: "ui", Version: 1}
	if err := f.s.DB.Create(&verify).Error; err != nil {
		t.Fatal(err)
	}
	origin := db.Scan{RepositoryID: f.repo.ID, Kind: worker.JobSkill, SkillName: deepDiveSkillName,
		Status: db.ScanDone, StatusPriority: db.StatusPriorityFor(db.ScanDone)}
	if err := f.s.DB.Create(&origin).Error; err != nil {
		t.Fatal(err)
	}
	finding := db.Finding{RepositoryID: f.repo.ID, ScanID: origin.ID, Title: "Parser issue",
		Status: db.FindingEnriched, Severity: "High"}
	if err := f.s.DB.Create(&finding).Error; err != nil {
		t.Fatal(err)
	}
	finished := f.now.Add(-10 * time.Minute)
	lost := db.Scan{RepositoryID: f.repo.ID, Kind: worker.JobSkill, SkillID: &verify.ID, SkillName: verify.Name,
		Status: db.ScanFailed, StatusPriority: db.StatusPriorityFor(db.ScanFailed),
		Error: worker.AbandonedPrefix + "no heartbeat", FinishedAt: &finished,
		FindingID: &finding.ID, VerificationFeedback: "recheck the auth boundary"}
	if err := f.s.DB.Create(&lost).Error; err != nil {
		t.Fatal(err)
	}

	if n := f.s.autoRetryTick(context.Background(), f.now); n != 1 {
		t.Fatalf("retried %d, want 1", n)
	}
	kids := f.queuedChildren(t, lost.ID)
	if len(kids) != 1 {
		t.Fatalf("children = %d, want 1", len(kids))
	}
	if kid := kids[0]; kid.VerificationFeedback != "recheck the auth boundary" {
		t.Errorf("retry verification_feedback = %q, want it carried over", kid.VerificationFeedback)
	}
}
