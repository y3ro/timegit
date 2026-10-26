package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// newKimaiStub starts a fake Kimai server whose routes are keyed by
// "METHOD /path", points the global config to it and returns a function
// listing the requests received so far.
func newKimaiStub(t *testing.T, routes map[string]http.HandlerFunc) func() []string {
	t.Helper()

	var (
		mu    sync.Mutex
		calls []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		calls = append(calls, key)
		mu.Unlock()

		handler, ok := routes[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	prevConfig := config
	config = Config{
		KimaiUrl:      server.URL + "/",
		KimaiApiToken: "secret",
		ProjectMap:    map[string]int{"timegit": 1},
	}
	t.Cleanup(func() { config = prevConfig })

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func respondJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

func TestFilterValidRecords(t *testing.T) {
	records := []KimaiRecord{{Id: 0}, {Id: 3}, {Id: -1}, {Id: 5}}

	got := filterValidRecords(records)

	want := []KimaiRecord{{Id: 3}, {Id: 5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterValidRecords() = %v, want %v", got, want)
	}
}

func TestFetchKimaiResourceAuthHeaders(t *testing.T) {
	var headers http.Header
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/projects": func(w http.ResponseWriter, r *http.Request) {
			headers = r.Header.Clone()
			respondJSON("[]")(w, r)
		},
	})

	if _, err := fetchKimaiProjects(); err != nil {
		t.Fatal(err)
	}
	if got := headers.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer secret")
	}
	if got := headers.Get("X-AUTH-USER"); got != "" {
		t.Errorf("X-AUTH-USER = %q, want it unset when using a token", got)
	}

	config.KimaiApiToken = ""
	config.KimaiUsername = "user"
	config.KimaiPassword = "pass"
	if _, err := fetchKimaiProjects(); err != nil {
		t.Fatal(err)
	}
	if headers.Get("X-AUTH-USER") != "user" || headers.Get("X-AUTH-TOKEN") != "pass" {
		t.Errorf("X-AUTH headers = %q/%q, want user/pass", headers.Get("X-AUTH-USER"), headers.Get("X-AUTH-TOKEN"))
	}
	if got := headers.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want it unset without a token", got)
	}
}

func TestFetchKimaiResourceHTTPError(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/projects": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"Invalid credentials"}`)
		},
	})

	_, err := fetchKimaiProjects()
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Invalid credentials") {
		t.Errorf("error %q should contain the status and the response body", err)
	}
}

func TestHttpClientHasTimeout(t *testing.T) {
	if httpClient.Timeout <= 0 {
		t.Fatal("httpClient must have a timeout so Git hooks cannot hang")
	}
}

