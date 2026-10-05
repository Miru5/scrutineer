package worker

import (
	"context"
	"fmt"
	"time"

	"scrutineer/internal/db"
)

const (
	// DefaultHeartbeatInterval is how often a running scan's row is touched
	// while this process holds it.
	DefaultHeartbeatInterval = 30 * time.Second
	// DefaultReapAfter is how stale a heartbeat may be before the row is
	// treated as abandoned: sixty missed beats, well past any database
	// hiccup but a fraction of the days an orphan otherwise sits.
	//
	// Was ten minutes, which reaped a scan that was still working. On
	// 2026-10-02 a security-deep-dive on eclipse-mraa/mraa (scan 7678) went
	// quiet 20 minutes in while its container compiled and ran sanitizer
	// harnesses; every other scan on that instance kept finishing normally,
	// so the worker was alive and only this scan's beat had stopped. The row
	// was failed at 12:54 while the agent carried on to a complete,
	// schema-valid report — whose findings, cost and turn count the failed
	// row could no longer take. A reaper that fires on a working scan costs
	// more than one that waits: the orphan it is chasing is idle, the scan it
	// interrupts is paid for.
	DefaultReapAfter = 30 * time.Minute
	// LegacyReapAfter applies to running rows with no heartbeat at all, which
	// a binary from before the column existed claimed. Such a scan cannot
	// prove it is alive, so it gets a grace period longer than any scan
	// timeout the fleet configures (the default is one hour) before it is
	// declared lost.
	LegacyReapAfter = 24 * time.Hour
	// DefaultReapInterval is how often StartReaper runs a pass.
	DefaultReapInterval = 5 * time.Minute
	// AbandonedPrefix opens the error text of every reaped scan so the
	// condition is recognisable in the UI and the retry-failed action.
	AbandonedPrefix = "worker lost: "
)

func (w *Worker) heartbeatInterval() time.Duration {
	if w.HeartbeatInterval > 0 {
		return w.HeartbeatInterval
	}
	return DefaultHeartbeatInterval
}

func (w *Worker) reapAfter() time.Duration {
	if w.ReapAfter > 0 {
		return w.ReapAfter
	}
	return DefaultReapAfter
}

// beat writes one heartbeat. Only a row still marked running takes it, so a
// tick that races the terminal save leaves the finished row alone.
func (w *Worker) beat(scanID uint) {
	now := w.now().UTC()
	if err := w.DB.Model(&db.Scan{}).
		Where("id = ? AND status = ?", scanID, db.ScanRunning).
		Update("heartbeat_at", &now).Error; err != nil {
		w.Log.Warn("scan heartbeat", "scan", scanID, "err", err)
	}
}

// startHeartbeat keeps the scan's heartbeat fresh from a goroutine of its own
// until the returned stop func is called. It is deliberately independent of
// the scan's context: a runner that ignores its deadline is still a live
// process, and what the reaper must catch is a process that is gone.
func (w *Worker) startHeartbeat(scanID uint) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(w.heartbeatInterval())
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				w.beat(scanID)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// ReapAbandonedScans fails every running row whose worker has stopped
// reporting, and returns how many it flipped. Deliberately fleet-wide, unlike
// db.SweepRunning: that sweep only runs on the instance that restarted, so a
// worker lost to SIGKILL, a host outage or a hung runner left its rows
// `running` on the shared database until someone noticed — 40 hours, in the
// case that prompted this. A stale heartbeat is proof of a dead process
// wherever the row came from, so any instance may act on it; each update is
// re-guarded on the status and the staleness so a beat landing between the
// read and the write wins.
//
// A reaped scan gets the same group-settlement hook a cancel gets, so a batch
// whose last member died still completes — the hook's consumer scopes itself
// to the owning instance (web.autoEnqueueFindingDedup), so settling another
// member's cohort here enqueues nothing on this instance; their own reaper
// pass settles it when it is the one that wins the update. OnScanFailed is not
// fired: that hook is for a scan this worker ran, and the reaped one belongs
// elsewhere.
func (w *Worker) ReapAbandonedScans(ctx context.Context) (int, error) {
	if w.DB == nil {
		return 0, nil
	}
	now := w.now().UTC()
	staleBeat := now.Add(-w.reapAfter())
	legacyStart := now.Add(-LegacyReapAfter)
	var rows []db.Scan
	if err := w.DB.WithContext(ctx).
		Select("id", "repository_id", "instance", "kind", "skill_name", "model", "backend",
			"finding_id", "scan_group", "status", "heartbeat_at", "started_at", "created_at").
		Where("status = ?", db.ScanRunning).
		Where("(heartbeat_at IS NOT NULL AND heartbeat_at < ?) OR (heartbeat_at IS NULL AND COALESCE(started_at, created_at) < ?)",
			staleBeat, legacyStart).
		Order("id").Find(&rows).Error; err != nil {
		return 0, err
	}
	reaped := 0
	for i := range rows {
		sc := &rows[i]
		reason := abandonedReason(sc, now)
		res := w.DB.WithContext(ctx).Model(&db.Scan{}).
			Where("id = ? AND status = ?", sc.ID, db.ScanRunning).
			Where("heartbeat_at IS NULL OR heartbeat_at < ?", staleBeat).
			Updates(map[string]any{
				"status":          db.ScanFailed,
				"status_priority": db.StatusPriorityFor(db.ScanFailed),
				errorColumn:       reason,
				"finished_at":     &now,
			})
		if res.Error != nil {
			return reaped, res.Error
		}
		if res.RowsAffected == 0 {
			continue // it beat, or finished, since the read
		}
		sc.Status = db.ScanFailed
		sc.StatusPriority = db.StatusPriorityFor(db.ScanFailed)
		sc.Error = reason
		sc.FinishedAt = &now
		if kind, ok := db.ScanLifecycleEventKind(db.ScanFailed); ok {
			if err := db.LogScanEvent(w.DB.WithContext(ctx), kind, sc); err != nil {
				w.Log.Warn("log reaped scan event", "scan", sc.ID, "err", err)
			}
		}
		w.Log.Warn("reaped abandoned scan", "scan", sc.ID, "instance", sc.Instance, "reason", reason)
		w.SettleScanGroup(sc)
		w.publish(sc.ID, sc.RepositoryID, "scan-status", string(db.ScanFailed))
		reaped++
	}
	return reaped, nil
}

func abandonedReason(sc *db.Scan, now time.Time) string {
	owner := "this instance"
	if sc.Instance != "" {
		owner = "instance " + sc.Instance
	}
	if sc.HeartbeatAt != nil {
		return fmt.Sprintf("%sno heartbeat from %s since %s (%s ago)",
			AbandonedPrefix, owner, sc.HeartbeatAt.UTC().Format(time.RFC3339),
			now.Sub(sc.HeartbeatAt.UTC()).Round(time.Minute))
	}
	since := sc.CreatedAt
	if sc.StartedAt != nil {
		since = *sc.StartedAt
	}
	return fmt.Sprintf("%srunning on %s since %s with no heartbeat (pre-heartbeat build)",
		AbandonedPrefix, owner, since.UTC().Format(time.RFC3339))
}

// StartReaper runs ReapAbandonedScans every `every` (DefaultReapInterval when
// zero) until ctx ends. The first pass runs at once so a restart clears any
// backlog without waiting an interval.
func (w *Worker) StartReaper(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultReapInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := w.ReapAbandonedScans(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			w.Log.Warn("reap abandoned scans", "err", err)
		} else if n > 0 {
			w.Log.Info("reaped abandoned scans", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
