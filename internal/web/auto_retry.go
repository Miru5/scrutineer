package web

import (
	"context"
	"errors"
	"strings"
	"time"

	"scrutineer/internal/db"
	"scrutineer/internal/fleet"
	"scrutineer/internal/worker"
)

const (
	// DefaultAutoRetryMax is how many times the automatic pass re-enqueues one
	// lineage before leaving it to the operator. Two covers the transient
	// causes this exists for (a lost worker, a restart mid-run, a staging
	// hiccup, a runner that hung until its timeout) without turning a durable
	// failure into an all-day loop.
	DefaultAutoRetryMax = 2
	// DefaultAutoRetryDelay is the wait after a failure before its first
	// automatic retry; it doubles with every further retry of the lineage.
	DefaultAutoRetryDelay = 5 * time.Minute
	autoRetryTick         = 2 * time.Minute
	// autoRetryWindow bounds how old a failure may be and still be retried
	// automatically. The pass exists for failures that just happened; a
	// backlog from before the pass was deployed, or from a week-old outage,
	// is the operator's call (Retry all failed), not a surprise bill.
	autoRetryWindow = 24 * time.Hour
	// autoRetryBatch bounds one pass so a backlog of failures is re-enqueued
	// over a few ticks rather than flooding the queue at once.
	autoRetryBatch = 50
)

// retryScanColumns is what a retry needs to reproduce the failed scan: every
// field scanRetry copies into ScanOpts, plus the failure context the
// automatic pass filters on.
const retryScanColumns = "id, repository_id, skill_id, skill_name, model, effort, finding_id, " +
	"remediation_attempt_id, sub_path, scope_mode, ref, profile, rescan_mode, diff_base_scan_id, " +
	"scan_group, focus_area, triage_scan_id, exploration_mode, exploration_path, " +
	"verification_feedback, backend, status, session_id, resumed_from_scan_id, import_payload, " +
	"auto_retries, error, finished_at"

// newestAttemptPerTupleSQL keeps only the newest scan per (repository, skill,
// sub_path, ref, finding) tuple: a failure that already has a later attempt in
// one of supersedingStatuses is not retried again. Cancelled is deliberately
// absent from that list, so a user-cancelled newer run does not block retrying
// an older genuine failure.
const newestAttemptPerTupleSQL = `NOT EXISTS (
			SELECT 1 FROM scans n
			WHERE n.id > scans.id
			  AND n.repository_id = scans.repository_id
			  AND COALESCE(n.skill_id, 0) = COALESCE(scans.skill_id, 0)
			  AND COALESCE(n.sub_path, '') = COALESCE(scans.sub_path, '')
			  AND COALESCE(n.ref, '') = COALESCE(scans.ref, '')
			  AND COALESCE(n.finding_id, 0) = COALESCE(scans.finding_id, 0)
			  AND n.status IN ?
		)`

var supersedingStatuses = []db.ScanStatus{db.ScanQueued, db.ScanRunning, db.ScanDone, db.ScanFailed, db.ScanPaused}

// nonRetryableFailure recognises failures that a rerun cannot change: the
// skill produced its verdict and the fail_on threshold turned it into a
// failure, or the enqueue itself was refused. Everything else — a worker
// lost mid-run, a restart, a staging error, a runner timeout, a harness or
// container error — is assumed transient until the retry budget says
// otherwise.
func nonRetryableFailure(errText string) bool {
	return strings.Contains(errText, "meets fail_on=") ||
		strings.HasPrefix(errText, "enqueue scan ")
}

