package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// TODO: add tests
// TODO: if it does not exist, create config file template
// TODO: allow to search project by name
// TODO: option to list Kimai projects with their ids

const (
	kimaiTimesheetsPath = "/timesheets/active"
	kimaiRecentPath     = "/timesheets/recent"
	configFileName      = "gimai.json"
)

var (
	config    Config
)

type Config struct {
	KimaiUrl      string
	KimaiUsername string
	KimaiPassword string
	HourlyRate    int
	ProjectMap    map[string]int
}

type KimaiActivity struct {
	Id int
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

func getHomePath() string {
	var homePath string
	if runtime.GOOS == "windows" {
		homePath = "HOMEPATH"
	} else {
		homePath = "HOME"
	}

	return filepath.Join(os.Getenv(homePath), ".config")
}

func getNow() string {
	return time.Now().Format("2006-01-02T15:04:05")
}

func buildActivitiesPath(term string, projectID int) string {
	return fmt.Sprintf("/activities?term=%s&project=%d", term, projectID)
}

func fetchKimaiResource(url string, method string, body io.Reader) ([]byte, error) {
	client := &http.Client{}
	httpReq, err := http.NewRequest(method, url, body)
	if err != nil {
		err = fmt.Errorf("Error creating the request in fetchKimaiResource: %w", err)
		return nil, err
	}

	httpReq.Header.Set("X-AUTH-USER", config.KimaiUsername)
	httpReq.Header.Set("X-AUTH-TOKEN", config.KimaiPassword)

	resp, err := client.Do(httpReq)
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

	return respBody, nil
}

func fetchKimaiActivity(term string, projectID int) (*KimaiActivity, error) {
	if term == "" || projectID == 0 {
		return nil, errors.New("empty term or invalid project id")
	}

	url := config.KimaiUrl + buildActivitiesPath(term, projectID)
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

	if len(kimaiActivities) == 0 {
		msg := fmt.Sprintf("term: %s, projectID: %d", term, projectID)
		return nil, &NoActivityFoundError{msg: msg}
	}

	if len(kimaiActivities) > 1 {
		return nil, errors.New("multiple activities fetched")
	}

	kimaiActivity := kimaiActivities[0]
	if kimaiActivity.Id == 0 {
		msg := fmt.Sprintf("term: %s, projectID: %d, invalid", term, projectID)
		return nil, &NoActivityFoundError{msg: msg}
	}

	return &kimaiActivity, nil
}

func startKimaiActivity(projectId int, activityId int) (*KimaiActivity, error) {
	url := config.KimaiUrl + "/timesheets"
	method := "POST"
	reqBody := map[string]interface{}{
		"begin":      getNow(),
		"project":    projectId,
		"activity":   activityId,
		"hourlyRate": config.HourlyRate,
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
	validRecords := make([]KimaiRecord, len(records))

	for i := 0; i < len(records); i++ {
		if records[i].Id > 0 {
			validRecords = append(validRecords, records[i])
		}
	}

	return validRecords
}

func fetchKimaiActiveRecords() ([]KimaiRecord, error) {
	url := config.KimaiUrl + kimaiTimesheetsPath
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

	validActiveRecords := filterValidRecords(activeRecords)
	if len(validActiveRecords) == 0 {
		return nil, errors.New("no active records retrieved")
	}

	return activeRecords, nil
}

func buildStopActivityPath(activityID int) string {
	return fmt.Sprintf("/timesheets/%v/stop", activityID)
}

func stopKimaiRecord(activityID int) (*KimaiActivity, error) {
	url := config.KimaiUrl + buildStopActivityPath(activityID)
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

func getProjectName() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.CombinedOutput()
	outputStr := string(output)
	if err != nil {
		err = fmt.Errorf("Error getting project name: %s [%w]", outputStr, err)
		return "", err 
	}
	parts := strings.Split(strings.TrimSpace(outputStr), "/")
	projectName := parts[len(parts)-1]

	return projectName, nil
}

func getCurrentGitBranch() (string, error) {
	cmd := exec.Command("git", "branch", "--show-current")
	output, err := cmd.Output()
	outputStr := string(output)
	if err != nil {
		err = fmt.Errorf("Error getting current git branch: %s [%w]", outputStr, err)
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func StopCurrentKimaiActivities() error {
	activeRecords, err := fetchKimaiActiveRecords()
	if err != nil {
		err = fmt.Errorf("Error fetching active records in stopCurrentKimaiActivities: %w", err)
		return err
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
	url := config.KimaiUrl + "/api/activities"
	method := "POST"
	reqBody := map[string]interface{}{
		"name":       projectName,
		"project":    projectID,
		"visible":    true,
		"billable":   true,
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
		return nil, errors.New("error creating the default project activity")
	}

	return &createdKimaiActivity, nil
}

func fetchProjectKimaiActivity(prevErr error, projectName string,
	projectID int, branchOrProjectName string) (*KimaiActivity, error) {

	var noActivityFoundErrorPtr *NoActivityFoundError
	if errors.As(prevErr, noActivityFoundErrorPtr) && branchOrProjectName != projectName {
		projKimaiActivityPtr, projErr := fetchKimaiActivity(projectName, projectID)
		if projErr != nil {
			createdKimaiActivityPtr, createErr := createDefaultProjectKimaiActivity(projectName, projectID)
			projKimaiActivityPtr = createdKimaiActivityPtr
			projErr = createErr
		}
		if projErr != nil { 
			msg := "%w --after trying: %w"
			return nil, fmt.Errorf(msg, projErr, prevErr)
		}
		return projKimaiActivityPtr, nil
	} else {
		return nil, prevErr
	}
}

func StartCurrentGitBranchKimaiActivity() error {
	projectName, err := getProjectName()
	if err != nil {
		return err
	}

	branchOrProjectName, err := getCurrentGitBranch()
	if err != nil {
		return err
	}
	if branchOrProjectName == "master" || branchOrProjectName == "develop" {
		branchOrProjectName = projectName
	}
	projectID, ok := config.ProjectMap[projectName]
	if !ok {
		return errors.New("no activity associated with the current branch/project")
	}
	kimaiActivityPtr, err := fetchKimaiActivity(branchOrProjectName, projectID)
	if err != nil {
		projKimaiActivityPtr, projErr := fetchProjectKimaiActivity(err, projectName, projectID, branchOrProjectName)
		kimaiActivityPtr = projKimaiActivityPtr
		err = projErr
	}
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
	params := "?size=1"
	url := config.KimaiUrl + kimaiRecentPath + params
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

	validActiveRecords := filterValidRecords(recentRecords)
	if len(validActiveRecords) == 0 {
		return nil, errors.New("no recent records retrieved")
	}

	return &recentRecords[0], nil
}

func buildRestartRecordPath(recordID int) string {
	return fmt.Sprintf("/timesheets/%v/restart", recordID)
}

func restartKimaiRecord(recordID int) (*KimaiRecord, error) {
	url := config.KimaiUrl + buildRestartRecordPath(recordID)
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

func readConfig() error {
	configDir := getHomePath()
	err := os.MkdirAll(configDir, os.ModePerm)
	if err != nil {
		err = fmt.Errorf("Error mkdir'ing in readConfig: %w", err)
		return err
	}

	configFilePath := filepath.Join(configDir, configFileName)
	configFile, err := os.Open(configFilePath)
	if err != nil {
		err = fmt.Errorf("Error opening config file in readConfig: %w", err)
		return err
	}
	defer configFile.Close()

	configBytes, err := io.ReadAll(configFile)
	if err != nil {
		err = fmt.Errorf("Error reading config file in readConfig: %w", err)
		return err
	}

	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		err = fmt.Errorf("Error unmarshalling in readConfig: %w", err)
		return err
	}

	if config.KimaiUrl == "" {
		return errors.New("no Kimai URL specified in the config file")
	}
	if config.KimaiUsername == "" {
		return errors.New("no Kimai username specified in the config file")
	}
	if config.KimaiPassword == "" {
		return errors.New("no Kimai password specified in the config file")
	}
	if config.HourlyRate == 0 {
		return errors.New("no hourly rate specified in the config file")
	}
	if len(config.ProjectMap) == 0 {
		return errors.New("no project id map specified in the config file")
	}

	return nil
}

func parseCliArgsAndRun() error {
	stopOpPtr := flag.Bool("stop", false, "Stop current activity")
	startOpPtr := flag.Bool("start", false, "Start task for the current branch")
	restartOpPtr := flag.Bool("restart", false, "Restart previous activity")
	flag.Parse()

	var opErr error
	if *stopOpPtr {
		opErr = StopCurrentKimaiActivities()
	}
	if *startOpPtr && *restartOpPtr {
		return errors.New("you cannot start and restart tasks at the same time")
	}
	if *startOpPtr {
		opErr = StartCurrentGitBranchKimaiActivity()
	} else if *restartOpPtr {
		opErr = RestartLastKimaiRecord()
	}

	return opErr
}

func main() {
	err := readConfig()
	if err != nil {
		fmt.Println(err)
		return
	}

	opErr := parseCliArgsAndRun()
	if opErr != nil {
		fmt.Println(opErr)
		return
	}
}
