package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"scrutineer/internal/db"
	"scrutineer/internal/worker"
)

// reflect requires threat-model; the repository's only threat-model run hit
// the turn cap with an empty report (2026-10-21, capella-studio).
func cappedThreatModelFixture(t *testing.T, s *Server) (db.Repository, db.Skill) {
	t.Helper()
	repo := db.Repository{URL: "https://example.com/capella", Name: "capella"}
	s.DB.Create(&repo)
	s.DB.Create(&db.Skill{Name: "threat-model", Description: "d", Body: "b", Active: true,
		Source: "ui", Version: 1, OutputFile: "report.json", OutputKind: "threat_model"})
	reflect := db.Skill{Name: "reflect", Description: "d", Body: "b", Active: true,
		Source: "ui", Version: 1, OutputFile: "report.json", OutputKind: "reflection",
		Requires: "threat-model"}
	s.DB.Create(&reflect)
	s.DB.Create(&db.Scan{RepositoryID: repo.ID, Kind: worker.JobSkill, SkillName: "threat-model",
		Status: db.ScanDone, StatusPriority: db.StatusPriorityFor(db.ScanDone),
		MaxTurnsHit: true, Report: ""})
	return repo, reflect
}

// A capped, empty completion restarts like a failure; a capped run with a
// partial report is a completion and is left alone (see db.UsableDone).
func TestRestartFailedPrereqs_cappedEmptyDoneIsDead(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo, _ := cappedThreatModelFixture(t, s)
	if n := s.restartFailedPrereqs(context.Background(), repo.ID, "reflect", ""); n != 1 {
		t.Fatalf("restarted %d, want 1 (the empty threat-model)", n)
	}

	s2, done2 := newTestServer(t)
	defer done2()
	repo2, _ := cappedThreatModelFixture(t, s2)
	s2.DB.Model(&db.Scan{}).Where("repository_id = ? AND skill_name = ?", repo2.ID, "threat-model").
		Update("report", `{"partial":true}`)
	if n := s2.restartFailedPrereqs(context.Background(), repo2.ID, "reflect", ""); n != 0 {
		t.Fatalf("restarted %d, want 0 — a partial report satisfies the gate", n)
	}
}

// The manual Retry button restarts dead prerequisites, as the automatic retry
// always has: retrying reflect alone fails again on the same empty model.
func TestScanRetry_restartsDeadPrereqs(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo, reflect := cappedThreatModelFixture(t, s)
	failed := db.Scan{RepositoryID: repo.ID, Kind: worker.JobSkill, SkillID: &reflect.ID,
		SkillName: "reflect", Status: db.ScanFailed, StatusPriority: db.StatusPriorityFor(db.ScanFailed)}
	s.DB.Create(&failed)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, localReq(http.MethodPost, fmt.Sprintf("/scans/%d/retry", failed.ID)))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("retry status %d: %s", w.Code, w.Body)
	}
	if q := s.queuedOf(t, repo.ID); q["threat-model"] != 1 || q["reflect"] != 1 {
		t.Fatalf("queued after retry = %v, want one threat-model and one reflect", q)
	}
}

// Resuming a scan that sat paused while its prerequisite finished empty
// restarts that prerequisite too — single and bulk resume alike.
func TestScanResume_restartsDeadPrereqs(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(fmt.Sprintf("bulk=%v", bulk), func(t *testing.T) {
			s, done := newTestServer(t)
			defer done()
			repo, reflect := cappedThreatModelFixture(t, s)
			paused := db.Scan{RepositoryID: repo.ID, Kind: worker.JobSkill, SkillID: &reflect.ID,
				SkillName: "reflect", Status: db.ScanPaused,
				StatusPriority: db.StatusPriorityFor(db.ScanPaused), Error: "account paused"}
			s.DB.Create(&paused)

			path := fmt.Sprintf("/scans/%d/resume", paused.ID)
			if bulk {
				path = "/scans/resume-paused"
			}
			if w := postForm(t, s, path, url.Values{}); w.Code >= 400 {
				t.Fatalf("resume status %d: %s", w.Code, w.Body)
			}
			var got db.Scan
			s.DB.First(&got, paused.ID)
			if got.Status != db.ScanQueued {
				t.Fatalf("reflect status = %s, want queued", got.Status)
			}
			if q := s.queuedOf(t, repo.ID); q["threat-model"] != 1 {
				t.Fatalf("queued after resume = %v, want the threat-model restarted", q)
			}
		})
	}
}
