package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	kimaiActivitiesPath = "/api/activities"
	kimaiTimesheetsPath = "/api/timesheets"
	kimaiActivePath     = "/api/timesheets/active"
	kimaiRecentPath     = "/api/timesheets/recent"
	kimaiProjectsPath   = "/api/projects"
	configFileName      = "timegit.json"
	httpTimeout         = 15 * time.Second
	maxErrorBodyLength  = 500

	kimaiTimeFormat       = "2006-01-02T15:04:05"
	stateFileName         = "timegit-state.json"
	defaultMaxRecordHours = 8
	defaultIdleMinutes    = 30
	gitProjectConfigKey   = "timegit.project"
	postCheckoutHookName  = "post-checkout"
	postCheckoutHookMark  = "# Installed by timegit"
)

// postCheckoutHookBody switches the Kimai record only on branch checkouts
// ($3 is 0 for file checkouts) that leave a branch checked out, so rebases,
// bisects and tag checkouts (detached HEAD) are ignored.
const postCheckoutHookBody = `[ "$3" = "1" ] || exit 0
git symbolic-ref -q HEAD >/dev/null || exit 0
timegit -stop -all -start
`

// kimaiTimeLayouts are the formats in which Kimai may return dates
var kimaiTimeLayouts = []string{"2006-01-02T15:04:05-0700", time.RFC3339, kimaiTimeFormat}

var (
	config     Config
	httpClient = &http.Client{Timeout: httpTimeout}
)

type Config struct {
	KimaiUrl       string
	KimaiApiToken  string `json:",omitempty"`
	KimaiUsername  string `json:",omitempty"`
	KimaiPassword  string `json:",omitempty"`
	HourlyRate     float64
	MaxRecordHours float64 `json:",omitempty"`
	IdleMinutes    int     `json:",omitempty"`
	ProjectMap     map[string]int
	ActivityRules  []ActivityRule `json:",omitempty"`
}

// ActivityRule maps the branches matching a glob pattern, where * matches any
// sequence of characters (including /) and ? any single character, to a Kimai
// activity.
type ActivityRule struct {
	Branch   string
	Activity string
}

type KimaiProject struct {
	Id   int
	Name string
}

type KimaiActivity struct {
	Id   int
	Name string
}

// KimaiRef is a reference to a Kimai entity, which the API returns either as
// a bare id or as an expanded object depending on the endpoint.
type KimaiRef struct {
	Id   int
	Name string
}

func (r *KimaiRef) UnmarshalJSON(data []byte) error {
	var id int
	if err := json.Unmarshal(data, &id); err == nil {
		r.Id, r.Name = id, ""
		return nil
	}

	var entity struct {
		Id   int
		Name string
	}
	if err := json.Unmarshal(data, &entity); err != nil {
		return err
	}
	r.Id, r.Name = entity.Id, entity.Name

	return nil
}

type KimaiRecord struct {
	Id          int
	Begin       string
	Description string
	Project     KimaiRef
	Activity    KimaiRef
}

// recordTarget is the record that should be running for the current Git
// repository and branch.
type recordTarget struct {
	ProjectID   int
	ActivityID  int
	Description string
	RepoRoot    string
}

// State is what timegit remembers between runs
type State struct {
	// RepoPaths maps Kimai project ids to the repository their last record
	// was started from, so -stop-idle can check them from anywhere.
	RepoPaths map[int]string
}

func (t *recordTarget) matches(record KimaiRecord) bool {
	return t != nil &&
		record.Project.Id == t.ProjectID &&
		record.Activity.Id == t.ActivityID &&
		record.Description == t.Description
}

type NoActivityFoundError struct {
	msg string
}

func (e *NoActivityFoundError) Error() string {
	return fmt.Sprintf("[%s] activity not found", e.msg)
}

func getConfigDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		err = fmt.Errorf("Error getting the home directory in getConfigDir: %w", err)
		return "", err
	}

	return filepath.Join(homeDir, ".config"), nil
}

func formatKimaiTime(t time.Time) string {
	return t.Local().Format(kimaiTimeFormat)
}

func getNow() string {
	return formatKimaiTime(time.Now())
}

func getStatePath() (string, error) {
	configDir, err := getConfigDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(configDir, stateFileName), nil
}

func readState() (State, error) {
	state := State{RepoPaths: map[int]string{}}
	statePath, err := getStatePath()
	if err != nil {
		return state, err
	}

	stateBytes, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		err = fmt.Errorf("Error reading the state file in readState: %w", err)
		return state, err
	}

	err = json.Unmarshal(stateBytes, &state)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling the state file %s in readState: %w", statePath, err)
		return state, err
	}
	if state.RepoPaths == nil {
		state.RepoPaths = map[int]string{}
	}

	return state, nil
}

