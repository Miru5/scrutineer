package fleet

import (
	"testing"

	"gorm.io/gorm"

	"scrutineer/internal/db"
)

// newDB opens an in-memory database and joins it as name.
func newDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	t.Cleanup(func() { Install(nil, "") })
	gdb, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(gdb, name); err != nil {
		t.Fatal(err)
	}
	return gdb
}

func newRepo(t *testing.T, gdb *gorm.DB, url string) db.Repository {
	t.Helper()
	repo := db.Repository{URL: url, Name: "r"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	return repo
}

func statusOf(t *testing.T, gdb *gorm.DB, id uint) db.ScanStatus {
	t.Helper()
	var got db.Scan
	if err := gdb.First(&got, id).Error; err != nil {
		t.Fatal(err)
	}
	return got.Status
}

// SweepRunning must only touch this instance's rows. Unscoped it would fail
// every other member's live scans on a shared database while their work kept
// running, which is the worst available outcome: the row says failed, the
// container keeps burning quota, and nobody is told.
func TestSweepRunningScopedToInstance(t *testing.T) {
	gdb := newDB(t, "mine")
	repo := newRepo(t, gdb, "https://example.invalid/o/r")

	ours := db.Scan{RepositoryID: repo.ID, Kind: "skill", Status: db.ScanRunning}
	if err := gdb.Create(&ours).Error; err != nil {
		t.Fatal(err)
	}
	if ours.Instance != "mine" {
		t.Fatalf("the create callback did not stamp the owner: %q", ours.Instance)
	}

	// Another member's live scan, and a row from before the column existed.
	// Neither belongs to this process.
	theirs := db.Scan{RepositoryID: repo.ID, Kind: "skill", Status: db.ScanRunning, Instance: "theirs"}
	legacy := db.Scan{RepositoryID: repo.ID, Kind: "skill", Status: db.ScanRunning}
	for _, s := range []*db.Scan{&theirs, &legacy} {
		if err := gdb.Create(s).Error; err != nil {
			t.Fatal(err)
		}
	}
	// The callback stamps every row this process writes, so an unowned row
	// cannot be produced through Create — it only exists as data written
	// before the column did. Blank it directly to model that.
	if err := gdb.Model(&db.Scan{}).Where("id = ?", legacy.ID).
		UpdateColumn("instance", "").Error; err != nil {
		t.Fatal(err)
	}

	if err := SweepRunning(gdb); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		id   uint
		want db.ScanStatus
	}{
		{"ours", ours.ID, db.ScanFailed},
		{"theirs", theirs.ID, db.ScanRunning},
		// Left alone deliberately. A blank owner is ambiguous, and on a
		// shared database claiming someone else's row is the worse error of
		// the two: a stale spinner is visible, a wrongly-failed scan that
		// keeps running is not.
		{"legacy", legacy.ID, db.ScanRunning},
	} {
		if got := statusOf(t, gdb, tc.id); got != tc.want {
			t.Errorf("%s scan: status = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A single-instance deployment writes the empty identity, so the sweep must
// still find its own rows — otherwise every existing SQLite install would
// start showing spinners that never clear after a restart.
func TestSweepRunningEmptyInstanceSweepsOwnRows(t *testing.T) {
	gdb := newDB(t, "")
	repo := newRepo(t, gdb, "https://example.invalid/o/r2")
	scan := db.Scan{RepositoryID: repo.ID, Kind: "skill", Status: db.ScanRunning}
	if err := gdb.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	if err := SweepRunning(gdb); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, gdb, scan.ID); got != db.ScanFailed {
		t.Fatalf("status = %q, want %q", got, db.ScanFailed)
	}
}

// Rows written before this layer existed come back with a NULL owner, and
// `instance = ”` never matches NULL. Without the backfill an upgraded
// single-instance deployment loses every scoped action over its own history:
// ScopeOwn sees nothing, so cancel-all-queued, retry-failed and the running
// count all skip the rows, and SweepRunning leaves a restarted scan spinning
// forever. Reproduced against a real pre-upgrade database before it was fixed.
func TestInstallBackfillsPreUpgradeRows(t *testing.T) {
	gdb := newDB(t, "")
	repo := newRepo(t, gdb, "https://example.invalid/o/r3")
	scan := db.Scan{RepositoryID: repo.ID, Kind: "skill", Status: db.ScanRunning}
	if err := gdb.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	// What AutoMigrate leaves behind when it adds the column to an existing
	// database: the rows predate it, so they hold NULL rather than "".
	if err := gdb.Exec("UPDATE scans SET instance = NULL").Error; err != nil {
		t.Fatal(err)
	}

	var scoped int64
	if err := ScopeOwn(gdb.Model(&db.Scan{})).Count(&scoped).Error; err != nil {
		t.Fatal(err)
	}
	if scoped != 0 {
		t.Fatalf("precondition: NULL owners should be invisible to ScopeOwn, saw %d", scoped)
	}

	if err := Install(gdb, ""); err != nil {
		t.Fatal(err)
	}
	if err := ScopeOwn(gdb.Model(&db.Scan{})).Count(&scoped).Error; err != nil {
		t.Fatal(err)
	}
	if scoped != 1 {
		t.Fatalf("after backfill ScopeOwn saw %d scans, want 1", scoped)
	}
	if err := SweepRunning(gdb); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, gdb, scan.ID); got != db.ScanFailed {
		t.Fatalf("pre-upgrade running scan: status = %q, want %q", got, db.ScanFailed)
	}
}

// The same upgrade, but joining the shared database under a name — which is
// how a single-instance deployment actually becomes a fleet member. Leaving
// the history at "" scopes it away exactly as the NULLs did: `instance = ”`
// matches no named instance either, so the sweep, retry-all, cancel-all-queued
// and the queued count all skip it, and every single-scan action answers 409.
// The rows nobody owns are therefore adopted by the first named instance.
func TestInstallAdoptsUnownedRowsUnderAName(t *testing.T) {
	gdb := newDB(t, "")
	repo := newRepo(t, gdb, "https://example.invalid/o/r3b")
	scan := db.Scan{RepositoryID: repo.ID, Kind: "skill", Status: db.ScanRunning}
	if err := gdb.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	// Both shapes a pre-fleet database can hold: NULL from AutoMigrate, and
	// "" from a deployment that ran unnamed after the column existed.
	if err := gdb.Exec("UPDATE scans SET instance = NULL").Error; err != nil {
		t.Fatal(err)
	}
	other := db.Scan{RepositoryID: repo.ID, Kind: "skill", Status: db.ScanQueued, Instance: ""}
	if err := gdb.Create(&other).Error; err != nil {
		t.Fatal(err)
	}

	if err := Install(gdb, "sif"); err != nil {
		t.Fatal(err)
	}
	var scoped int64
	if err := ScopeOwn(gdb.Model(&db.Scan{})).Count(&scoped).Error; err != nil {
		t.Fatal(err)
	}
	if scoped != 2 {
		t.Fatalf("after adoption ScopeOwn saw %d scans, want 2", scoped)
	}
	var got db.Scan
	if err := gdb.First(&got, scan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !Owns(got.Instance) {
		t.Fatalf("pre-upgrade scan owner = %q, want %q", got.Instance, Name())
	}
	// The scoped sweep must still reach it, which was the whole point.
	if err := SweepRunning(gdb); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, gdb, scan.ID); st != db.ScanFailed {
		t.Fatalf("pre-upgrade running scan: status = %q, want %q", st, db.ScanFailed)
	}
}

// A member joining later finds nothing unowned: the history already belongs to
// whoever adopted it, and a second instance must not take it from them.
func TestInstallDoesNotStealAnotherInstancesRows(t *testing.T) {
	gdb := newDB(t, "")
	repo := newRepo(t, gdb, "https://example.invalid/o/r3c")
	theirs := db.Scan{RepositoryID: repo.ID, Kind: "skill", Status: db.ScanQueued, Instance: "alessia"}
	if err := gdb.Create(&theirs).Error; err != nil {
		t.Fatal(err)
	}
	if err := Install(gdb, "sif"); err != nil {
		t.Fatal(err)
	}
	var got db.Scan
	if err := gdb.First(&got, theirs.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Instance != "alessia" {
		t.Fatalf("another instance's scan was adopted: owner = %q, want %q", got.Instance, "alessia")
	}
}

// An explicitly named owner survives the stamp, which is what lets an import
// attribute a scan to the instance it came from.
func TestStampKeepsExplicitOwner(t *testing.T) {
	gdb := newDB(t, "mine")
	repo := newRepo(t, gdb, "https://example.invalid/o/r4")
	scan := db.Scan{RepositoryID: repo.ID, Kind: "import", Status: db.ScanDone, Instance: "theirs"}
	if err := gdb.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	var got db.Scan
	if err := gdb.First(&got, scan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Instance != "theirs" {
		t.Fatalf("owner = %q, want %q", got.Instance, "theirs")
	}
}

func TestQueueNamePartitionsByInstance(t *testing.T) {
	t.Cleanup(func() { Install(nil, "") })
	if err := Install(nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := QueueName("scans"); got != "scans" {
		t.Errorf("single instance: QueueName = %q, want %q", got, "scans")
	}
	if err := Install(nil, "miruna"); err != nil {
		t.Fatal(err)
	}
	if got := QueueName("scans"); got != "scans-miruna" {
		t.Errorf("named instance: QueueName = %q, want %q", got, "scans-miruna")
	}
}
