package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
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

func TestStopCurrentKimaiActivities(t *testing.T) {
	calls := newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/timesheets/active":   respondJSON(`[{"id":5},{"id":7}]`),
		"PATCH /api/timesheets/5/stop": respondJSON(`{"id":5}`),
		"PATCH /api/timesheets/7/stop": respondJSON(`{"id":7}`),
	})

	if err := StopCurrentKimaiActivities(); err != nil {
		t.Fatal(err)
	}

	want := []string{"GET /api/timesheets/active", "PATCH /api/timesheets/5/stop", "PATCH /api/timesheets/7/stop"}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestStopCurrentKimaiActivitiesWithoutActiveRecords(t *testing.T) {
	calls := newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/timesheets/active": respondJSON(`[]`),
	})

	if err := StopCurrentKimaiActivities(); err != nil {
		t.Fatalf("stopping with no active records should not fail: %v", err)
	}
	if got := calls(); len(got) != 1 {
		t.Errorf("calls = %v, want only the active records request", got)
	}
}

func TestStopCurrentKimaiActivitiesFailure(t *testing.T) {
	newKimaiStub(t, map[string]http.HandlerFunc{
		"GET /api/timesheets/active": respondJSON(`[{"id":5}]`),
	})

	if err := StopCurrentKimaiActivities(); err == nil {
		t.Fatal("expected an error when the stop request fails")
	}
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

			record, err := startKimaiActivity(1, 2)
			if err != nil {
				t.Fatal(err)
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
