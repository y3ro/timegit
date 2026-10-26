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
)

var (
	config     Config
	httpClient = &http.Client{Timeout: httpTimeout}
)

type Config struct {
	KimaiUrl      string
	KimaiApiToken string `json:",omitempty"`
	KimaiUsername string `json:",omitempty"`
	KimaiPassword string `json:",omitempty"`
	HourlyRate    float64
	ProjectMap    map[string]int
}

type KimaiProject struct {
	Id   int
	Name string
}

type KimaiActivity struct {
	Id   int
	Name string
}

type KimaiRecord struct {
	Id int
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

func getNow() string {
	return time.Now().Format("2006-01-02T15:04:05")
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

func startKimaiActivity(projectId int, activityId int) (*KimaiActivity, error) {
	url := buildKimaiUrl(kimaiTimesheetsPath, nil)
	method := "POST"
	reqBody := map[string]interface{}{
		"begin":    getNow(),
		"project":  projectId,
		"activity": activityId,
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

func getProjectName() (string, error) {
	repoRoot, err := getRepoRoot()
	if err != nil {
		err = fmt.Errorf("Error getting project name: %w", err)
		return "", err
	}

	return filepath.Base(repoRoot), nil
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

func StopCurrentKimaiActivities() error {
	activeRecords, err := fetchKimaiActiveRecords()
	if err != nil {
		err = fmt.Errorf("Error fetching active records in stopCurrentKimaiActivities: %w", err)
		return err
	}

	if len(activeRecords) == 0 {
		fmt.Println("No active records to stop")
		return nil
	}

	var kimaiActivity *KimaiActivity
	for i := 0; i < len(activeRecords); i++ {
		activeRecord := activeRecords[i]
		kimaiActivity, err = stopKimaiRecord(activeRecord.Id)
		if err != nil {
			err = fmt.Errorf("Error stopping active record (%d) in stopCurrentKimaiActivities: %w", activeRecord.Id, err)
			return err
		}
		fmt.Println("Stopped active record", kimaiActivity.Id)
	}

	return nil
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

// resolveKimaiActivity returns the activity named after the branch or, if
// there is none (or HEAD is detached), the default activity of the project.
func resolveKimaiActivity(projectName string, branchName string, projectID int) (*KimaiActivity, error) {
	if branchName != "" {
		kimaiActivityPtr, err := fetchKimaiActivity(branchName, projectID)
		if err == nil {
			return kimaiActivityPtr, nil
		}
		var noActivityFoundError *NoActivityFoundError
		if !errors.As(err, &noActivityFoundError) {
			return nil, err
		}
	}

	return fetchProjectKimaiActivity(projectName, projectID)
}

func StartCurrentGitBranchKimaiActivity() error {
	projectName, err := getProjectName()
	if err != nil {
		return err
	}

	branchName, err := getCurrentGitBranch()
	if err != nil {
		return err
	}
	projectID, ok := config.ProjectMap[projectName]
	if !ok {
		return fmt.Errorf("project %q not found in the ProjectMap of the config file", projectName)
	}
	kimaiActivityPtr, err := resolveKimaiActivity(projectName, branchName, projectID)
	if err != nil {
		return err
	}

	startedActivity, errStart := startKimaiActivity(projectID, kimaiActivityPtr.Id)
	if errStart != nil {
		return errStart
	}

	fmt.Println("Started record", startedActivity.Id)
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
		KimaiUrl:      "https://timetracking.domain.com",
		KimaiApiToken: "api-token",
		HourlyRate:    100,
		ProjectMap: map[string]int{
			"project1": 1,
			"project2": 2,
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
	if len(config.ProjectMap) == 0 {
		return errors.New("no project id map specified in the config file")
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
	stopOpPtr := flags.Bool("stop", false, "Stop current activity")
	startOpPtr := flags.Bool("start", false, "Start task for the current branch")
	restartOpPtr := flags.Bool("restart", false, "Restart previous activity")
	listProjsOpPtr := flags.Bool("list-projs", false, "List available projects info")
	configPathPtr := flags.String("config", "", "Path to the configuration file")

	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	if *startOpPtr && *restartOpPtr {
		return errors.New("you cannot start and restart tasks at the same time")
	}
	if !*stopOpPtr && !*startOpPtr && !*restartOpPtr && !*listProjsOpPtr {
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
	if *stopOpPtr {
		err = StopCurrentKimaiActivities()
		if err != nil {
			return err
		}
	}
	if *startOpPtr {
		return StartCurrentGitBranchKimaiActivity()
	}
	if *restartOpPtr {
		return RestartLastKimaiRecord()
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
