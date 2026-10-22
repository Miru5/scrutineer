package web

import (
	"context"
	"testing"

	"scrutineer/internal/db"
)

type patchBackfillFixture struct {
	s     *Server
	repo  db.Repository
	patch db.Skill
}

func newPatchBackfillFixture(t *testing.T) (*patchBackfillFixture, func()) {
	t.Helper()
	s, done := newTestServer(t)
	repo := db.Repository{URL: "https://example.com/pb", Name: "pb"}
	s.DB.Create(&repo)
	patch := db.Skill{Name: "patch", OutputFile: "report.json", OutputKind: "patch", Version: 1, Active: true}
	s.DB.Create(&patch)
	return &patchBackfillFixture{s: s, repo: repo, patch: patch}, done
}

// verified seeds a finding whose newest verification has the given status,
// from a verify scan on ref (empty = default branch).
func (f *patchBackfillFixture) verified(t *testing.T, status, ref string) db.Finding {
	t.Helper()
	parent := db.Scan{RepositoryID: f.repo.ID, Status: db.ScanDone, SkillName: "security-deep-dive"}
	f.s.DB.Create(&parent)
	fd := db.Finding{ScanID: parent.ID, RepositoryID: f.repo.ID, Title: "t", Severity: "High", Status: db.FindingEnriched}
	f.s.DB.Create(&fd)
	vs := db.Scan{RepositoryID: f.repo.ID, Status: db.ScanDone, SkillName: "verify", Ref: ref, FindingID: &fd.ID, Profile: "java"}
	f.s.DB.Create(&vs)
	f.s.DB.Create(&db.FindingVerification{FindingID: fd.ID, ScanID: vs.ID, Status: status, Report: "{}"})
	return fd
}

func (f *patchBackfillFixture) patchScans(fd db.Finding) int64 {
	var n int64
	f.s.DB.Model(&db.Scan{}).Where("finding_id = ? AND skill_id = ?", fd.ID, f.patch.ID).Count(&n)
	return n
}

func TestPatchBackfill_queuesConfirmedFindingWithoutPatchOnce(t *testing.T) {
	f, done := newPatchBackfillFixture(t)
	defer done()
	fd := f.verified(t, "confirmed", "")

	if n := f.s.patchBackfillTick(context.Background()); n != 1 {
		t.Fatalf("queued %d, want 1", n)
	}
	var sc db.Scan
	f.s.DB.Where("finding_id = ? AND skill_id = ?", fd.ID, f.patch.ID).First(&sc)
	if sc.Profile != "java" {
		t.Errorf("profile = %q, want java (carried from the verify scan)", sc.Profile)
	}
	// Any patch scan, whatever became of it, ends the backfill for the finding.
	f.s.DB.Model(&db.Scan{}).Where("id = ?", sc.ID).Update("status", db.ScanFailed)
	if n := f.s.patchBackfillTick(context.Background()); n != 0 {
		t.Errorf("second pass queued %d, want 0", n)
	}
}

func TestPatchBackfill_leavesOtherFindingsAlone(t *testing.T) {
	f, done := newPatchBackfillFixture(t)
	defer done()
	inconclusive := f.verified(t, "inconclusive", "")
	fixRef := f.verified(t, "confirmed", "fix-branch")
	patched := f.verified(t, "confirmed", "")
	f.s.DB.Model(&db.Finding{}).Where("id = ?", patched.ID).Update("suggested_fix", "diff --git a/x b/x\n")
	closed := f.verified(t, "confirmed", "")
	f.s.DB.Model(&db.Finding{}).Where("id = ?", closed.ID).Update("status", db.FindingRejected)
	// Confirmed once, then a later verify said otherwise: the newest wins.
	reverified := f.verified(t, "confirmed", "")
	vs := db.Scan{RepositoryID: f.repo.ID, Status: db.ScanDone, SkillName: "verify", FindingID: &reverified.ID}
	f.s.DB.Create(&vs)
	f.s.DB.Create(&db.FindingVerification{FindingID: reverified.ID, ScanID: vs.ID, Status: "fixed", Report: "{}"})

	if n := f.s.patchBackfillTick(context.Background()); n != 0 {
		t.Fatalf("queued %d, want 0", n)
	}
	for _, fd := range []db.Finding{inconclusive, fixRef, patched, closed, reverified} {
		if n := f.patchScans(fd); n != 0 {
			t.Errorf("finding %d got %d patch scans, want 0", fd.ID, n)
		}
	}
}
