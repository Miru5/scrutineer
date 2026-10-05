package web

import (
	"context"
	"testing"

	"scrutineer/internal/db"
)

// A skill whose prerequisites failed is retried: restartFailedPrereqs must
// re-enqueue exactly the failed prerequisites — not the ones that succeeded,
// are already running, or were never enqueued — and walk them transitively.
func TestRestartFailedPrereqs(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/p", Name: "p"}
	s.DB.Create(&repo)

	skill := func(name, requires string) {
		s.DB.Create(&db.Skill{Name: name, Description: "d", Body: "b", Active: true,
			Source: "ui", Version: 1, Requires: requires})
	}
	// deep-dive requires recon, threat-model, semgrep; threat-model requires recon.
	skill("recon", "")
	skill("threat-model", "recon")
	skill("semgrep", "")
	skill("security-deep-dive", "recon\nthreat-model\nsemgrep")
	skill("disabled-prereq", "")
	s.DB.Model(&db.Skill{}).Where("name = ?", "disabled-prereq").Update("active", false)

	scan := func(name string, st db.ScanStatus) {
		s.DB.Create(&db.Scan{RepositoryID: repo.ID, Kind: "skill", SkillName: name,
			Status: st, StatusPriority: db.StatusPriorityFor(st)})
	}
	scan("recon", db.ScanDone)          // satisfied → not restarted
	scan("threat-model", db.ScanFailed) // dead → restarted
	scan("semgrep", db.ScanRunning)     // in flight → not restarted
	// no scan for anything else

	before := s.queuedOf(t, repo.ID)
	n := s.restartFailedPrereqs(context.Background(), repo.ID, "security-deep-dive", "")
	if n != 1 {
		t.Fatalf("restarted %d, want 1 (threat-model only)", n)
	}
	q := s.queuedOf(t, repo.ID)
	added := map[string]int{}
	for name, c := range q {
		if c > before[name] {
			added[name] = c - before[name]
		}
	}
	if len(added) != 1 || added["threat-model"] != 1 {
		t.Fatalf("re-enqueued %v, want just threat-model", added)
	}
}

// A prerequisite left in any terminal non-success state is restarted, not
// only a failed one — cancelled and skipped count too.
//
// This is the case that bit in production (2026-10-01): a triage pipeline
// was cancelled by hand on 2026-09-28, leaving repo-overview, advisories and
// packages as `cancelled` rows on ~96 repositories. The gate reads those as
// dead exactly as it reads `failed` (worker.prereqStatus: any prereq with
// scans, none done and none in flight), so every security-deep-dive on those
// repositories failed instantly with "prereqs failed" — 527 of them.
//
// restartFailedPrereqs already handles this, because prereqFailedOnRepo
// mirrors prereqStatus rather than testing for `failed`. The test exists so
// that stays true: the function's name and the word "failed" throughout
// invite a later "simplification" to `status = 'failed'`, which would
// silently strand every cancelled prereq again.
func TestRestartFailedPrereqs_cancelledAndSkipped(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/c", Name: "c"}
	s.DB.Create(&repo)

	skill := func(name, requires string) {
		s.DB.Create(&db.Skill{Name: name, Description: "d", Body: "b", Active: true,
			Source: "ui", Version: 1, Requires: requires})
	}
	skill("repo-overview", "")
	skill("advisories", "")
	skill("packages", "")
	skill("recon", "")
	skill("security-deep-dive", "repo-overview\nadvisories\npackages\nrecon")

	scan := func(name string, st db.ScanStatus) {
		s.DB.Create(&db.Scan{RepositoryID: repo.ID, Kind: "skill", SkillName: name,
			Status: st, StatusPriority: db.StatusPriorityFor(st)})
	}
	scan("repo-overview", db.ScanCancelled) // dead → restarted
	scan("advisories", db.ScanCancelled)    // dead → restarted
	scan("packages", db.ScanSkipped)        // dead → restarted
	scan("recon", db.ScanDone)              // satisfied → left alone

	before := s.queuedOf(t, repo.ID)
	n := s.restartFailedPrereqs(context.Background(), repo.ID, "security-deep-dive", "")
	if n != 3 {
		t.Fatalf("restarted %d, want 3 (two cancelled + one skipped)", n)
	}
	q := s.queuedOf(t, repo.ID)
	added := map[string]int{}
	for name, c := range q {
		if c > before[name] {
			added[name] = c - before[name]
		}
	}
	want := map[string]int{"repo-overview": 1, "advisories": 1, "packages": 1}
	if len(added) != len(want) {
		t.Fatalf("re-enqueued %v, want %v", added, want)
	}
	for name, c := range want {
		if added[name] != c {
			t.Fatalf("re-enqueued %v, want %v", added, want)
		}
	}
}

// A cancelled prerequisite that has *also* succeeded at some point is
// satisfied: the gate asks whether any scan is done, not whether the latest
// one is. Re-running it would be wasted model time on every retry.
func TestRestartFailedPrereqs_cancelledAfterDoneIsSatisfied(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/d", Name: "d"}
	s.DB.Create(&repo)
	s.DB.Create(&db.Skill{Name: "repo-overview", Active: true, Source: "ui", Version: 1})
	s.DB.Create(&db.Skill{Name: "security-deep-dive", Active: true, Source: "ui",
		Version: 1, Requires: "repo-overview"})
	for _, st := range []db.ScanStatus{db.ScanDone, db.ScanCancelled} {
		s.DB.Create(&db.Scan{RepositoryID: repo.ID, Kind: "skill",
			SkillName: "repo-overview", Status: st,
			StatusPriority: db.StatusPriorityFor(st)})
	}
	if n := s.restartFailedPrereqs(context.Background(), repo.ID, "security-deep-dive", ""); n != 0 {
		t.Fatalf("restarted %d, want 0 (an earlier done scan satisfies the gate)", n)
	}
}

// A prerequisite that never ran on the repository is left alone (the gate
// treats an absent prereq as satisfied), and the walk does not fail when a
// prerequisite is disabled.
func TestRestartFailedPrereqs_absentAndDisabled(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/q", Name: "q"}
	s.DB.Create(&repo)
	s.DB.Create(&db.Skill{Name: "recon", Active: true, Source: "ui", Version: 1})
	s.DB.Create(&db.Skill{Name: "vuln-scan", Active: true, Source: "ui", Version: 1, Requires: "recon\nmissing-skill"})
	// recon has no scan on the repo at all → absent → not forced.

	n := s.restartFailedPrereqs(context.Background(), repo.ID, "vuln-scan", "")
	if n != 0 {
		t.Fatalf("restarted %d, want 0 (recon absent, missing-skill unregistered)", n)
	}
}

// queuedOf returns queued scan counts by skill name for a repository.
func (s *Server) queuedOf(t *testing.T, repoID uint) map[string]int {
	t.Helper()
	var rows []db.Scan
	s.DB.Select("skill_name").Where("repository_id = ? AND status = ?", repoID, db.ScanQueued).Find(&rows)
	out := map[string]int{}
	for _, r := range rows {
		out[r.SkillName]++
	}
	return out
}
