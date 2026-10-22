package web

import (
	"context"
	"time"

	"scrutineer/internal/db"
	"scrutineer/internal/fleet"
)

// Every finding verify has confirmed should carry a patch.
// autoChainPatchAfterVerify covers a verify that finishes from now on; this
// pass covers the rest — findings confirmed before that chain existed (697 on
// the fleet on 2026-10-22) and any the chain missed while the patch skill was
// inactive. It runs on the instance that ran the confirming verify, so each
// member pays for its own findings, and only on findings that have never had
// a patch scan of any status: a patch that failed is auto-retry's business, a
// cancelled one was a decision, and one the gate rejected would only be
// rejected again.
const (
	patchBackfillEvery = 10 * time.Minute
	// A small batch per pass: patch scans are finding-priority and run ahead
	// of repository scans, so a whole backlog at once would hold every
	// worker for hours. Ten every ten minutes clears ~700 in half a day.
	patchBackfillBatch = 10
)

type patchBackfillRow struct {
	FindingID    uint
	RepositoryID uint
	Profile      string
}

func (s *Server) patchBackfillTick(ctx context.Context) int {
	var skill db.Skill
	if err := s.DB.Where("name = ? AND active = ?", patchSkillName, true).First(&skill).Error; err != nil {
		return 0
	}
	var rows []patchBackfillRow
	err := s.DB.Raw(`
SELECT f.id AS finding_id, f.repository_id, COALESCE(vs.profile, '') AS profile
  FROM findings f
  JOIN finding_verifications fv
    ON fv.id = (SELECT MAX(v.id) FROM finding_verifications v WHERE v.finding_id = f.id)
  JOIN scans vs ON vs.id = fv.scan_id
 WHERE fv.status = 'confirmed'
   AND COALESCE(vs.ref, '') = ''
   AND COALESCE(vs.instance, '') = ?
   AND COALESCE(f.suggested_fix, '') = ''
   AND f.status NOT IN ?
   AND NOT EXISTS (SELECT 1 FROM scans p WHERE p.finding_id = f.id AND p.skill_id = ?)
 ORDER BY f.id DESC
 LIMIT ?`, fleet.Name(), db.ClosedFindingLifecycles, skill.ID, patchBackfillBatch).
		Scan(&rows).Error
	if err != nil {
		s.Log.Warn("patch backfill: list findings", "err", err)
		return 0
	}
	queued := 0
	for _, r := range rows {
		if err := s.enqueueFindingScopedSkillIfIdle(ctx, r.RepositoryID, r.FindingID, skill.ID,
			ScanOpts{Profile: r.Profile}); err != nil {
			s.Log.Warn("patch backfill", "finding", r.FindingID, "repo", r.RepositoryID, "err", err)
			continue
		}
		queued++
	}
	return queued
}

// StartPatchBackfill runs patchBackfillTick every patchBackfillEvery until
// ctx ends.
func (s *Server) StartPatchBackfill(ctx context.Context) {
	t := time.NewTicker(patchBackfillEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.patchBackfillTick(ctx); n > 0 {
				s.Log.Info("patch backfill pass", "queued", n)
			}
		}
	}
}
