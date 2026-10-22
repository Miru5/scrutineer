package web

import (
	"sync"
	"testing"
	"time"

	"scrutineer/internal/db"
)

// TestAutoEnqueueRevalidate_carriesParentProfile locks #548: the auto-enqueued
// revalidate scan inherits the parent scan's resolved runner profile so it
// skips DetectProfile and runs on the same image the finding came from.
func TestAutoEnqueueRevalidate_carriesParentProfile(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/r", Name: "r"}
	s.DB.Create(&repo)
	revalidate := db.Skill{Name: "revalidate", OutputFile: "report.json", OutputKind: "revalidate", Version: 1, Active: true}
	s.DB.Create(&revalidate)
	// Parent scan already has its auto-detected profile persisted
	// (skill.go writes res.Profile back before OnFindingCreated fires).
	scan := db.Scan{RepositoryID: repo.ID, Status: db.ScanDone, SkillName: "security-deep-dive", Profile: "ruby-ext"}
	s.DB.Create(&scan)
	f := db.Finding{ScanID: scan.ID, RepositoryID: repo.ID, Title: "t", Severity: "High"}
	s.DB.Create(&f)

	s.autoEnqueueRevalidate(&scan, &f)

	var derived db.Scan
	if err := s.DB.Where("finding_id = ? AND skill_id = ?", f.ID, revalidate.ID).First(&derived).Error; err != nil {
		t.Fatalf("derived revalidate scan not enqueued: %v", err)
	}
	if derived.Profile != "ruby-ext" {
		t.Errorf("derived Profile = %q, want %q (carried from parent)", derived.Profile, "ruby-ext")
	}
}

func TestAutoChainRevalidate_truePositiveAlwaysQueuesCritic(t *testing.T) {
	s, done, verify, newFinding := chainTestSetup(t)
	defer done()
	critic := db.Skill{Name: criticSkillName, OutputFile: "report.json", OutputKind: "critic", Version: 1, Active: true}
	s.DB.Create(&critic)
	f := newFinding("Medium")
	parent := db.Scan{RepositoryID: f.RepositoryID, Status: db.ScanDone, SkillName: revalidateSkillName, Profile: "go"}
	s.DB.Create(&parent)

	s.autoChainVerifyAfterRevalidate(&parent, f, "true_positive", "Medium")

	var criticScan db.Scan
	if err := s.DB.Where("finding_id = ? AND skill_id = ?", f.ID, critic.ID).First(&criticScan).Error; err != nil {
		t.Fatalf("critic scan not enqueued: %v", err)
	}
	if criticScan.Profile != "go" {
		t.Errorf("critic profile = %q, want inherited go profile", criticScan.Profile)
	}
	var verifyCount int64
	s.DB.Model(&db.Scan{}).Where("finding_id = ? AND skill_id = ?", f.ID, verify.ID).Count(&verifyCount)
	if verifyCount != 0 {
		t.Fatalf("Medium finding queued %d verify scans, want 0", verifyCount)
	}
}

func TestAutoChainRevalidate_nonTruePositiveQueuesNeitherAssessment(t *testing.T) {
	s, done, verify, newFinding := chainTestSetup(t)
	defer done()
	critic := db.Skill{Name: criticSkillName, OutputFile: "report.json", OutputKind: "critic", Version: 1, Active: true}
	s.DB.Create(&critic)
	f := newFinding("High")

	s.autoChainVerifyAfterRevalidate(nil, f, "uncertain", "High")

	var count int64
	s.DB.Model(&db.Scan{}).Where("finding_id = ? AND skill_id IN ?", f.ID, []uint{critic.ID, verify.ID}).Count(&count)
	if count != 0 {
		t.Fatalf("non-true-positive queued %d downstream scans, want 0", count)
	}
}

func TestAutoEnqueueRevalidate_doesNotDoubleQueueConcurrently(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/r", Name: "r"}
	s.DB.Create(&repo)
	revalidate := db.Skill{Name: "revalidate", OutputFile: "report.json", OutputKind: "revalidate", Version: 1, Active: true}
	s.DB.Create(&revalidate)
	scan := db.Scan{RepositoryID: repo.ID, Status: db.ScanDone, SkillName: "security-deep-dive"}
	s.DB.Create(&scan)
	f := db.Finding{ScanID: scan.ID, RepositoryID: repo.ID, Title: "t", Severity: "High"}
	s.DB.Create(&f)

	const callers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.autoEnqueueRevalidate(&scan, &f)
		}()
	}
	close(start)
	wg.Wait()

	var queued int64
	s.DB.Model(&db.Scan{}).
		Where("finding_id = ? AND skill_id = ? AND status = ?", f.ID, revalidate.ID, db.ScanQueued).
		Count(&queued)
	if queued != 1 {
		t.Fatalf("queued revalidate scans = %d, want 1", queued)
	}
}