func saveRepoPath(projectID int, repoRoot string) error {
	state, err := readState()
	if err != nil {
		return err
	}
	if state.RepoPaths[projectID] == repoRoot {
		return nil
	}
	state.RepoPaths[projectID] = repoRoot

	statePath, err := getStatePath()
	if err != nil {
		return err
	}
	stateBytes, err := json.MarshalIndent(state, "", "    ")
	if err != nil {
		return err
	}
	err = os.MkdirAll(filepath.Dir(statePath), 0o755)
	if err != nil {
		return err
	}

	return os.WriteFile(statePath, stateBytes, 0o600)
}

func buildKimaiUrl(path string, query url.Values) string {
	kimaiUrl := strings.TrimRight(config.KimaiUrl, "/") + path
	if len(query) > 0 {
		kimaiUrl += "?" + query.Encode()
	}

	return kimaiUrl
}

func truncate(s string, maxLength int) string {
	if len(s) <= maxLength {
		return s
	}

	return s[:maxLength] + "..."
}

func fetchKimaiResource(url string, method string, body io.Reader) ([]byte, error) {
	httpReq, err := http.NewRequest(method, url, body)
	if err != nil {
		err = fmt.Errorf("Error creating the request in fetchKimaiResource: %w", err)
		return nil, err
	}

	if config.KimaiApiToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+config.KimaiApiToken)
	} else {
		httpReq.Header.Set("X-AUTH-USER", config.KimaiUsername)
		httpReq.Header.Set("X-AUTH-TOKEN", config.KimaiPassword)
	}
	httpReq.Header.Set("Accept", "application/json")

	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		err = fmt.Errorf("Error performing the request in fetchKimaiResource: %w", err)
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("Error reading the response in fetchKimaiResource: %w", err)
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyStr := truncate(strings.TrimSpace(string(respBody)), maxErrorBodyLength)
		err = fmt.Errorf("Kimai responded %s to %s %s: %s", resp.Status, method, url, bodyStr)
		return nil, err
	}

	return respBody, nil
}

func fetchKimaiActivity(name string, projectID int) (*KimaiActivity, error) {
	if name == "" || projectID == 0 {
		return nil, errors.New("empty name or invalid project id")
	}

	query := url.Values{}
	query.Set("term", name)
	query.Set("project", strconv.Itoa(projectID))
	url := buildKimaiUrl(kimaiActivitiesPath, query)
	method := "GET"

	respBody, err := fetchKimaiResource(url, method, nil)
	if err != nil {
		err = fmt.Errorf("Error fetching in fetchKimaiActivity: %w", err)
		return nil, err
	}

	var kimaiActivities []KimaiActivity
	err = json.Unmarshal(respBody, &kimaiActivities)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in fetchKimaiActivity: %w", err)
		return nil, err
	}

	// The term filter matches substrings, so keep only exact name matches
	var matchingActivities []KimaiActivity
	for _, kimaiActivity := range kimaiActivities {
		if kimaiActivity.Id > 0 && strings.EqualFold(kimaiActivity.Name, name) {
			matchingActivities = append(matchingActivities, kimaiActivity)
		}
	}

	if len(matchingActivities) == 0 {
		msg := fmt.Sprintf("name: %s, projectID: %d", name, projectID)
		return nil, &NoActivityFoundError{msg: msg}
	}

	if len(matchingActivities) > 1 {
		return nil, fmt.Errorf("multiple activities named %q found for project %d", name, projectID)
	}

	return &matchingActivities[0], nil
}

func startKimaiActivity(projectId int, activityId int, description string) (*KimaiActivity, error) {
	url := buildKimaiUrl(kimaiTimesheetsPath, nil)
	method := "POST"
	reqBody := map[string]interface{}{
		"begin":    getNow(),
		"project":  projectId,
		"activity": activityId,
	}
	if description != "" {
		reqBody["description"] = description
	}
	if config.HourlyRate > 0 {
		reqBody["hourlyRate"] = config.HourlyRate
	}
	reqBodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		err = fmt.Errorf("Error marshalling in startKimaiActivity: %w", err)
		return nil, err
	}
	bodyReader := bytes.NewReader(reqBodyBytes)

	respBody, err := fetchKimaiResource(url, method, bodyReader)
	if err != nil {
		err = fmt.Errorf("Error fetching in startKimaiActivity: %w", err)
		return nil, err
	}

	var startedKimaiActivity KimaiActivity
	err = json.Unmarshal(respBody, &startedKimaiActivity)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in startKimaiActivity: %w", err)
		return nil, err
	}

	if startedKimaiActivity.Id == 0 {
		return nil, errors.New("no activity started")
	}

	return &startedKimaiActivity, nil
}

func filterValidRecords(records []KimaiRecord) []KimaiRecord {
	validRecords := make([]KimaiRecord, 0, len(records))

	for i := 0; i < len(records); i++ {
		if records[i].Id > 0 {
			validRecords = append(validRecords, records[i])
		}
	}

	return validRecords
}

