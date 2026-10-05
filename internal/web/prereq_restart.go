package web

import (
	"context"

	"scrutineer/internal/db"
	"scrutineer/internal/skills"
)

// restartFailedPrereqs re-enqueues the prerequisite skills of `skillName`
// that the gate considers dead on this repository, so a retry of one skill
// restarts the whole pipeline rather than failing on the prereq gate.
//
// Why this exists: a skill that declares scrutineer.requires only dispatches
// once each prerequisite has a completed scan (see worker.preflightSkill). If
// a prerequisite's most recent scan is terminal-failed, the gate marks it
// "dead" and fails the dependent immediately with "prereqs failed" — so
// retrying just the downstream skill (the Retry button, the bulk Restart, the
// automatic retry) fails fast without re-running the failed prerequisites
// (user report, 2026-09-30: "restarting a scan doesn't restart all the
// skills"). Re-enqueuing the dead prerequisites first turns them in-flight, so
// the dependent's gate defers and waits for them instead of failing.
//
// "Failed" here means what the gate means by dead, which is wider than
// status=failed: a prerequisite that has scans on the repository, none of
// them done and none in flight — so `cancelled` and `skipped` restart too.
// prereqFailedOnRepo deliberately mirrors worker.prereqStatus instead of
// testing a status, and must keep mirroring it; narrowing this to
// status=failed would strand every prerequisite anyone had cancelled. That
// is not hypothetical: a triage pipeline cancelled by hand on 2026-09-28
// left `cancelled` prereqs on ~96 repositories, and every deep dive on them
// died on the gate (527 of them) until the prereqs were re-enqueued. See
// TestRestartFailedPrereqs_cancelledAndSkipped.
//
// Everything else is left alone: a prerequisite that succeeded once
// satisfies the gate even if a later attempt was cancelled, one in flight is
// waited for, and one never enqueued for this repository is not forced
// (triage or the operator chose to skip it — the gate treats that as
// satisfied too). The walk is transitive, so a prerequisite's own dead
// prerequisites restart with it; a scan cannot be reached twice.
//
// Scoping is the caller's: this runs from a retry path that already resolved
// which repository (and instance) it may act on. Prerequisites are enqueued
// into the dependent's own scan group when it has one, so a grouped rescan's
// gate (which waits on same-group prereqs) sees them. Failures to enqueue a
// single prerequisite are logged and skipped, never fatal to the retry.
//
// Returns how many prerequisite scans were re-enqueued.
func (s *Server) restartFailedPrereqs(ctx context.Context, repoID uint, skillName, scanGroup string) int {
	if skillName == "" {
		return 0
	}
	restarted := 0
	visited := map[string]bool{skillName: true}
	queue := s.prereqNames(skillName)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if visited[name] {
			continue
		}
		visited[name] = true

		var sk db.Skill
		if err := s.DB.Where("name = ? AND active = ?", name, true).First(&sk).Error; err != nil {
			continue // unregistered or disabled: the gate treats it as satisfied
		}
		// Recurse into this prerequisite's own prerequisites regardless of
		// whether we restart it — a satisfied prereq may still sit above a
		// failed one.
		queue = append(queue, skills.SplitPatterns(sk.Requires)...)

		if !s.prereqFailedOnRepo(repoID, name) {
			continue
		}
		if _, err := s.enqueueSkillWith(ctx, repoID, sk.ID, ScanOpts{ScanGroup: scanGroup}); err != nil {
			s.Log.Warn("restart prereq: enqueue failed",
				"repo", repoID, "prereq", name, "for", skillName, "err", err)
			continue
		}
		s.Log.Info("restarted failed prereq",
			"repo", repoID, "prereq", name, "for", skillName)
		restarted++
	}
	return restarted
}

// prereqNames is the declared prerequisite skill names of one skill, by name.
func (s *Server) prereqNames(skillName string) []string {
	var sk db.Skill
	if err := s.DB.Select("requires").Where("name = ?", skillName).First(&sk).Error; err != nil {
		return nil
	}
	return skills.SplitPatterns(sk.Requires)
}

// prereqFailedOnRepo reports whether the named skill's latest scans on the
// repository are terminal-failed with no completion and nothing in flight —
// the "dead" state the worker's prereq gate fails a dependent on. False when a
// scan is done (satisfied), one is queued/running/paused (already restarting),
// or none exists at all (never part of this repo's pipeline).
func (s *Server) prereqFailedOnRepo(repoID uint, skillName string) bool {
	base := s.DB.Model(&db.Scan{}).Where("repository_id = ? AND skill_name = ?", repoID, skillName)
	var total int64
	base.Count(&total)
	if total == 0 {
		return false
	}
	var done int64
	s.DB.Model(&db.Scan{}).Where("repository_id = ? AND skill_name = ? AND status = ?",
		repoID, skillName, db.ScanDone).Count(&done)
	if done > 0 {
		return false
	}
	var live int64
	s.DB.Model(&db.Scan{}).Where("repository_id = ? AND skill_name = ? AND status IN ?",
		repoID, skillName, []db.ScanStatus{db.ScanQueued, db.ScanRunning, db.ScanPaused}).Count(&live)
	return live == 0
}
