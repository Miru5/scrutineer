package worker

import (
	"io"
	"log/slog"
	"testing"

	"scrutineer/internal/db"
	"scrutineer/internal/db/dbtest"
)

// A prerequisite the turn cap cut off before it wrote anything is not a
// completion: the gate reads it as dead, so the dependent fails on the gate
// (and its retry restarts the prerequisite) instead of running against an
// absent input — the capella-studio case, 2026-10-21, where an empty threat
// model let reflect start and fail with "produced none" on every retry.
// A capped run that did write a partial report, and an uncapped run with no
// report (some skills deliver elsewhere), both still satisfy it.
func TestPrereqStatus_cappedEmptyDoneIsDead(t *testing.T) {
	inFlight := []db.ScanStatus{db.ScanQueued, db.ScanRunning, db.ScanPaused}
	for _, tc := range []struct {
		name   string
		capped bool
		report string
		want   prereqState
	}{
		{"capped, no report", true, "", prereqDead},
		{"capped, partial report", true, `{"partial":true}`, prereqSatisfied},
		{"not capped, no report", false, "", prereqSatisfied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gdb := dbtest.Open(t)
			repo := db.Repository{URL: "https://example.com/r", Name: "r"}
			gdb.Create(&repo)
			gdb.Create(&db.Scan{RepositoryID: repo.ID, Kind: JobSkill, SkillName: "threat-model",
				Status: db.ScanDone, StatusPriority: db.StatusPriorityFor(db.ScanDone),
				MaxTurnsHit: tc.capped, Report: tc.report})
			w := &Worker{DB: gdb, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			got := w.prereqStatus("repository_id = ? AND skill_name = ?",
				[]any{repo.ID, "threat-model"}, inFlight)
			if got != tc.want {
				t.Fatalf("prereqStatus = %v, want %v", got, tc.want)
			}
		})
	}
}