// autoRetryTick re-enqueues this instance's failed skill scans that are past
// their retry delay and under the retry budget, through the same path as the
// Retry button (session resumed when the failure captured one). Scoped to
// this instance for the same reason scansRetryFailed is: the retry lands on
// this member's queue partition and runs under their model account, so a
// teammate's failure is theirs to retry — their own instance runs this pass
// too. The reaper (worker.ReapAbandonedScans) is what turns a hung or lost
// scan into a failed row this pass can see, so together they make "detect
// the hang, restart the scan" automatic.
//
// Returns how many scans were re-enqueued.
func (s *Server) autoRetryTick(ctx context.Context, now time.Time) int {
	if s.AutoRetryMax <= 0 {
		return 0
	}
	base := s.AutoRetryDelay
	if base <= 0 {
		base = DefaultAutoRetryDelay
	}
	var scans []db.Scan
	err := fleet.ScopeOwn(s.DB.Model(&db.Scan{}).
		Select(retryScanColumns).
		Where("status = ? AND kind = ? AND skill_id IS NOT NULL", db.ScanFailed, worker.JobSkill).
		Where("auto_retries < ?", s.AutoRetryMax).
		Where("finished_at IS NOT NULL AND finished_at < ? AND finished_at > ?", now.Add(-base), now.Add(-autoRetryWindow)).
		Where(newestAttemptPerTupleSQL, supersedingStatuses)).
		Order("id").Limit(autoRetryBatch).
		Find(&scans).Error
	if err != nil {
		s.Log.Warn("auto-retry: list failed scans", "err", err)
		return 0
	}
	retried := 0
	repos := map[uint]struct{}{}
	for _, sc := range scans {
		// Delay doubles per retry of the lineage: base, 2×base, 4×base...
		wait := base << uint(sc.AutoRetries)
		if sc.FinishedAt == nil || now.Sub(*sc.FinishedAt) < wait {
			continue
		}
		if nonRetryableFailure(sc.Error) {
			continue
		}
		s.restartFailedPrereqs(ctx, sc.RepositoryID, sc.SkillName, sc.ScanGroup)
		sessionID, resumeOf := s.resumeOpts(sc)
		parent := sc.ID
		newID, err := s.enqueueSkillWith(ctx, sc.RepositoryID, *sc.SkillID, ScanOpts{
			Model:                sc.Model,
			Effort:               sc.Effort,
			FindingID:            sc.FindingID,
			RemediationAttemptID: sc.RemediationAttemptID,
			SubPath:              sc.SubPath,
			ScopeMode:            sc.ScopeMode,
			Ref:                  sc.Ref,
			Profile:              sc.Profile,
			RescanMode:           sc.RescanMode,
			DiffBaseScanID:       sc.DiffBaseScanID,
			ScanGroup:            sc.ScanGroup,
			FocusArea:            sc.FocusArea,
			// Keep in lockstep with scanRetry and scansRetryFailed; a reflect
			// scan without its TriageScanID cannot pass prepareReflection.
			TriageScanID:         sc.TriageScanID,
			ExplorationMode:      sc.ExplorationMode,
			ExplorationPath:      sc.ExplorationPath,
			VerificationFeedback: sc.VerificationFeedback,
			SessionID:            sessionID,
			ResumedFromScanID:    resumeOf,
			ParentScanID:         &parent,
			ImportPayload:        sc.ImportPayload,
			AutoRetries:          sc.AutoRetries + 1,
		})
		if err != nil {
			if errors.Is(err, db.ErrFindingNonViable) || errors.Is(err, ErrRepoFederationOptOut) {
				continue // the enqueue gate said no; nothing to retry
			}
			s.Log.Warn("auto-retry: enqueue failed", "scan", sc.ID, "skill", sc.SkillName, "err", err)
			continue
		}
		s.Log.Info("auto-retried failed scan",
			"scan", sc.ID, "retry", newID, "attempt", sc.AutoRetries+1, "max", s.AutoRetryMax,
			"skill", sc.SkillName, "resumed", resumeOf != nil, "reason", truncate(sc.Error, 120))
		repos[sc.RepositoryID] = struct{}{}
		retried++
	}
	for repoID := range repos {
		s.publishScanList(repoID)
	}
	return retried
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// StartAutoRetry runs autoRetryTick every autoRetryTick until ctx ends.
func (s *Server) StartAutoRetry(ctx context.Context) {
	if s.AutoRetryMax <= 0 {
		s.Log.Info("automatic retry of failed scans disabled")
		return
	}
	t := time.NewTicker(autoRetryTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n := s.autoRetryTick(ctx, now); n > 0 {
				s.Log.Info("auto-retry pass", "retried", n)
			}
		}
	}
}