func fetchKimaiActiveRecords() ([]KimaiRecord, error) {
	url := buildKimaiUrl(kimaiActivePath, nil)
	method := "GET"

	respBody, err := fetchKimaiResource(url, method, nil)
	if err != nil {
		err = fmt.Errorf("Error fetching in fetchKimaiActiveRecords: %w", err)
		return nil, err
	}

	var activeRecords []KimaiRecord
	err = json.Unmarshal(respBody, &activeRecords)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in fetchKimaiActiveRecords: %w", err)
		return nil, err
	}

	return filterValidRecords(activeRecords), nil
}

func buildStopActivityPath(activityID int) string {
	return fmt.Sprintf("%s/%d/stop", kimaiTimesheetsPath, activityID)
}

func stopKimaiRecord(activityID int) (*KimaiActivity, error) {
	url := buildKimaiUrl(buildStopActivityPath(activityID), nil)
	method := "PATCH"

	respBody, err := fetchKimaiResource(url, method, nil)
	if err != nil {
		err = fmt.Errorf("Error fetching in stopKimaiRecord: %w", err)
		return nil, err
	}

	var stoppedActivity KimaiActivity
	err = json.Unmarshal(respBody, &stoppedActivity)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in stopKimaiRecord: %w", err)
		return nil, err
	}

	if stoppedActivity.Id == 0 {
		return nil, errors.New("no stopped activity")
	}

	return &stoppedActivity, nil
}

func getRepoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.CombinedOutput()
	outputStr := strings.TrimSpace(string(output))
	if err != nil {
		err = fmt.Errorf("Error getting the repository root: %s [%w]", outputStr, err)
		return "", err
	}

	return outputStr, nil
}

// getGitConfig returns the value of a Git config key, or "" if it is unset
func getGitConfig(key string) (string, error) {
	cmd := exec.Command("git", "config", "--get", key)
	output, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		err = fmt.Errorf("Error reading the git config key %s: %w", key, err)
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
}

// getProjectName returns the timegit.project Git config value or, if unset,
// the name of the repository folder.
func getProjectName() (string, error) {
	repoRoot, err := getRepoRoot()
	if err != nil {
		err = fmt.Errorf("Error getting project name: %w", err)
		return "", err
	}

	projectName, err := getGitConfig(gitProjectConfigKey)
	if err != nil {
		err = fmt.Errorf("Error getting project name: %w", err)
		return "", err
	}
	if projectName != "" {
		return projectName, nil
	}

	return filepath.Base(repoRoot), nil
}

func getCurrentProject() (string, int, error) {
	projectName, err := getProjectName()
	if err != nil {
		return "", 0, err
	}

	projectID, ok := config.ProjectMap[projectName]
	if !ok {
		return "", 0, fmt.Errorf("project %q not found in the ProjectMap of the config file", projectName)
	}

	return projectName, projectID, nil
}

func getCurrentGitBranch() (string, error) {
	cmd := exec.Command("git", "branch", "--show-current")
	output, err := cmd.Output()
	outputStr := string(output)
	if err != nil {
		err = fmt.Errorf("Error getting current git branch: %s [%w]", outputStr, err)
		return "", err
	}
	return strings.TrimSpace(outputStr), nil
}

// getStopScopeProjectID returns the id of the project whose records -stop
// affects, or 0 for every project when all is set or outside a Git repository.
func getStopScopeProjectID(all bool) (int, error) {
	if all {
		return 0, nil
	}
	if _, err := getRepoRoot(); err != nil {
		return 0, nil
	}

	_, projectID, err := getCurrentProject()
	if err != nil {
		return 0, fmt.Errorf("%w (use -all to stop the records of every project)", err)
	}

	return projectID, nil
}

// stopKimaiRecords stops the active records of the project projectID (of
// every project if it is 0), except the one matching keep, if any, and
// returns the records left running.
func stopKimaiRecords(activeRecords []KimaiRecord, projectID int, keep *recordTarget) ([]KimaiRecord, error) {
	var runningRecords []KimaiRecord
	stoppedCount := 0
	now := time.Now()

	for _, activeRecord := range activeRecords {
		if (projectID != 0 && activeRecord.Project.Id != projectID) || keep.matches(activeRecord) {
			runningRecords = append(runningRecords, activeRecord)
			continue
		}

		stoppedRecord, err := stopKimaiRecord(activeRecord.Id)
		if err != nil {
			err = fmt.Errorf("Error stopping active record (%d) in stopKimaiRecords: %w", activeRecord.Id, err)
			return nil, err
		}
		stoppedCount++

		msg := fmt.Sprintf("Stopped record %d", stoppedRecord.Id)
		if duration, ok := recordDuration(activeRecord, now); ok {
			msg += " after " + formatDuration(duration)
		}
		fmt.Println(msg)
		warnIfTooLong(activeRecord, now)
	}

	if stoppedCount == 0 {
		fmt.Println("No active records to stop")
	}

	return runningRecords, nil
}

