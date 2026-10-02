package fleet

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"gorm.io/gorm"

	"scrutineer/internal/db"
)

// instance identifies this process among the instances sharing a database. A
// package-level value rather than a parameter threaded through every call
// site: it is fixed for the life of the process, and every scan created
// anywhere in the program must carry it or SweepRunning cannot tell whose
// scans are whose. Empty means single-instance — the SQLite default, and what
// rows written before this layer existed carry.
var instance string

// stampCallback is the GORM create hook that writes the owner onto new scans.
// Named so it can be looked up, and registered once by Install.
const stampCallback = "fleet:stamp_scan_instance"

// Install turns this process into a named member of a fleet sharing one
// database. Call it once at startup, after db.Open and before anything
// creates a scan. An empty name installs nothing but the backfill, which is
// what a single-instance deployment wants: every helper here then behaves
// exactly as the unscoped code it replaced.
func Install(gdb *gorm.DB, name string) error {
	instance = strings.TrimSpace(name)
	if gdb == nil {
		return nil
	}
	if err := backfill(gdb); err != nil {
		return err
	}
	// Idempotent: Install runs again in tests and on a reconfigured handle,
	// and GORM warns rather than failing on a duplicate registration.
	create := gdb.Callback().Create()
	if create.Get(stampCallback) != nil {
		if err := create.Replace(stampCallback, stampScanInstance); err != nil {
			return fmt.Errorf("fleet: replace scan owner stamp: %w", err)
		}
		return nil
	}
	if err := create.Before("gorm:create").
		Register(stampCallback, stampScanInstance); err != nil {
		return fmt.Errorf("fleet: register scan owner stamp: %w", err)
	}
	return nil
}

// Name reports this instance's identity, empty on a single-instance
// deployment.
func Name() string { return instance }

// Enabled reports whether this process is running as a named instance.
// Templates use it to decide whether the fleet columns and filters are worth
// showing at all.
func Enabled() bool { return instance != "" }

// Owns reports whether a scan stamped with owner belongs to this instance.
func Owns(owner string) bool { return owner == instance }

// backfill gives rows written before this layer the empty identity a
// single-instance deployment writes.
//
// Without it the upgrade silently breaks every scoped action. AutoMigrate adds
// Instance as a nullable column, so existing scans come back NULL, and
// `instance = ”` never matches NULL: ScopeOwn would see none of them, which
// means cancel-all-queued, retry-failed, the running count and SweepRunning
// would all skip every pre-upgrade row, leaving a restarted scan spinning
// forever. Both updates are no-ops once they have run.
func backfill(gdb *gorm.DB) error {
	if err := gdb.Model(&db.Scan{}).Where("instance IS NULL").
		Update("instance", "").Error; err != nil {
		return fmt.Errorf("fleet: backfill scan owners: %w", err)
	}
	if instance == "" {
		return nil
	}
	// Then adopt what a nameless deployment wrote. The empty identity above is
	// only the right answer while nothing is named: once this instance has a
	// name, `instance = ''` no longer matches it either, so the history would
	// be scoped away just as thoroughly as the NULLs were — invisible to the
	// sweep, retry-all, cancel-all-queued and the queued count, and 409 from
	// every single-scan action. The first named instance to start therefore
	// claims the rows nobody owns, which is what "upgrading an existing
	// deployment" means: they were all written by the one deployment that
	// existed. A later member finds nothing unowned left to take.
	if err := gdb.Model(&db.Scan{}).Where("instance = ?", "").
		Update("instance", instance).Error; err != nil {
		return fmt.Errorf("fleet: adopt unowned scans: %w", err)
	}
	return nil
}

// stampScanInstance writes this instance's name onto every scan being created
// that does not name an owner already.
//
// A create callback rather than a BeforeCreate method on db.Scan: the model
// stays a plain record with no knowledge of fleets, and the behaviour arrives
// and leaves with this package. An explicitly set owner wins, which keeps
// imports and tests able to attribute a scan to somebody else.
func stampScanInstance(tx *gorm.DB) {
	if instance == "" || tx.Statement == nil || tx.Statement.Schema == nil {
		return
	}
	if tx.Statement.Table != "scans" {
		return
	}
	field := tx.Statement.Schema.LookUpField("Instance")
	if field == nil {
		return
	}
	ctx := tx.Statement.Context
	set := func(elem reflect.Value) {
		if v, zero := field.ValueOf(ctx, elem); !zero {
			if s, ok := v.(string); ok && s != "" {
				return
			}
		}
		if err := field.Set(ctx, elem, instance); err != nil {
			tx.Logger.Warn(ctx, "fleet: stamp scan owner: %v", err)
		}
	}
	switch rv := tx.Statement.ReflectValue; rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := range rv.Len() {
			set(rv.Index(i))
		}
	case reflect.Struct:
		set(rv)
	}
}

// SweepRunning marks this instance's still-running scans as failed. Call once
// at startup: a running row with no worker attached means the previous process
// died mid-job and the UI would otherwise show a spinner forever.
//
// This replaces db.SweepRunning, and the scoping is why. Unscoped on a shared
// database, one instance restarting would mark every other member's in-flight
// scans failed while their work carried on — the rows would say failed, the
// containers would keep burning model quota, and the UI would show a result
// that never happened. With no instance name the filter matches the empty
// identity every row carries, so a single-instance deployment sweeps exactly
// what it always did.
func SweepRunning(gdb *gorm.DB) error {
	return ScopeOwn(gdb.Model(&db.Scan{}).Where("status = ?", db.ScanRunning)).
		Updates(map[string]any{
			"status":      db.ScanFailed,
			"error":       "server restarted during run",
			"finished_at": new(time.Now()),
		}).Error
}