func TestFetchKimaiActiveRecordsExpanded(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/timesheets/active": respondJSON(`[{
			"id": 5,
			"begin": "2026-10-26T10:00:00+0100",
			"description": "feature/x",
			"project": {"id": 1, "name": "timegit"},
			"activity": {"id": 3, "name": "Development"}
		}, {"id": 6, "project": 2, "activity": 4, "description": null}]`),
	})

	records, err := fetchKimaiActiveRecords()
	if err != nil {
		t.Fatal(err)
	}
	want := []KimaiRecord{
		{Id: 5, Begin: "2026-10-26T10:00:00+0100", Description: "feature/x",
			Project: KimaiRef{Id: 1, Name: "timegit"}, Activity: KimaiRef{Id: 3, Name: "Development"}},
		{Id: 6, Project: KimaiRef{Id: 2}, Activity: KimaiRef{Id: 4}},
	}
	if !reflect.DeepEqual(records, want) {
		t.Errorf("records = %+v, want %+v", records, want)
	}
}

func TestStopKimaiRecordsAllProjects(t *testing.T) {
	calls := newKimaiStub(t, map[string]http.HandlerFunc{
		"PATCH /api/timesheets/5/stop": respondJSON(`{"id":5}`),
		"PATCH /api/timesheets/7/stop": respondJSON(`{"id":7}`),
	})
	active := []KimaiRecord{{Id: 5, Project: KimaiRef{Id: 1}}, {Id: 7, Project: KimaiRef{Id: 2}}}

	running, err := stopKimaiRecords(active, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(running) != 0 {
		t.Errorf("running = %v, want none", running)
	}
	want := []string{"PATCH /api/timesheets/5/stop", "PATCH /api/timesheets/7/stop"}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestStopKimaiRecordsScopedAndKeepingTarget(t *testing.T) {
	calls := newKimaiStub(t, map[string]http.HandlerFunc{
		"PATCH /api/timesheets/5/stop": respondJSON(`{"id":5}`),
	})
	active := []KimaiRecord{
		{Id: 5, Project: KimaiRef{Id: 1}, Activity: KimaiRef{Id: 3}, Description: "main"},
		{Id: 7, Project: KimaiRef{Id: 2}, Activity: KimaiRef{Id: 3}, Description: "feature"},
		{Id: 8, Project: KimaiRef{Id: 1}, Activity: KimaiRef{Id: 3}, Description: "feature"},
	}
	keep := &recordTarget{ProjectID: 1, ActivityID: 3, Description: "feature"}

	running, err := stopKimaiRecords(active, 1, keep)
	if err != nil {
		t.Fatal(err)
	}

	if want := []KimaiRecord{active[1], active[2]}; !reflect.DeepEqual(running, want) {
		t.Errorf("running = %v, want %v", running, want)
	}
	if want := []string{"PATCH /api/timesheets/5/stop"}; !reflect.DeepEqual(calls(), want) {
		t.Errorf("calls = %v, want %v", calls(), want)
	}
}

func TestStopKimaiRecordsWithoutActiveRecords(t *testing.T) {
	calls := newKimaiStub(t, nil)

	if _, err := stopKimaiRecords(nil, 0, nil); err != nil {
		t.Fatalf("stopping with no active records should not fail: %v", err)
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

func TestStopKimaiRecordsFailure(t *testing.T) {
	newKimaiStub(t, nil)

	if _, err := stopKimaiRecords([]KimaiRecord{{Id: 5}}, 0, nil); err == nil {
		t.Fatal("expected an error when the stop request fails")
	}
}

func TestStartRecordTarget(t *testing.T) {
	target := &recordTarget{ProjectID: 1, ActivityID: 3, Description: "feature"}

	t.Run("skips an active matching record", func(t *testing.T) {
		calls := newKimaiStub(t, nil)
		active := []KimaiRecord{{Id: 8, Project: KimaiRef{Id: 1}, Activity: KimaiRef{Id: 3}, Description: "feature"}}

		if err := startRecordTarget(target, active); err != nil {
			t.Fatal(err)
		}
		if got := calls(); len(got) != 0 {
			t.Errorf("calls = %v, want none", got)
		}
	})

	t.Run("starts a record otherwise", func(t *testing.T) {
		var reqBody map[string]interface{}
		newKimaiStub(t, map[string]http.HandlerFunc{
			"POST /api/timesheets": func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&reqBody)
				respondJSON(`{"id":12}`)(w, r)
			},
		})
		active := []KimaiRecord{{Id: 8, Project: KimaiRef{Id: 1}, Activity: KimaiRef{Id: 3}, Description: "main"}}

		if err := startRecordTarget(target, active); err != nil {
			t.Fatal(err)
		}
		if reqBody["description"] != "feature" || reqBody["activity"] != 3.0 {
			t.Errorf("unexpected request body: %v", reqBody)
		}
	})
}

func TestFetchKimaiActivityExactMatchAndEscaping(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/activities": func(w http.ResponseWriter, r *http.Request) {
			query := r.URL.Query()
			if query.Get("term") != "feature/a&b" || query.Get("project") != "1" {
				t.Errorf("unexpected query: %s", r.URL.RawQuery)
			}
			respondJSON(`[{"id":1,"name":"feature/a&b-old"},{"id":2,"name":"Feature/A&B"}]`)(w, r)
		},
	})

	activity, err := fetchKimaiActivity("feature/a&b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Id != 2 {
		t.Errorf("activity id = %d, want 2", activity.Id)
	}
}

func TestFetchKimaiActivityNotFound(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/activities": respondJSON(`[{"id":1,"name":"feature-old"}]`),
	})

	_, err := fetchKimaiActivity("feature", 1)
	var noActivityFoundError *NoActivityFoundError
	if !errors.As(err, &noActivityFoundError) {
		t.Fatalf("error = %v, want a NoActivityFoundError", err)
	}
}

func TestFetchKimaiActivityMultipleMatches(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/activities": respondJSON(`[{"id":1,"name":"feature"},{"id":2,"name":"feature"}]`),
	})

	_, err := fetchKimaiActivity("feature", 1)
	var noActivityFoundError *NoActivityFoundError
	if err == nil || errors.As(err, &noActivityFoundError) {
		t.Fatalf("error = %v, want a multiple activities error", err)
	}
}

func TestResolveKimaiActivityUsesBranch(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/activities": respondJSON(`[{"id":3,"name":"feature"}]`),
	})

	activity, err := resolveKimaiActivity("timegit", "feature", 1)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Id != 3 {
		t.Errorf("activity id = %d, want 3", activity.Id)
	}
}

func TestResolveKimaiActivityFallsBackToProject(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/activities": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("term") == "timegit" {
				respondJSON(`[{"id":4,"name":"timegit"}]`)(w, r)
				return
			}
			respondJSON(`[]`)(w, r)
		},
	})

	activity, err := resolveKimaiActivity("timegit", "feature", 1)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Id != 4 {
		t.Errorf("activity id = %d, want 4", activity.Id)
	}
}

func TestResolveKimaiActivityDetachedHeadCreatesDefault(t *testing.T) {
	var created map[string]interface{}
	calls := newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/activities": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("term"); got != "timegit" {
				t.Errorf("term = %q, want the project name", got)
			}
			respondJSON(`[]`)(w, r)
		},
		"POST /api/activities": func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&created)
			respondJSON(`{"id":9}`)(w, r)
		},
	})

	activity, err := resolveKimaiActivity("timegit", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Id != 9 {
		t.Errorf("activity id = %d, want 9", activity.Id)
	}
	if created["name"] != "timegit" {
		t.Errorf("created activity name = %v, want timegit", created["name"])
	}
	want := []string{"GET /api/activities", "POST /api/activities"}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestResolveKimaiActivityPropagatesErrors(t *testing.T) {
	calls := newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/activities": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})

	activity, err := resolveKimaiActivity("timegit", "feature", 1)
	if err == nil {
		t.Fatalf("expected an error, got activity %v", activity)
	}
	for _, call := range calls() {
		if strings.HasPrefix(call, "POST") {
			t.Errorf("no activity should be created after a server error, got %s", call)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		s       string
		want    bool
	}{
		{"feature/*", "feature/login", true},
		{"feature/*", "feature/a/b", true},
		{"feature/*", "fix/login", false},
		{"*", "anything/at/all", true},
		{"release-?", "release-1", true},
		{"release-?", "release-10", false},
		{"v1.0", "v1x0", false},
		{"main", "main", true},
	}

	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.s); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestResolveKimaiActivityUsesFirstMatchingRule(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/activities": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("term"); got != "Development" {
				t.Errorf("term = %q, want the activity of the first matching rule", got)
			}
			respondJSON(`[{"id":6,"name":"Development"}]`)(w, r)
		},
	})
	config.ActivityRules = []ActivityRule{
		{Branch: "fix/*", Activity: "Bugfixing"},
		{Branch: "feature/*", Activity: "Development"},
		{Branch: "*", Activity: "Other"},
	}

	activity, err := resolveKimaiActivity("timegit", "feature/login", 1)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Id != 6 {
		t.Errorf("activity id = %d, want 6", activity.Id)
	}
}

func TestResolveKimaiActivityRuleNotFoundFallsBack(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/activities": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("term") == "timegit" {
				respondJSON(`[{"id":4,"name":"timegit"}]`)(w, r)
				return
			}
			respondJSON(`[]`)(w, r)
		},
	})
	config.ActivityRules = []ActivityRule{{Branch: "feature/*", Activity: "Missing"}}

	activity, err := resolveKimaiActivity("timegit", "feature/login", 1)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Id != 4 {
		t.Errorf("activity id = %d, want the default activity 4", activity.Id)
	}
}

func TestKimaiRefUnmarshal(t *testing.T) {
	tests := []struct {
		json string
		want KimaiRef
	}{
		{`3`, KimaiRef{Id: 3}},
		{`{"id":3,"name":"Development"}`, KimaiRef{Id: 3, Name: "Development"}},
		{`null`, KimaiRef{}},
	}

	for _, tt := range tests {
		var got KimaiRef
		if err := json.Unmarshal([]byte(tt.json), &got); err != nil {
			t.Fatalf("Unmarshal(%s) error: %v", tt.json, err)
		}
		if got != tt.want {
			t.Errorf("Unmarshal(%s) = %+v, want %+v", tt.json, got, tt.want)
		}
	}
}

func TestRecordDuration(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		begin  string
		want   time.Duration
		wantOk bool
	}{
		{"2026-10-26T11:00:00+0000", 90 * time.Minute, true},
		{"2026-10-26T12:00:00+01:00", 90 * time.Minute, true},
		{"2026-10-26T13:00:00+0000", 0, false},
		{"not a date", 0, false},
		{"", 0, false},
	}

	for _, tt := range tests {
		got, ok := recordDuration(KimaiRecord{Begin: tt.begin}, now)
		if got != tt.want || ok != tt.wantOk {
			t.Errorf("recordDuration(%q) = %v, %v, want %v, %v", tt.begin, got, ok, tt.want, tt.wantOk)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(11*time.Hour + 2*time.Minute + 40*time.Second); got != "11h02m" {
		t.Errorf("formatDuration() = %q, want 11h02m", got)
	}
}

func TestLongRecordWarning(t *testing.T) {
	prevConfig := config
	t.Cleanup(func() { config = prevConfig })
	now := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	record := KimaiRecord{Id: 5, Begin: "2026-10-26T10:00:00+0000"}

	config = Config{}
	if got := longRecordWarning(record, now); !strings.Contains(got, "10h00m") || !strings.Contains(got, "8h00m") {
		t.Errorf("warning = %q, want it to mention the duration and the default limit", got)
	}

	config.MaxRecordHours = 12
	if got := longRecordWarning(record, now); got != "" {
		t.Errorf("warning = %q, want none under the configured limit", got)
	}
}

func TestFormatRecord(t *testing.T) {
	now := time.Date(2026, 10, 26, 11, 15, 0, 0, time.UTC)
	record := KimaiRecord{Id: 5, Begin: "2026-10-26T10:00:00+0000", Description: "feature/x",
		Project: KimaiRef{Id: 1, Name: "timegit"}, Activity: KimaiRef{Id: 3}}

	want := "Record 5: timegit / activity 3 (feature/x), running for 1h15m"
	if got := formatRecord(record, now); got != want {
		t.Errorf("formatRecord() = %q, want %q", got, want)
	}
}

func TestStartKimaiActivityHourlyRate(t *testing.T) {
	tests := []struct {
		name       string
		hourlyRate float64
		wantRate   interface{}
	}{
		{"omitted when not configured", 0, nil},
		{"sent when configured", 42.5, 42.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqBody map[string]interface{}
			newKimaiStub(t, map[string]http.HandlerFunc{
				"POST /api/timesheets": func(w http.ResponseWriter, r *http.Request) {
					json.NewDecoder(r.Body).Decode(&reqBody)
					respondJSON(`{"id":11}`)(w, r)
				},
			})
			config.HourlyRate = tt.hourlyRate

			record, err := startKimaiActivity(1, 2, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := reqBody["description"]; ok {
				t.Errorf("description should be omitted when empty: %v", reqBody)
			}
			if record.Id != 11 {
				t.Errorf("record id = %d, want 11", record.Id)
			}
			if reqBody["project"] != 1.0 || reqBody["activity"] != 2.0 {
				t.Errorf("unexpected request body: %v", reqBody)
			}
			if got := reqBody["hourlyRate"]; got != tt.wantRate {
				t.Errorf("hourlyRate = %v, want %v", got, tt.wantRate)
			}
		})
	}
}

func TestFetchLastKimaiRecordSkipsInvalid(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/timesheets/recent": respondJSON(`[{"id":0},{"id":9}]`),
	})

	record, err := fetchLastKimaiRecord()
	if err != nil {
		t.Fatal(err)
	}
	if record.Id != 9 {
		t.Errorf("record id = %d, want 9", record.Id)
	}
}

func TestRestartLastKimaiRecord(t *testing.T) {
	calls := newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/timesheets/recent":      respondJSON(`[{"id":9}]`),
		"PATCH /api/timesheets/9/restart": respondJSON(`{"id":10}`),
	})

	if err := RestartLastKimaiRecord(); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /api/timesheets/recent", "PATCH /api/timesheets/9/restart"}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestCheckConfigFields(t *testing.T) {
	projectMap := map[string]int{"timegit": 1}
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{"api token", Config{KimaiUrl: "u", KimaiApiToken: "t", ProjectMap: projectMap}, false},
		{"username and password", Config{KimaiUrl: "u", KimaiUsername: "a", KimaiPassword: "b", ProjectMap: projectMap}, false},
		{"username without password", Config{KimaiUrl: "u", KimaiUsername: "a", ProjectMap: projectMap}, true},
		{"no url", Config{KimaiApiToken: "t", ProjectMap: projectMap}, true},
		{"negative hourly rate", Config{KimaiUrl: "u", KimaiApiToken: "t", HourlyRate: -1, ProjectMap: projectMap}, true},
		{"no project map", Config{KimaiUrl: "u", KimaiApiToken: "t"}, true},
		{"negative max record hours", Config{KimaiUrl: "u", KimaiApiToken: "t", MaxRecordHours: -1, ProjectMap: projectMap}, true},
		{"negative idle minutes", Config{KimaiUrl: "u", KimaiApiToken: "t", IdleMinutes: -1, ProjectMap: projectMap}, true},
		{"activity rules", Config{KimaiUrl: "u", KimaiApiToken: "t", ProjectMap: projectMap,
			ActivityRules: []ActivityRule{{Branch: "fix/*", Activity: "Bugfixing"}}}, false},
		{"activity rule without activity", Config{KimaiUrl: "u", KimaiApiToken: "t", ProjectMap: projectMap,
			ActivityRules: []ActivityRule{{Branch: "fix/*"}}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkConfigFields(&tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkConfigFields() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReadConfigLegacyFormat(t *testing.T) {
	prevConfig := config
	t.Cleanup(func() { config = prevConfig })

	configPath := filepath.Join(t.TempDir(), configFileName)
	legacyConfig := `{
		"KimaiUrl": "https://kimai.example.com",
		"KimaiUsername": "user",
		"KimaiPassword": "pass",
		"HourlyRate": 100,
		"ProjectMap": {"timegit": 1}
	}`
	if err := os.WriteFile(configPath, []byte(legacyConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := readConfig(configPath); err != nil {
		t.Fatal(err)
	}
	if config.HourlyRate != 100 || config.KimaiUsername != "user" {
		t.Errorf("unexpected config: %+v", config)
	}
}

func TestGetConfigDir(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	got, err := getConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(homeDir, ".config"); got != want {
		t.Errorf("getConfigDir() = %q, want %q", got, want)
	}
}

func TestParseCliArgsWithoutOperations(t *testing.T) {
	missingConfig := filepath.Join(t.TempDir(), "missing.json")
	for _, args := range [][]string{{}, {"-config", missingConfig}} {
		if err := parseCliArgsAndRun(args); err != nil {
			t.Errorf("parseCliArgsAndRun(%v) = %v, want usage without reading the config", args, err)
		}
	}
}

func TestParseCliArgsStartAndRestart(t *testing.T) {
	missingConfig := filepath.Join(t.TempDir(), "missing.json")

	err := parseCliArgsAndRun([]string{"-stop", "-start", "-restart", "-config", missingConfig})
	if err == nil || !strings.Contains(err.Error(), "at the same time") {
		t.Fatalf("error = %v, want the start/restart validation error before anything else", err)
	}
}

func TestParseCliArgsInvalidCombinations(t *testing.T) {
	missingConfig := filepath.Join(t.TempDir(), "missing.json")
	tests := [][]string{
		{"-all", "-start"},
		{"-install-hook", "-status"},
		{"-stop-idle", "-start"},
	}

	for _, args := range tests {
		args = append(args, "-config", missingConfig)
		err := parseCliArgsAndRun(args)
		if err == nil || strings.Contains(err.Error(), "missing.json") {
			t.Errorf("parseCliArgsAndRun(%v) = %v, want a validation error before reading the config", args, err)
		}
	}
}

// newGitRepo creates a Git repository in a folder called name, isolated from
// the user's Git config, and makes it the working directory for the test.
func newGitRepo(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	baseDir := t.TempDir()
	emptyConfig := filepath.Join(baseDir, "gitconfig")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", emptyConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Keep the state file written by -start out of the user's config folder
	t.Setenv("HOME", baseDir)
	t.Setenv("USERPROFILE", baseDir)

	repoDir := filepath.Join(baseDir, name)
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "init", "-q")

	prevDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repoDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(prevDir) })

	return repoDir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// commitFiles writes and commits files, recording the reflog entry at
// committedAt.
func commitFiles(t *testing.T, repoDir string, committedAt time.Time, files map[string]string) {
	t.Helper()
	for name, content := range files {
		writeFile(t, filepath.Join(repoDir, name), content)
	}
	t.Setenv("GIT_COMMITTER_DATE", fmt.Sprintf("%d +0000", committedAt.Unix()))
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "-c", "user.name=timegit", "-c", "user.email=timegit@example.com", "commit", "-q", "-m", "commit")
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFileAt(t *testing.T, path string, content string, modTime time.Time) {
	t.Helper()
	writeFile(t, path, content)
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func TestGetLastRepoActivity(t *testing.T) {
	repoDir := newGitRepo(t, "timegit")
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	lastActivity, err := getLastRepoActivity(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if !lastActivity.IsZero() {
		t.Errorf("activity of an empty repository = %v, want none", lastActivity)
	}

	commitFiles(t, repoDir, base, map[string]string{"tracked.txt": "a", ".gitignore": "build/\n"})
	steps := []struct {
		name string
		path string
		want time.Time
	}{
		{"commit", "", base},
		{"untracked file", "notes.txt", base.Add(time.Hour)},
		{"modified file", "tracked.txt", base.Add(2 * time.Hour)},
		{"ignored file", filepath.Join("build", "out.bin"), base.Add(2 * time.Hour)},
		{"older untracked file", "old.txt", base.Add(2 * time.Hour)},
	}
	modTimes := map[string]time.Time{
		"notes.txt":                       base.Add(time.Hour),
		"tracked.txt":                     base.Add(2 * time.Hour),
		filepath.Join("build", "out.bin"): base.Add(3 * time.Hour),
		"old.txt":                         base.Add(-time.Hour),
	}

	for _, step := range steps {
		if step.path != "" {
			writeFileAt(t, filepath.Join(repoDir, step.path), "changed", modTimes[step.path])
		}
		got, err := getLastRepoActivity(repoDir)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(step.want) {
			t.Errorf("after %s: last activity = %v, want %v", step.name, got.UTC(), step.want)
		}
	}
}

func TestStopIdleKimaiRecords(t *testing.T) {
	repoDir := newGitRepo(t, "timegit")
	lastActivity := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	commitFiles(t, repoDir, lastActivity, map[string]string{"tracked.txt": "a"})
	activeRecords := fmt.Sprintf(`[
		{"id":5,"begin":%q,"project":{"id":1}},
		{"id":7,"begin":%q,"project":{"id":3}}
	]`, formatKimaiTime(lastActivity.Add(-time.Hour)), formatKimaiTime(lastActivity))

	t.Run("stops idle records at the last activity", func(t *testing.T) {
		var reqBody map[string]interface{}
		calls := newKimaiStub(t, map[string]http.HandlerFunc{
			"GET /api/timesheets/active": respondJSON(activeRecords),
			"PATCH /api/timesheets/5": func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&reqBody)
				respondJSON(`{"id":5}`)(w, r)
			},
		})

		if err := StopIdleKimaiRecords(); err != nil {
			t.Fatal(err)
		}
		if got, want := reqBody["end"], formatKimaiTime(lastActivity); got != want {
			t.Errorf("end = %v, want %v", got, want)
		}
		want := []string{"GET /api/timesheets/active", "PATCH /api/timesheets/5"}
		if got := calls(); !reflect.DeepEqual(got, want) {
			t.Errorf("calls = %v, want %v (record 7 has no known repository)", got, want)
		}
	})

	t.Run("keeps records with recent activity", func(t *testing.T) {
		calls := newKimaiStub(t, map[string]http.HandlerFunc{
			"GET /api/timesheets/active": respondJSON(activeRecords),
		})
		config.IdleMinutes = 180

		if err := StopIdleKimaiRecords(); err != nil {
			t.Fatal(err)
		}
		if got := calls(); len(got) != 1 {
			t.Errorf("calls = %v, want only the active records request", got)
		}
	})

	t.Run("counts the record start as activity", func(t *testing.T) {
		calls := newKimaiStub(t, map[string]http.HandlerFunc{
			"GET /api/timesheets/active": respondJSON(fmt.Sprintf(`[{"id":5,"begin":%q,"project":{"id":1}}]`,
				formatKimaiTime(time.Now().Add(-time.Minute)))),
		})

		if err := StopIdleKimaiRecords(); err != nil {
			t.Fatal(err)
		}
		if got := calls(); len(got) != 1 {
			t.Errorf("calls = %v, want a just restarted record to be kept", got)
		}
	})
}

func TestSaveRepoPath(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	for projectID, repoRoot := range map[int]string{1: "/repos/timegit", 2: "/repos/other"} {
		if err := saveRepoPath(projectID, repoRoot); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveRepoPath(1, "/repos/timegit-worktree"); err != nil {
		t.Fatal(err)
	}

	state, err := readState()
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: "/repos/timegit-worktree", 2: "/repos/other"}
	if !reflect.DeepEqual(state.RepoPaths, want) {
		t.Errorf("RepoPaths = %v, want %v", state.RepoPaths, want)
	}
}

func TestGetProjectName(t *testing.T) {
	repoDir := newGitRepo(t, "timegit-worktree")

	got, err := getProjectName()
	if err != nil {
		t.Fatal(err)
	}
	if got != "timegit-worktree" {
		t.Errorf("getProjectName() = %q, want the folder name", got)
	}

	runGit(t, repoDir, "config", gitProjectConfigKey, "timegit")
	got, err = getProjectName()
	if err != nil {
		t.Fatal(err)
	}
	if got != "timegit" {
		t.Errorf("getProjectName() = %q, want the %s value", got, gitProjectConfigKey)
	}
}

func TestRunRecordOpsSwitchesBranchRecord(t *testing.T) {
	repoDir := newGitRepo(t, "timegit")
	runGit(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/feature/login")
	activeRecords := `[
		{"id":5,"project":{"id":1},"activity":{"id":3},"description":"main"},
		{"id":7,"project":{"id":2},"activity":{"id":8},"description":"other"}
	]`

	tests := []struct {
		name      string
		stopAll   bool
		active    string
		wantCalls []string
	}{
		{"stops the current project only", false, activeRecords, []string{
			"GET /api/activities", "GET /api/timesheets/active",
			"PATCH /api/timesheets/5/stop", "POST /api/timesheets",
		}},
		{"stops every project with -all", true, activeRecords, []string{
			"GET /api/activities", "GET /api/timesheets/active",
			"PATCH /api/timesheets/5/stop", "PATCH /api/timesheets/7/stop", "POST /api/timesheets",
		}},
		{"keeps the record of the current branch", true,
			`[{"id":9,"project":{"id":1},"activity":{"id":3},"description":"feature/login"}]`,
			[]string{"GET /api/activities", "GET /api/timesheets/active"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := newKimaiStub(t, map[string]http.HandlerFunc{
				"GET /api/activities":          respondJSON(`[{"id":3,"name":"Development"}]`),
				"GET /api/timesheets/active":   respondJSON(tt.active),
				"PATCH /api/timesheets/5/stop": respondJSON(`{"id":5}`),
				"PATCH /api/timesheets/7/stop": respondJSON(`{"id":7}`),
				"POST /api/timesheets":         respondJSON(`{"id":10}`),
			})
			config.ActivityRules = []ActivityRule{{Branch: "feature/*", Activity: "Development"}}

			if err := runRecordOps(true, tt.stopAll, true); err != nil {
				t.Fatal(err)
			}
			if got := calls(); !reflect.DeepEqual(got, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", got, tt.wantCalls)
			}
		})
	}
}

func TestInstallGitHook(t *testing.T) {
	repoDir := newGitRepo(t, "timegit")
	hookPath := filepath.Join(repoDir, ".git", "hooks", postCheckoutHookName)

	if err := InstallGitHook(); err != nil {
		t.Fatal(err)
	}
	hook, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(hook), "#!/bin/sh\n") || !strings.Contains(string(hook), `[ "$3" = "1" ]`) {
		t.Errorf("unexpected hook contents:\n%s", hook)
	}

	if err := InstallGitHook(); err != nil {
		t.Errorf("reinstalling the hook should be a no-op, got %v", err)
	}

	foreignHook := "#!/bin/sh\necho custom\n"
	if err := os.WriteFile(hookPath, []byte(foreignHook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InstallGitHook(); err == nil {
		t.Error("expected an error when a foreign hook exists")
	}
	if hook, _ := os.ReadFile(hookPath); string(hook) != foreignHook {
		t.Errorf("a foreign hook must not be overwritten, got:\n%s", hook)
	}
}