func createDefaultProjectKimaiActivity(projectName string, projectID int) (*KimaiActivity, error) {
	url := buildKimaiUrl(kimaiActivitiesPath, nil)
	method := "POST"
	reqBody := map[string]interface{}{
		"name":     projectName,
		"project":  projectID,
		"visible":  true,
		"billable": true,
	}
	reqBodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		err = fmt.Errorf("Error marshalling in createDefaultProjectKimaiActivity: %w", err)
		return nil, err
	}
	bodyReader := bytes.NewReader(reqBodyBytes)

	respBody, err := fetchKimaiResource(url, method, bodyReader)
	if err != nil {
		err = fmt.Errorf("Error fetching in createDefaultProjectKimaiActivity: %w", err)
		return nil, err
	}

	var createdKimaiActivity KimaiActivity
	err = json.Unmarshal(respBody, &createdKimaiActivity)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in createDefaultProjectKimaiActivity: %w", err)
		return nil, err
	}

	if createdKimaiActivity.Id == 0 {
		return nil, errors.New("error creating the default project activity for " + projectName)
	}

	return &createdKimaiActivity, nil
}

func fetchProjectKimaiActivity(projectName string, projectID int) (*KimaiActivity, error) {
	projKimaiActivityPtr, err := fetchKimaiActivity(projectName, projectID)
	var noActivityFoundError *NoActivityFoundError
	if errors.As(err, &noActivityFoundError) {
		return createDefaultProjectKimaiActivity(projectName, projectID)
	}
	if err != nil {
		return nil, err
	}

	return projKimaiActivityPtr, nil
}

func globMatch(pattern string, s string) bool {
	var re strings.Builder
	re.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			re.WriteString(".*")
		case '?':
			re.WriteString(".")
		default:
			re.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	re.WriteString("$")

	return regexp.MustCompile(re.String()).MatchString(s)
}

// matchActivityRule returns the activity of the first rule whose branch
// pattern matches branchName.
func matchActivityRule(branchName string) (string, bool) {
	for _, rule := range config.ActivityRules {
		if globMatch(rule.Branch, branchName) {
			return rule.Activity, true
		}
	}

	return "", false
}

// resolveKimaiActivity returns, in order of preference, the activity of the
// first rule matching the branch, the activity named after the branch or the
// default activity of the project, which is also used when HEAD is detached.
func resolveKimaiActivity(projectName string, branchName string, projectID int) (*KimaiActivity, error) {
	if branchName == "" {
		return fetchProjectKimaiActivity(projectName, projectID)
	}

	var noActivityFoundError *NoActivityFoundError
	if ruleActivity, ok := matchActivityRule(branchName); ok {
		kimaiActivityPtr, err := fetchKimaiActivity(ruleActivity, projectID)
		if err == nil {
			return kimaiActivityPtr, nil
		}
		if !errors.As(err, &noActivityFoundError) {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "Warning: activity %q of the rule matching branch %q not found in Kimai\n", ruleActivity, branchName)
	}

	kimaiActivityPtr, err := fetchKimaiActivity(branchName, projectID)
	if err == nil {
		return kimaiActivityPtr, nil
	}
	if !errors.As(err, &noActivityFoundError) {
		return nil, err
	}

	return fetchProjectKimaiActivity(projectName, projectID)
}

func resolveRecordTarget() (*recordTarget, error) {
	repoRoot, err := getRepoRoot()
	if err != nil {
		return nil, err
	}

	projectName, projectID, err := getCurrentProject()
	if err != nil {
		return nil, err
	}

	branchName, err := getCurrentGitBranch()
	if err != nil {
		return nil, err
	}

	kimaiActivityPtr, err := resolveKimaiActivity(projectName, branchName, projectID)
	if err != nil {
		return nil, err
	}

	return &recordTarget{ProjectID: projectID, ActivityID: kimaiActivityPtr.Id, Description: branchName, RepoRoot: repoRoot}, nil
}

// rememberRepo saves the repository of the target for -stop-idle. Failing to
// do so only disables idle detection, so it is just reported.
func rememberRepo(target *recordTarget) {
	if target.RepoRoot == "" {
		return
	}
	if err := saveRepoPath(target.ProjectID, target.RepoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save the repository path for -stop-idle: %s\n", err)
	}
}

// startRecordTarget starts a record for target unless one of the active
// records already matches it, so checking out the same branch again does not
// split the record.
func startRecordTarget(target *recordTarget, activeRecords []KimaiRecord) error {
	for _, activeRecord := range activeRecords {
		if target.matches(activeRecord) {
			fmt.Println("Already tracking record", activeRecord.Id)
			rememberRepo(target)
			return nil
		}
	}

	startedActivity, err := startKimaiActivity(target.ProjectID, target.ActivityID, target.Description)
	if err != nil {
		return err
	}

	fmt.Println("Started record", startedActivity.Id)
	rememberRepo(target)
	return nil
}

