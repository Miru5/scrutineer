package fleet

import (
	"gorm.io/gorm"

	"scrutineer/internal/db"
)

// ScopeOwn restricts a query over scans to the ones this instance owns. It is
// the write half of the rule in the package doc: wrap every mutating query in
// it, leave every listing alone.
//
// With no instance name it matches the empty identity every row carries, so a
// single-instance deployment is unaffected. The column is qualified because
// some call sites join.
func ScopeOwn(q *gorm.DB) *gorm.DB {
	return q.Where("scans.instance = ?", instance)
}

// FilterScans narrows a scan listing to one member's work, for the Instance
// filter on the scans page. An empty name means every instance, which is the
// unfiltered fleet-wide view.
func FilterScans(q *gorm.DB, name string) *gorm.DB {
	if name == "" {
		return q
	}
	return q.Where("scans.instance = ?", name)
}

// FilterRepos narrows a repository listing to the repositories one member has
// scanned. Membership is by scan, not by who added the row: on a shared
// database the repository is everyone's, the scans are somebody's.
func FilterRepos(q *gorm.DB, name string) *gorm.DB {
	if name == "" {
		return q
	}
	return q.Where(
		"EXISTS (SELECT 1 FROM scans WHERE scans.repository_id = repositories.id AND scans.instance = ?)",
		name)
}

// Instances lists the instance names that appear on scans, for the fleet
// filters. Empty on a single-instance deployment, whose rows carry no name at
// all — which is how the templates know not to render the column.
func Instances(gdb *gorm.DB) []string {
	var names []string
	gdb.Model(&db.Scan{}).Where("instance != ''").Distinct("instance").
		Order("instance").Pluck("instance", &names)
	return names
}

// InstancesByRepo maps each of repoIDs to the instances that have scanned it,
// for the Instances column on the repositories page. One query rather than one
// per row.
func InstancesByRepo(gdb *gorm.DB, repoIDs []uint) map[uint][]string {
	out := map[uint][]string{}
	if len(repoIDs) == 0 {
		return out
	}
	var rows []struct {
		RepositoryID uint
		Instance     string
	}
	gdb.Model(&db.Scan{}).
		Select("DISTINCT repository_id, instance").
		Where("repository_id IN ? AND instance != ''", repoIDs).
		Order("instance").
		Scan(&rows)
	for _, r := range rows {
		out[r.RepositoryID] = append(out[r.RepositoryID], r.Instance)
	}
	return out
}
