package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"scrutineer/internal/db"
	"scrutineer/internal/fleet"
)

// The proposal's two halves pull in opposite directions: a shared database so
// everyone can see what has already been scanned, but per-instance control so
// one member's queue actions cannot disturb another's. These tests pin the
// dividing line — reads fleet-wide, writes scoped.

func scopeTestServer(t *testing.T) (*Server, uint) {
	t.Helper()
	t.Cleanup(func() { fleet.Install(nil, "") })

	gdb, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.Install(gdb, "mine"); err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{URL: "https://example.invalid/o/r", Name: "r"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	return &Server{DB: gdb}, repo.ID
}

func TestOwnsScanRejectsAnotherInstance(t *testing.T) {
	s, repoID := scopeTestServer(t)

	theirs := db.Scan{RepositoryID: repoID, Kind: "skill", Status: db.ScanQueued, Instance: "theirs"}
	if err := s.DB.Create(&theirs).Error; err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if s.ownsScan(rec, &theirs) {
		t.Fatal("ownsScan allowed a scan owned by another instance")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}

	ours := db.Scan{RepositoryID: repoID, Kind: "skill", Status: db.ScanQueued}
	if err := s.DB.Create(&ours).Error; err != nil {
		t.Fatal(err)
	}
	if !s.ownsScan(httptest.NewRecorder(), &ours) {
		t.Fatal("ownsScan rejected this instance's own scan")
	}
}

// "Pause queue" must mean this member's queue. Unscoped it would stop
// everyone else's work from a button that looks entirely local.
func TestScopeOwnLeavesOtherInstancesAlone(t *testing.T) {
	s, repoID := scopeTestServer(t)

	ours := db.Scan{RepositoryID: repoID, Kind: "skill", Status: db.ScanQueued}
	theirs := db.Scan{RepositoryID: repoID, Kind: "skill", Status: db.ScanQueued, Instance: "theirs"}
	for _, sc := range []*db.Scan{&ours, &theirs} {
		if err := s.DB.Create(sc).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := fleet.ScopeOwn(s.DB.Model(&db.Scan{}).Where("status = ?", db.ScanQueued)).
		Update("status", db.ScanPaused).Error; err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		id   uint
		want db.ScanStatus
	}{
		{"ours", ours.ID, db.ScanPaused},
		{"theirs", theirs.ID, db.ScanQueued},
	} {
		var got db.Scan
		if err := s.DB.First(&got, tc.id).Error; err != nil {
			t.Fatal(err)
		}
		if got.Status != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, got.Status, tc.want)
		}
	}
}

// Visibility is the other half of the bargain: a teammate's scan must still
// be readable, or the shared database buys nothing.
func TestReadsStayFleetWide(t *testing.T) {
	s, repoID := scopeTestServer(t)

	if err := s.DB.Create(&db.Scan{RepositoryID: repoID, Kind: "skill",
		Status: db.ScanDone, Instance: "theirs"}).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := s.DB.Model(&db.Scan{}).Where("repository_id = ?", repoID).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unscoped read returned %d scans, want 1 — reads must stay fleet-wide", count)
	}
}

// Every single-scan write route must refuse another instance's row before it
// touches anything: retry and resume would re-queue it onto this instance's
// partition, cancel would flip a row whose work carries on elsewhere.
func TestSingleScanWritesRefuseAnotherInstance(t *testing.T) {
	s, repoID := scopeTestServer(t)

	for _, tc := range []struct {
		name    string
		status  db.ScanStatus
		handler http.HandlerFunc
	}{
		{"retry", db.ScanFailed, s.scanRetry},
		{"resume", db.ScanPaused, s.scanResume},
		{"cancel", db.ScanQueued, s.scanCancel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			theirs := db.Scan{RepositoryID: repoID, Kind: "skill", Status: tc.status, Instance: "theirs"}
			if err := s.DB.Create(&theirs).Error; err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/scans/0/"+tc.name, nil)
			req.SetPathValue("id", strconv.Itoa(int(theirs.ID)))
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
			}
			var got db.Scan
			if err := s.DB.First(&got, theirs.ID).Error; err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.status {
				t.Fatalf("status changed to %q; the row must be untouched", got.Status)
			}
		})
	}
}