// runRecordOps stops and/or starts records. When both are requested, the
// record of the current branch is left running if it is already active.
func runRecordOps(stop bool, stopAll bool, start bool) error {
	var (
		target    *recordTarget
		projectID int
		err       error
	)

	if start {
		target, err = resolveRecordTarget()
		if err != nil {
			return err
		}
	}

	activeRecords, err := fetchKimaiActiveRecords()
	if err != nil {
		err = fmt.Errorf("Error fetching active records in runRecordOps: %w", err)
		return err
	}

	if stop {
		projectID, err = getStopScopeProjectID(stopAll)
		if err != nil {
			return err
		}
		activeRecords, err = stopKimaiRecords(activeRecords, projectID, target)
		if err != nil {
			return err
		}
	}

	warnLongRecords(activeRecords)

	if start {
		return startRecordTarget(target, activeRecords)
	}

	return nil
}

func parseKimaiTime(value string) (time.Time, error) {
	for _, layout := range kimaiTimeLayouts {
		parsed, err := time.ParseInLocation(layout, value, time.Local)
		if err == nil {
			return parsed, nil
		}
	}

	return time.Time{}, fmt.Errorf("unknown Kimai time format %q", value)
}

func recordDuration(record KimaiRecord, now time.Time) (time.Duration, bool) {
	begin, err := parseKimaiTime(record.Begin)
	if err != nil || begin.After(now) {
		return 0, false
	}

	return now.Sub(begin), true
}

func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Minute)
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func getMaxRecordDuration() time.Duration {
	hours := config.MaxRecordHours
	if hours == 0 {
		hours = defaultMaxRecordHours
	}

	return time.Duration(hours * float64(time.Hour))
}

// longRecordWarning returns a warning if the record lasts longer than
// MaxRecordHours, which usually means it was not stopped on time.
func longRecordWarning(record KimaiRecord, now time.Time) string {
	duration, ok := recordDuration(record, now)
	maxDuration := getMaxRecordDuration()
	if !ok || duration <= maxDuration {
		return ""
	}

	return fmt.Sprintf("Warning: record %d is %s long, over the %s limit; check its times in Kimai",
		record.Id, formatDuration(duration), formatDuration(maxDuration))
}

func warnIfTooLong(record KimaiRecord, now time.Time) {
	if warning := longRecordWarning(record, now); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
}

func warnLongRecords(records []KimaiRecord) {
	now := time.Now()
	for _, record := range records {
		warnIfTooLong(record, now)
	}
}

func refLabel(ref KimaiRef, kind string) string {
	if ref.Name != "" {
		return ref.Name
	}

	return fmt.Sprintf("%s %d", kind, ref.Id)
}

func formatRecord(record KimaiRecord, now time.Time) string {
	line := fmt.Sprintf("Record %d: %s / %s", record.Id, refLabel(record.Project, "project"), refLabel(record.Activity, "activity"))
	if record.Description != "" {
		line += fmt.Sprintf(" (%s)", record.Description)
	}
	if duration, ok := recordDuration(record, now); ok {
		line += ", running for " + formatDuration(duration)
	}

	return line
}

func ShowKimaiStatus() error {
	activeRecords, err := fetchKimaiActiveRecords()
	if err != nil {
		return err
	}

	if len(activeRecords) == 0 {
		fmt.Println("No active records")
		return nil
	}

	now := time.Now()
	for _, record := range activeRecords {
		fmt.Println(formatRecord(record, now))
	}
	warnLongRecords(activeRecords)

	return nil
}

func getIdleDuration() time.Duration {
	minutes := config.IdleMinutes
	if minutes == 0 {
		minutes = defaultIdleMinutes
	}

	return time.Duration(minutes) * time.Minute
}

// getLastReflogTime returns when HEAD last moved (commits, checkouts,
// rebases, resets...), if the repository has any reflog entry.
func getLastReflogTime(repoRoot string) (time.Time, bool) {
	cmd := exec.Command("git", "log", "-g", "-1", "--date=unix", "--format=%gd", "HEAD")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return time.Time{}, false
	}

	// The selector looks like HEAD@{1761470000}
	selector := strings.TrimSpace(string(output))
	start := strings.LastIndex(selector, "{")
	end := strings.LastIndex(selector, "}")
	if start < 0 || end <= start {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(selector[start+1:end], 10, 64)
	if err != nil {
		return time.Time{}, false
	}

	return time.Unix(seconds, 0), true
}

// getLastChangeTime returns the latest modification time of the changed and
// untracked (but not ignored) files of the repository.
func getLastChangeTime(repoRoot string) (time.Time, error) {
	cmd := exec.Command("git", "status", "--porcelain", "-z", "--untracked-files=all")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		err = fmt.Errorf("Error getting the changed files of %s: %w", repoRoot, err)
		return time.Time{}, err
	}

	var lastChange time.Time
	entries := strings.Split(string(output), "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		// Renames and copies are followed by an entry with the original path
		if strings.ContainsAny(entry[:2], "RC") {
			i++
		}

		fileInfo, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(entry[3:])))
		if err == nil && fileInfo.ModTime().After(lastChange) {
			lastChange = fileInfo.ModTime()
		}
	}

	return lastChange, nil
}

