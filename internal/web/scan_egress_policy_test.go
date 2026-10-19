package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"scrutineer/internal/db"
	"scrutineer/internal/worker"
)

func seedEgressPolicyFixture(t *testing.T, s *Server) (db.Repository, db.Skill) {
	t.Helper()
	repo := db.Repository{URL: "https://example.com/r", Name: "r"}
	s.DB.Create(&repo)
	skill := db.Skill{Name: "hello", Description: "d", Body: "b", OutputFile: "report.json",
		OutputKind: "freeform", Version: 1, Active: true, Source: "ui"}
	s.DB.Create(&skill)
	return repo, skill
}

func postSkillScan(s *Server, repo db.Repository, skill db.Skill, policy string) *httptest.ResponseRecorder {
	form := url.Values{"skill_id": {strconv.Itoa(int(skill.ID))}}
	if policy != "" {
		form.Set("egress_policy", policy)
	}
	req := httptest.NewRequest("POST", "/repositories/"+strconv.Itoa(int(repo.ID))+"/skill-scan", strings.NewReader(form.Encode()))
	asOperator(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func skillScans(s *Server) []db.Scan {
	var scans []db.Scan
	s.DB.Where("kind = ?", worker.JobSkill).Order("id").Find(&scans)
	return scans
}

func TestSkillRun_storesScanEgressPolicy(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	s.ScanEgressPolicies = []string{"net", "registries"}
	repo, skill := seedEgressPolicyFixture(t, s)

	if w := postSkillScan(s, repo, skill, " registries "); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if w := postSkillScan(s, repo, skill, ""); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	scans := skillScans(s)
	if len(scans) != 2 || scans[0].EgressPolicy != "registries" || scans[1].EgressPolicy != "" {
		t.Fatalf("scans = %+v, want policies [registries, empty]", scans)
	}
}

func TestSkillRun_unknownScanEgressPolicyIs422(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	s.ScanEgressPolicies = []string{"net"}
	repo, skill := seedEgressPolicyFixture(t, s)

	for _, policy := range []string{"nope", "NET"} {
		w := postSkillScan(s, repo, skill, policy)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), policy) {
			t.Fatalf("policy %q: status %d: %s, want 422 naming the policy", policy, w.Code, w.Body)
		}
	}
	if n := len(skillScans(s)); n != 0 {
		t.Fatalf("%d scans created, want 0", n)
	}

	// With no scan policies configured at all every name is unknown.
	s.ScanEgressPolicies = nil
	if w := postSkillScan(s, repo, skill, "net"); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d: %s, want 422", w.Code, w.Body)
	}
}

func retryScan(s *Server, id uint) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/scans/"+strconv.Itoa(int(id))+"/retry", nil)
	asOperator(req)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestScanRetry_carriesScanEgressPolicy(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	s.ScanEgressPolicies = []string{"net"}
	repo, skill := seedEgressPolicyFixture(t, s)
	if w := postSkillScan(s, repo, skill, "net"); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	first := skillScans(s)[0]

	if w := retryScan(s, first.ID); w.Code != http.StatusSeeOther {
		t.Fatalf("retry status %d: %s", w.Code, w.Body)
	}
	scans := skillScans(s)
	if len(scans) != 2 || scans[1].EgressPolicy != "net" {
		t.Fatalf("scans = %+v, want the retry to carry policy net", scans)
	}

	// The policy leaves the config: a retry must fail rather than run without it.
	s.ScanEgressPolicies = nil
	w := retryScan(s, first.ID)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "net") {
		t.Fatalf("retry after removal: status %d: %s, want 422", w.Code, w.Body)
	}
	if n := len(skillScans(s)); n != 2 {
		t.Fatalf("%d scans after refused retry, want 2", n)
	}
}

func TestScansRetryFailed_carriesScanEgressPolicy(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	s.ScanEgressPolicies = []string{"net"}
	repo, skill := seedEgressPolicyFixture(t, s)
	failed := db.Scan{RepositoryID: repo.ID, Kind: worker.JobSkill, Status: db.ScanFailed,
		StatusPriority: db.StatusPriorityFor(db.ScanFailed), SkillID: &skill.ID, SkillName: skill.Name, EgressPolicy: "net"}
	s.DB.Create(&failed)

	w := httptest.NewRecorder()
	s.scansRetryFailed(w, localReq("POST", "/scans/retry-failed"))
	var retry db.Scan
	if err := s.DB.Where("parent_scan_id = ?", failed.ID).First(&retry).Error; err != nil {
		t.Fatalf("no retry created: %v", err)
	}
	if retry.EgressPolicy != "net" {
		t.Errorf("retry EgressPolicy = %q, want net", retry.EgressPolicy)
	}
}

func TestAPIRunSkill_ignoresEgressPolicyAndDoesNotInherit(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	s.ScanEgressPolicies = []string{"net"}
	repo, caller := seedRunningScan(t, s)
	// The calling scan itself runs with a policy, which its children must not get.
	s.DB.Model(&caller).Update("egress_policy", "net")
	skill := db.Skill{Name: "metadata", Description: "m", Body: "b", OutputFile: "report.json", Version: 1, Active: true, Source: "ui"}
	s.DB.Create(&skill)

	w := runSkillAPIJSON(t, s, repo, caller, "metadata", `{"egress_policy":"net","model":"fake"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var row db.Scan
	if err := s.DB.Where("skill_id = ?", skill.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.EgressPolicy != "" {
		t.Errorf("API-enqueued scan EgressPolicy = %q, want empty", row.EgressPolicy)
	}
}

func TestRepoShow_rendersEgressPolicyPickerOnlyWhenConfigured(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo, _ := seedEgressPolicyFixture(t, s)
	get := func() string {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, localReq("GET", "/repositories/"+strconv.Itoa(int(repo.ID))))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	if body := get(); strings.Contains(body, `name="egress_policy"`) {
		t.Error("picker rendered with no scan policies configured")
	}
	s.ScanEgressPolicies = []string{"net", "registries"}
	body := get()
	for _, want := range []string{`name="egress_policy"`, `aria-label="Egress policy"`, "No extra egress", `<option value="registries">registries</option>`} {
		if !strings.Contains(body, want) {
			t.Errorf("repo page lacks %q", want)
		}
	}
}

func TestScanShow_displaysEgressPolicy(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	repo, skill := seedEgressPolicyFixture(t, s)
	scan := db.Scan{RepositoryID: repo.ID, Kind: worker.JobSkill, Status: db.ScanDone, SkillID: &skill.ID, SkillName: skill.Name, EgressPolicy: "registries"}
	s.DB.Create(&scan)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, localReq("GET", "/scans/"+strconv.Itoa(int(scan.ID))))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Egress policy") || !strings.Contains(w.Body.String(), "registries") {
		t.Fatalf("status %d; scan page lacks the egress policy row", w.Code)
	}
}
