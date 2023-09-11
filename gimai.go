package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// TODO: if no matching activity for branch, use project name (or some other default)
// TODO: proper error handling
// TODO: factor out common logic for Kimai fetching
// TODO: help messages
// TODO: start specific task (cli arg)
// TODO: option to restart the last one

const kimaiTimesheetsPath = "/timesheets/active"
const configFileName = "gimai.json"

var configDir = os.Getenv("HOME") + "/.config/"
var config Config

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

func getNow() string {
	return time.Now().Format("2006-01-02T15:04:05")
}

func buildActivitiesPath(term string, projectID int) string {
	return fmt.Sprintf("/activities?term=%s&project=%d", term, projectID)
}

func fetchKimaiActivity(term string, projectID int) (*KimaiActivity, error) {
	if term == "" || projectID == 0 {
		return nil, errors.New("Empty term or invalid project id")
	}

	url := config.KimaiUrl + buildActivitiesPath(term, projectID)
	method := "GET"

	client := &http.Client{}
	httpReq, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("X-AUTH-USER", config.KimaiUsername)
	httpReq.Header.Set("X-AUTH-TOKEN", config.KimaiPassword)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var kimaiActivities []KimaiActivity
	err = json.Unmarshal(respBody, &kimaiActivities)
	if err != nil {
		fmt.Println(config)
		return nil, err
	}

	if len(kimaiActivities) == 0 {
		return nil, errors.New("No activities fetched")
	}

	if len(kimaiActivities) > 1 {
		return nil, errors.New("Multiple activities fetched")
	}

	kimaiActivity := kimaiActivities[0]
	if kimaiActivity.Id == 0 {
		return nil, errors.New("No valid activity fetched")
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
		return nil, err
	}
	bodyReader := bytes.NewReader(reqBodyBytes)

	client := &http.Client{}
	httpReq, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("X-AUTH-USER", config.KimaiUsername)
	httpReq.Header.Set("X-AUTH-TOKEN", config.KimaiPassword)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var startedKimaiActivity KimaiActivity
	err = json.Unmarshal(respBody, &startedKimaiActivity)
	if err != nil {
		return nil, err
	}

	if startedKimaiActivity.Id == 0 {
		return nil, errors.New("No activity started")
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

	client := &http.Client{}
	httpReq, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("X-AUTH-USER", config.KimaiUsername)
	httpReq.Header.Set("X-AUTH-TOKEN", config.KimaiPassword)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var activeRecords []KimaiRecord
	err = json.Unmarshal(respBody, &activeRecords)
	if err != nil {
		return nil, err
	}

	validActiveRecords := filterValidRecords(activeRecords)
	if len(validActiveRecords) == 0 {
		return nil, errors.New("No active records retrieved")
	}

	return activeRecords, nil
}

func buildStopActivityPath(activityID int) string {
	return fmt.Sprintf("/timesheets/%v/stop", activityID)
}

func stopKimaiRecord(activityID int) (*KimaiActivity, error) {
	url := config.KimaiUrl + buildStopActivityPath(activityID)
	method := "PATCH"

	client := &http.Client{}
	httpReq, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("X-AUTH-USER", config.KimaiUsername)
	httpReq.Header.Set("X-AUTH-TOKEN", config.KimaiPassword)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var stoppedActivity KimaiActivity
	err = json.Unmarshal(respBody, &stoppedActivity)
	if err != nil {
		return nil, err
	}

	if stoppedActivity.Id == 0 {
		return nil, errors.New("No stopped activity")
	}

	return &stoppedActivity, nil
}

func getProjectName() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.CombinedOutput()
	outputStr := string(output)
	if err != nil {
		return "", errors.New(outputStr)
	}
	parts := strings.Split(strings.TrimSpace(outputStr), "/")
	projectName := parts[len(parts)-1]

	return projectName, nil
}

func getCurrentGitBranch() (string, error) {
	cmd := exec.Command("git", "branch", "--show-current")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func StopCurrentKimaiActivities() error {
	activeRecords, err := fetchKimaiActiveRecords()
	if err != nil {
		return err
	}

	var kimaiActivity *KimaiActivity
	for i := 0; i < len(activeRecords); i++ {
		activeRecord := activeRecords[i]
		kimaiActivity, err = stopKimaiRecord(activeRecord.Id)
		if err != nil {
			return err
		}
		fmt.Println("Stopped active record", kimaiActivity.Id)
	}

	return nil
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
		return errors.New("No activity associated with the current branch/project")
	}
	kimaiActivityPtr, err := fetchKimaiActivity(branchOrProjectName, projectID)
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

func readConfig() error {
	err := os.MkdirAll(configDir, os.ModePerm)
	if err != nil {
		return err
	}

	configFilePath := configDir + configFileName
	configFile, err := os.Open(configFilePath)
	if err != nil {
		return err
	}
	defer configFile.Close()

	configBytes, err := ioutil.ReadAll(configFile)
	if err != nil {
		return err
	}

	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		return err
	}

	if config.KimaiUrl == "" {
		return errors.New("No Kimai URL specified in the config file")
	}
	if config.KimaiUsername == "" {
		return errors.New("No Kimai username specified in the config file")
	}
	if config.KimaiPassword == "" {
		return errors.New("No Kimai password specified in the config file")
	}
	if config.HourlyRate == 0 {
		return errors.New("No hourly rate specified in the config file")
	}
	if len(config.ProjectMap) == 0 {
		return errors.New("No project id map specified in the config file")
	}

	return nil
}

func main() {
	err := readConfig()
	if err != nil {
		fmt.Println(err)
		return
	}

	stopOpPtr := flag.Bool("stop", false, "Stop current activity")
	flag.Parse()

	var opErr error
	if *stopOpPtr {
		opErr = StopCurrentKimaiActivities()
	} else {
		opErr = StartCurrentGitBranchKimaiActivity()
	}

	if opErr != nil {
		fmt.Println(opErr)
		return
	}
}