// getLastRepoActivity returns the latest time HEAD moved or a changed file
// was modified in the repository, or the zero time if there is none.
func getLastRepoActivity(repoRoot string) (time.Time, error) {
	if _, err := os.Stat(repoRoot); err != nil {
		return time.Time{}, err
	}

	lastActivity, err := getLastChangeTime(repoRoot)
	if err != nil {
		return time.Time{}, err
	}
	if reflogTime, ok := getLastReflogTime(repoRoot); ok && reflogTime.After(lastActivity) {
		lastActivity = reflogTime
	}

	return lastActivity, nil
}

// getKnownRepoPaths returns the repositories remembered by -start, plus the
// current one, by Kimai project id.
func getKnownRepoPaths() (map[int]string, error) {
	state, err := readState()
	if err != nil {
		return nil, err
	}

	if repoRoot, err := getRepoRoot(); err == nil {
		if _, projectID, err := getCurrentProject(); err == nil {
			state.RepoPaths[projectID] = repoRoot
		}
	}

	return state.RepoPaths, nil
}

func buildRecordPath(recordID int) string {
	return fmt.Sprintf("%s/%d", kimaiTimesheetsPath, recordID)
}

// endKimaiRecordAt stops a record setting its end time, instead of the
// current time used by the stop endpoint.
func endKimaiRecordAt(recordID int, end time.Time) (*KimaiRecord, error) {
	url := buildKimaiUrl(buildRecordPath(recordID), nil)
	method := "PATCH"
	reqBodyBytes, err := json.Marshal(map[string]interface{}{"end": formatKimaiTime(end)})
	if err != nil {
		err = fmt.Errorf("Error marshalling in endKimaiRecordAt: %w", err)
		return nil, err
	}

	respBody, err := fetchKimaiResource(url, method, bytes.NewReader(reqBodyBytes))
	if err != nil {
		err = fmt.Errorf("Error fetching in endKimaiRecordAt: %w", err)
		return nil, err
	}

	var endedRecord KimaiRecord
	err = json.Unmarshal(respBody, &endedRecord)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in endKimaiRecordAt: %w", err)
		return nil, err
	}

	if endedRecord.Id == 0 {
		return nil, errors.New("no ended record")
	}

	return &endedRecord, nil
}

// stopIdleRecord stops the record, at the time of the last activity, if its
// repository has had no activity for IdleMinutes.
func stopIdleRecord(record KimaiRecord, repoRoot string, now time.Time) error {
	lastActivity, err := getLastRepoActivity(repoRoot)
	if err != nil {
		return fmt.Errorf("Error checking the activity of record %d: %w", record.Id, err)
	}
	// A record restarted or started by hand counts as activity
	if begin, err := parseKimaiTime(record.Begin); err == nil && begin.After(lastActivity) {
		lastActivity = begin
	}
	if lastActivity.IsZero() {
		fmt.Printf("Skipping record %d: no activity found in %s\n", record.Id, repoRoot)
		return nil
	}
	if lastActivity.After(now) {
		lastActivity = now
	}

	idle := now.Sub(lastActivity)
	if idle < getIdleDuration() {
		fmt.Printf("Record %d is active: last activity in %s %s ago\n", record.Id, repoRoot, formatDuration(idle))
		return nil
	}

	_, err = endKimaiRecordAt(record.Id, lastActivity)
	if err != nil {
		return fmt.Errorf("Error stopping idle record %d: %w", record.Id, err)
	}
	fmt.Printf("Stopped record %d at %s, after %s without activity in %s\n",
		record.Id, lastActivity.Local().Format("2006-01-02 15:04"), formatDuration(idle), repoRoot)

	return nil
}