func patchChainSetup(t *testing.T) (*Server, func(), db.Skill, *db.Finding) {
	t.Helper()
	s, done, _, newFinding := chainTestSetup(t)
	patch := db.Skill{Name: "patch", OutputFile: "report.json", OutputKind: "patch", Version: 1, Active: true}
	s.DB.Create(&patch)
	return s, done, patch, newFinding("High")
}

func queuedPatches(s *Server, f *db.Finding, patch db.Skill) int64 {
	var n int64
	s.DB.Model(&db.Scan{}).Where("finding_id = ? AND skill_id = ? AND status = ?",
		f.ID, patch.ID, db.ScanQueued).Count(&n)
	return n
}

func TestAutoChainPatch_confirmedEnqueuesPatchOnce(t *testing.T) {
	withCutoff(t, time.Now().Add(-time.Hour))
	s, done, patch, f := patchChainSetup(t)
	defer done()
	verify := db.Scan{RepositoryID: f.RepositoryID, Status: db.ScanDone, SkillName: "verify", Profile: "java"}
	s.DB.Create(&verify)

	s.autoChainPatchAfterVerify(&verify, f, "confirmed")
	s.autoChainPatchAfterVerify(&verify, f, "confirmed") // a re-verify must not stack a second

	if n := queuedPatches(s, f, patch); n != 1 {
		t.Fatalf("queued patch scans = %d, want 1", n)
	}
	var derived db.Scan
	s.DB.Where("finding_id = ? AND skill_id = ?", f.ID, patch.ID).First(&derived)
	if derived.Profile != "java" {
		t.Errorf("patch Profile = %q, want %q (carried from verify)", derived.Profile, "java")
	}
}

func TestAutoChainPatch_skipsUnconfirmedRefScopedAndAlreadyPatched(t *testing.T) {
	withCutoff(t, time.Now().Add(-time.Hour))
	s, done, patch, f := patchChainSetup(t)
	defer done()

	for _, status := range []string{"fixed", "inconclusive", "deferred", "not_attempted"} {
		s.autoChainPatchAfterVerify(nil, f, status)
	}
	fixRef := db.Scan{RepositoryID: f.RepositoryID, Status: db.ScanDone, SkillName: "verify", Ref: "fix-branch"}
	s.DB.Create(&fixRef)
	s.autoChainPatchAfterVerify(&fixRef, f, "confirmed")
	if n := queuedPatches(s, f, patch); n != 0 {
		t.Fatalf("queued patch scans = %d, want 0", n)
	}

	s.DB.Model(&db.Finding{}).Where("id = ?", f.ID).Update("suggested_fix", "diff --git a/x b/x\n")
	s.autoChainPatchAfterVerify(nil, f, "confirmed")
	if n := queuedPatches(s, f, patch); n != 0 {
		t.Errorf("finding with a gated patch got %d new patch scans, want 0", n)
	}
}

// withCutoff pins autoChainCutoff for one test: findings created "now" by the
// fixtures fall after it, and one stamped before it is an old finding.
func withCutoff(t *testing.T, at time.Time) {
	t.Helper()
	prev := autoChainCutoff
	autoChainCutoff = at
	t.Cleanup(func() { autoChainCutoff = prev })
}

func TestAutoChain_newFindingGetsVerifyOldOneDoesNot(t *testing.T) {
	withCutoff(t, time.Now().Add(-time.Hour))
	s, done, verify, newFinding := chainTestSetup(t)
	defer done()
	fresh := newFinding("Medium")
	old := newFinding("Critical")
	s.DB.Model(&db.Finding{}).Where("id = ?", old.ID).Update("created_at", time.Now().Add(-48*time.Hour))
	old.CreatedAt = time.Time{} // make the chain read it from the database

	s.autoChainVerifyAfterRevalidate(nil, fresh, "true_positive", "Medium")
	s.autoChainVerifyAfterRevalidate(nil, old, "true_positive", "Critical")

	count := func(f *db.Finding) (n int64) {
		s.DB.Model(&db.Scan{}).Where("finding_id = ? AND skill_id = ?", f.ID, verify.ID).Count(&n)
		return
	}
	if count(fresh) != 1 || count(old) != 0 {
		t.Fatalf("verify scans: new finding %d, old finding %d; want 1 and 0", count(fresh), count(old))
	}
}

func TestAutoChainPatch_oldFindingIsLeftToClients(t *testing.T) {
	withCutoff(t, time.Now().Add(time.Hour)) // everything existing is old
	s, done, patch, f := patchChainSetup(t)
	defer done()
	s.autoChainPatchAfterVerify(nil, f, "confirmed")
	if n := queuedPatches(s, f, patch); n != 0 {
		t.Fatalf("old finding got %d patch scans, want 0", n)
	}
}
