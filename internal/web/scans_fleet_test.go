package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scrutineer/internal/db"
)

func queueMessages(t *testing.T, s *Server) int {
	t.Helper()
	sqldb, err := s.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sqldb.QueryRow(`select count(*) from goqite`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScanCancel_removesTheQueuedMessage(t *testing.T) {
	// Cancelling a scan that is still queued must take its goqite message
	// with it. Left behind, the message survives until a consumer of that
	// partition picks it up and drops the cancelled row — never, for a
	// retired instance — and meanwhile the queue table fills with dead rows.
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/q", Name: "q"}
	s.DB.Create(&repo)
	scan := db.Scan{RepositoryID: repo.ID, Kind: "skill", Status: db.ScanQueued,
		StatusPriority: db.StatusPriorityFor(db.ScanQueued)}
	s.DB.Create(&scan)
	msgID, err := s.Queue.Enqueue(t.Context(), scan.Kind, scan.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordQueueMessage(s.DB, scan.ID, msgID); err != nil {
		t.Fatal(err)
	}
	if got := queueMessages(t, s); got != 1 {
		t.Fatalf("messages before cancel = %d, want 1", got)
	}

	r := localReq("POST", fmt.Sprintf("/scans/%d/cancel", scan.ID))
	r.Header.Set("HX-Request", "true")
	r.SetPathValue("id", fmt.Sprint(scan.ID))
	w := httptest.NewRecorder()
	s.scanCancel(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body)
	}
	var got db.Scan
	s.DB.First(&got, scan.ID)
	if got.Status != db.ScanCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if n := queueMessages(t, s); n != 0 {
		t.Errorf("messages after cancel = %d, want 0", n)
	}
}

func TestScansCancelAll_removesTheQueuedMessages(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/a", Name: "a"}
	other := db.Repository{URL: "https://example.com/b", Name: "b"}
	s.DB.Create(&repo)
	s.DB.Create(&other)
	enqueue := func(repoID uint) db.Scan {
		sc := db.Scan{RepositoryID: repoID, Kind: "skill", Status: db.ScanQueued,
			StatusPriority: db.StatusPriorityFor(db.ScanQueued)}
		s.DB.Create(&sc)
		id, err := s.Queue.Enqueue(t.Context(), sc.Kind, sc.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RecordQueueMessage(s.DB, sc.ID, id); err != nil {
			t.Fatal(err)
		}
		return sc
	}
	enqueue(repo.ID)
	enqueue(repo.ID)
	kept := enqueue(other.ID)
	if got := queueMessages(t, s); got != 3 {
		t.Fatalf("messages before cancel-all = %d, want 3", got)
	}

	r := localReq("POST", fmt.Sprintf("/scans/cancel-all?repository=%d", repo.ID))
	r.Header.Set("HX-Request", "true")
	s.scansCancelAll(httptest.NewRecorder(), r)

	// Only the other repository's message survives.
	if got := queueMessages(t, s); got != 1 {
		t.Errorf("messages after cancel-all = %d, want 1 (the other repo's)", got)
	}
	var keptRow db.Scan
	s.DB.First(&keptRow, kept.ID)
	if keptRow.Status != db.ScanQueued || keptRow.QueueMessageID == "" {
		t.Errorf("other repo's scan touched: status=%q message=%q", keptRow.Status, keptRow.QueueMessageID)
	}
}

func TestJobs_showsAndFiltersByInstanceOnASharedDatabase(t *testing.T) {
	// On a shared database the scans page lists every member's scans; the
	// instance column says whose each one is and the filter narrows the list
	// to one member. Rows stamped with no instance (single-instance
	// deployments) get neither.
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/fleet", Name: "fleet"}
	s.DB.Create(&repo)
	mk := func(instance, skill string) db.Scan {
		sc := db.Scan{RepositoryID: repo.ID, Kind: "skill", SkillName: skill, Instance: instance,
			Status: db.ScanDone, StatusPriority: db.StatusPriorityFor(db.ScanDone)}
		s.DB.Create(&sc)
		return sc
	}
	alessia := mk("alessia", "critic")
	ioana := mk("ioana", "recon")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, localReq("GET", "/scans"))
	body := w.Body.String()
	if !strings.Contains(body, "<th>Instance</th>") {
		t.Errorf("scans page lacks the Instance column: %s", body)
	}
	for _, name := range []string{"alessia", "ioana"} {
		if !strings.Contains(body, fmt.Sprintf(`href="/scans?instance=%s"`, name)) {
			t.Errorf("row for %s does not link to its instance filter", name)
		}
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, localReq("GET", "/scans?instance=alessia"))
	body = w.Body.String()
	if !strings.Contains(body, fmt.Sprintf(`id="scan-%d"`, alessia.ID)) {
		t.Errorf("filtered page lost alessia's scan: %s", body)
	}
	if strings.Contains(body, fmt.Sprintf(`id="scan-%d"`, ioana.ID)) {
		t.Errorf("filtered page still shows ioana's scan")
	}
	// The other filters carry the instance along so switching skill or
	// status keeps the member selected.
	if !strings.Contains(body, `&instance=alessia"`) {
		t.Errorf("filter links dropped the instance parameter: %s", body)
	}
}

func TestJobs_hidesTheInstanceColumnWhenNoScanCarriesOne(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/solo", Name: "solo"}
	s.DB.Create(&repo)
	sc := db.Scan{RepositoryID: repo.ID, Kind: "skill", SkillName: "critic",
		Status: db.ScanDone, StatusPriority: db.StatusPriorityFor(db.ScanDone)}
	s.DB.Create(&sc)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, localReq("GET", "/scans"))
	if strings.Contains(w.Body.String(), "<th>Instance</th>") {
		t.Error("single-instance deployment shows an Instance column")
	}
}

func TestRepoList_showsAndFiltersByScanningInstance(t *testing.T) {
	// A repository has no owner of its own on the shared database; the
	// Instances column names the members whose scans touched it, and the
	// filter keeps the repositories one member has scanned.
	s, done := newTestServer(t)
	defer done()
	mine := db.Repository{URL: "https://example.com/mine", Name: "mine"}
	theirs := db.Repository{URL: "https://example.com/theirs", Name: "theirs"}
	both := db.Repository{URL: "https://example.com/both", Name: "both"}
	for _, r := range []*db.Repository{&mine, &theirs, &both} {
		s.DB.Create(r)
	}
	mk := func(repoID uint, instance string) {
		sc := db.Scan{RepositoryID: repoID, Kind: "skill", SkillName: "critic", Instance: instance,
			Status: db.ScanDone, StatusPriority: db.StatusPriorityFor(db.ScanDone)}
		s.DB.Create(&sc)
	}
	mk(mine.ID, "miruna")
	mk(theirs.ID, "alessia")
	mk(both.ID, "miruna")
	mk(both.ID, "alessia")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, localReq("GET", "/"))
	body := w.Body.String()
	if !strings.Contains(body, "<th>Instances</th>") {
		t.Errorf("repository list lacks the Instances column: %s", body)
	}
	row := requireRepoListRow(t, body, both.ID)
	for _, name := range []string{"miruna", "alessia"} {
		if !strings.Contains(row, fmt.Sprintf(`href="/?instance=%s"`, name)) {
			t.Errorf("row for a repo scanned by both lacks the %s badge: %s", name, row)
		}
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, localReq("GET", "/?instance=alessia"))
	body = w.Body.String()
	for _, id := range []uint{theirs.ID, both.ID} {
		if !strings.Contains(body, fmt.Sprintf(`<tr id="repo-%d">`, id)) {
			t.Errorf("filtered list dropped repo %d, which alessia scanned", id)
		}
	}
	if strings.Contains(body, fmt.Sprintf(`<tr id="repo-%d">`, mine.ID)) {
		t.Errorf("filtered list still shows a repo alessia never scanned")
	}
	if !strings.Contains(body, `&instance=alessia"`) {
		t.Errorf("language and sort links dropped the instance parameter")
	}
}

func TestRepoScansTab_rowsCarryTheInstanceBadge(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo := db.Repository{URL: "https://example.com/tab", Name: "tab"}
	s.DB.Create(&repo)
	sc := db.Scan{RepositoryID: repo.ID, Kind: "skill", SkillName: "critic", Instance: "ioana",
		Status: db.ScanDone, StatusPriority: db.StatusPriorityFor(db.ScanDone)}
	s.DB.Create(&sc)

	r := localReq("GET", fmt.Sprintf("/repositories/%d/scans", repo.ID))
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `href="/scans?instance=ioana"`) {
		t.Errorf("scans tab row lacks the instance badge: %s", w.Body.String())
	}
}