// StopIdleKimaiRecords stops the active records whose repository has had no
// activity for IdleMinutes, setting their end to the last activity.
func StopIdleKimaiRecords() error {
	activeRecords, err := fetchKimaiActiveRecords()
	if err != nil {
		return err
	}
	if len(activeRecords) == 0 {
		fmt.Println("No active records")
		return nil
	}

	repoPaths, err := getKnownRepoPaths()
	if err != nil {
		return err
	}

	var errs []error
	now := time.Now()
	for _, record := range activeRecords {
		repoRoot, ok := repoPaths[record.Project.Id]
		if !ok {
			fmt.Printf("Skipping record %d: no known repository for %s\n", record.Id, refLabel(record.Project, "project"))
			continue
		}
		if err := stopIdleRecord(record, repoRoot, now); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func getHooksDir() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-path", "hooks")
	output, err := cmd.CombinedOutput()
	outputStr := strings.TrimSpace(string(output))
	if err != nil {
		err = fmt.Errorf("Error getting the Git hooks directory: %s [%w]", outputStr, err)
		return "", err
	}

	return outputStr, nil
}

// InstallGitHook installs a post-checkout hook running timegit on branch
// checkouts, refusing to overwrite hooks it did not install.
func InstallGitHook() error {
	hooksDir, err := getHooksDir()
	if err != nil {
		return err
	}
	hookPath := filepath.Join(hooksDir, postCheckoutHookName)

	existingHook, err := os.ReadFile(hookPath)
	if err == nil {
		if strings.Contains(string(existingHook), postCheckoutHookMark) {
			fmt.Println("timegit hook already installed in", hookPath)
			return nil
		}
		return fmt.Errorf("a %s hook already exists in %s; add these lines to it:\n\n%s",
			postCheckoutHookName, hookPath, postCheckoutHookBody)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	err = os.MkdirAll(hooksDir, 0o755)
	if err != nil {
		return err
	}
	hookContent := "#!/bin/sh\n" + postCheckoutHookMark + "\n" + postCheckoutHookBody
	err = os.WriteFile(hookPath, []byte(hookContent), 0o755)
	if err != nil {
		return err
	}

	fmt.Println("Installed timegit hook in", hookPath)
	return nil
}

func fetchLastKimaiRecord() (*KimaiRecord, error) {
	query := url.Values{}
	query.Set("size", "1")
	url := buildKimaiUrl(kimaiRecentPath, query)
	method := "GET"

	respBody, err := fetchKimaiResource(url, method, nil)
	if err != nil {
		err = fmt.Errorf("Error fetching in fetchLastKimaiRecord: %w", err)
		return nil, err
	}

	var recentRecords []KimaiRecord
	err = json.Unmarshal(respBody, &recentRecords)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in fetchLastKimaiRecord: %w", err)
		return nil, err
	}

	validRecentRecords := filterValidRecords(recentRecords)
	if len(validRecentRecords) == 0 {
		return nil, errors.New("no recent records retrieved")
	}

	return &validRecentRecords[0], nil
}

func buildRestartRecordPath(recordID int) string {
	return fmt.Sprintf("%s/%d/restart", kimaiTimesheetsPath, recordID)
}

func restartKimaiRecord(recordID int) (*KimaiRecord, error) {
	url := buildKimaiUrl(buildRestartRecordPath(recordID), nil)
	method := "PATCH"

	respBody, err := fetchKimaiResource(url, method, nil)
	if err != nil {
		err = fmt.Errorf("Error fetching in restartKimaiRecord: %w", err)
		return nil, err
	}

	var restartedRecord KimaiRecord
	err = json.Unmarshal(respBody, &restartedRecord)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in restartKimaiRecord: %w", err)
		return nil, err
	}

	if restartedRecord.Id == 0 {
		return nil, errors.New("no restarted record")
	}

	return &restartedRecord, nil
}

func RestartLastKimaiRecord() error {
	lastRecord, err := fetchLastKimaiRecord()
	if err != nil {
		return err
	}

	restartedRecord, errRestart := restartKimaiRecord(lastRecord.Id)
	if errRestart != nil {
		return errRestart
	}

	fmt.Println("Restarted record", restartedRecord.Id)
	return nil
}

func fetchKimaiProjects() ([]KimaiProject, error) {
	url := buildKimaiUrl(kimaiProjectsPath, nil)
	method := "GET"

	respBody, err := fetchKimaiResource(url, method, nil)
	if err != nil {
		err = fmt.Errorf("Error fetching in fetchKimaiProjects: %w", err)
		return nil, err
	}

	var kimaiProjects []KimaiProject
	err = json.Unmarshal(respBody, &kimaiProjects)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in fetchKimaiProjects: %w", err)
		return nil, err
	}

	return kimaiProjects, nil
}

func ListKimaiProjects() error {
	kimaiProjects, err := fetchKimaiProjects()
	if err != nil {
		return err
	}

	var entries []string
	for i := 0; i < len(kimaiProjects); i++ {
		proj := kimaiProjects[i]
		entries = append(entries, "\""+proj.Name+"\""+": "+strconv.Itoa(proj.Id))
	}
	fmt.Println(strings.Join(entries, ",\n"))

	return nil
}

func configFileHelp() string {
	helpConfig := Config{
		KimaiUrl:       "https://timetracking.domain.com",
		KimaiApiToken:  "api-token",
		HourlyRate:     100,
		MaxRecordHours: defaultMaxRecordHours,
		IdleMinutes:    defaultIdleMinutes,
		ProjectMap: map[string]int{
			"project1": 1,
			"project2": 2,
		},
		ActivityRules: []ActivityRule{
			{Branch: "feature/*", Activity: "Development"},
			{Branch: "fix/*", Activity: "Bugfixing"},
		},
	}

	helpBytes, _ := json.MarshalIndent(helpConfig, "", "    ")
	return string(helpBytes)
}

func checkConfigFields(config *Config) error {
	if config.KimaiUrl == "" {
		return errors.New("no Kimai URL specified in the config file")
	}
	if config.KimaiApiToken == "" && (config.KimaiUsername == "" || config.KimaiPassword == "") {
		return errors.New("no Kimai API token (or username and password) specified in the config file")
	}
	if config.HourlyRate < 0 {
		return errors.New("negative hourly rate specified in the config file")
	}
	if config.MaxRecordHours < 0 {
		return errors.New("negative maximum record hours specified in the config file")
	}
	if config.IdleMinutes < 0 {
		return errors.New("negative idle minutes specified in the config file")
	}
	if len(config.ProjectMap) == 0 {
		return errors.New("no project id map specified in the config file")
	}
	for _, rule := range config.ActivityRules {
		if rule.Branch == "" || rule.Activity == "" {
			return errors.New("every activity rule in the config file needs a Branch pattern and an Activity")
		}
	}

	return nil
}

func openDefaultConfigFile() (*os.File, error) {
	repoRoot, err := getRepoRoot()
	if err == nil {
		configFile, err := os.Open(filepath.Join(repoRoot, configFileName))
		if err == nil {
			return configFile, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	configDir, err := getConfigDir()
	if err != nil {
		return nil, err
	}

	return os.Open(filepath.Join(configDir, configFileName))
}

func readConfig(configPath string) error {
	var (
		configFile *os.File
		err        error
	)

	if len(configPath) == 0 {
		configFile, err = openDefaultConfigFile()
	} else {
		configFile, err = os.Open(configPath)
	}

	if err != nil {
		helpMsg := configFileHelp()
		err = fmt.Errorf("%w\n\nExample configuration:\n\n%s", err, helpMsg)
		return err
	}
	defer configFile.Close()

	configBytes, err := io.ReadAll(configFile)
	if err != nil {
		err = fmt.Errorf("Error reading config file in readConfig: %w", err)
		return err
	}

	config = Config{}
	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in readConfig: %w", err)
		return err
	}

	return checkConfigFields(&config)
}

func parseCliArgsAndRun(args []string) error {
	flags := flag.NewFlagSet("timegit", flag.ContinueOnError)
	stopOpPtr := flags.Bool("stop", false, "Stop the active records of the current project")
	allOpPtr := flags.Bool("all", false, "With -stop, stop the active records of every project")
	startOpPtr := flags.Bool("start", false, "Start a record for the current branch, unless it is already active")
	restartOpPtr := flags.Bool("restart", false, "Restart previous activity")
	statusOpPtr := flags.Bool("status", false, "Show the active records")
	stopIdleOpPtr := flags.Bool("stop-idle", false, "Stop the records whose repository has had no activity for IdleMinutes")
	listProjsOpPtr := flags.Bool("list-projs", false, "List available projects info")
	installHookOpPtr := flags.Bool("install-hook", false, "Install a post-checkout Git hook tracking the checked out branch")
	configPathPtr := flags.String("config", "", "Path to the configuration file")

	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	kimaiOp := *stopOpPtr || *startOpPtr || *restartOpPtr || *statusOpPtr || *stopIdleOpPtr || *listProjsOpPtr
	if *startOpPtr && *restartOpPtr {
		return errors.New("you cannot start and restart tasks at the same time")
	}
	if *stopIdleOpPtr && (*stopOpPtr || *startOpPtr || *restartOpPtr) {
		return errors.New("-stop-idle cannot be combined with -stop, -start or -restart")
	}
	if *allOpPtr && !*stopOpPtr {
		return errors.New("-all can only be used with -stop")
	}
	if *installHookOpPtr && kimaiOp {
		return errors.New("-install-hook cannot be combined with other operations")
	}
	if *installHookOpPtr {
		return InstallGitHook()
	}
	if !kimaiOp {
		flags.Usage()
		return nil
	}

	err = readConfig(*configPathPtr)
	if err != nil {
		return err
	}

	if *listProjsOpPtr {
		return ListKimaiProjects()
	}
	if *stopOpPtr || *startOpPtr {
		err = runRecordOps(*stopOpPtr, *allOpPtr, *startOpPtr)
		if err != nil {
			return err
		}
	}
	if *restartOpPtr {
		err = RestartLastKimaiRecord()
		if err != nil {
			return err
		}
	}
	if *stopIdleOpPtr {
		err = StopIdleKimaiRecords()
		if err != nil {
			return err
		}
	}
	if *statusOpPtr {
		return ShowKimaiStatus()
	}

	return nil
}

func main() {
	opErr := parseCliArgsAndRun(os.Args[1:])
	if opErr != nil {
		fmt.Fprintf(os.Stderr, "%s\n", opErr)
		os.Exit(1)
	}
}
