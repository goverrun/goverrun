package goverrun

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	// start test server target
	go server()

	// add loadtest scenario
	err := AddScenario(&Scenario{
		Title:       "Stats Test",
		Description: "",
		Runner:      scenarioStatsTest,
		LoadConfig: LoadConfig{
			StartDelay: RandomInterval{
				Min: 50 * time.Millisecond,
				Max: 200 * time.Millisecond,
			},
			LoopingUsers: 100,
			LoopDelay: RandomInterval{
				Min: 50 * time.Millisecond,
				Max: 200 * time.Millisecond,
			},
			RampUp:   1 * time.Second,
			Plateau:  10 * time.Second,
			RampDown: 2 * time.Second,
		},
		Ignored: false})
	panicOnErr(err)

	// add loadtest scenario
	err = AddScenario(&Scenario{
		Title:       "Contains Test",
		Description: "",
		Runner:      scenarioContainsTest,
		LoadConfig: LoadConfig{
			StartDelay: RandomInterval{
				Min: 50 * time.Millisecond,
				Max: 200 * time.Millisecond,
			},
			LoopingUsers: 100,
			LoopDelay: RandomInterval{
				Min: 50 * time.Millisecond,
				Max: 200 * time.Millisecond,
			},
			RampUp:   1 * time.Second,
			Plateau:  10 * time.Second,
			RampDown: 2 * time.Second,
		},
		Ignored: false})
	panicOnErr(err)

	// add loadtest scenario
	err = AddScenario(&Scenario{
		Title:       "Status Code Test",
		Description: "",
		Runner:      scenarioStatusCodeTest,
		LoadConfig: LoadConfig{
			StartDelay: RandomInterval{
				Min: 50 * time.Millisecond,
				Max: 200 * time.Millisecond,
			},
			LoopingUsers: 100,
			LoopDelay: RandomInterval{
				Min: 50 * time.Millisecond,
				Max: 200 * time.Millisecond,
			},
			RampUp:   1 * time.Second,
			Plateau:  10 * time.Second,
			RampDown: 2 * time.Second,
		},
		Ignored: false})
	panicOnErr(err)

	// add loadtest scenario
	err = AddScenario(&Scenario{
		Title:       "Timeout Test",
		Description: "",
		Runner:      scenarioTimeoutTest,
		LoadConfig: LoadConfig{
			StartDelay: RandomInterval{
				Min: 50 * time.Millisecond,
				Max: 200 * time.Millisecond,
			},
			LoopingUsers: 100,
			LoopDelay: RandomInterval{
				Min: 50 * time.Millisecond,
				Max: 200 * time.Millisecond,
			},
			RampUp:   1 * time.Second,
			Plateau:  10 * time.Second,
			RampDown: 2 * time.Second,
		},
		Ignored: false})
	panicOnErr(err)

	// run loadtest
	output := "/tmp/goverrun-test"
	Run(output, false)
	GenerateResultsReport(output)
}

func server() {
	http.HandleFunc("/", hello)
	err := http.ListenAndServe(":8765", nil)
	if err != nil {
		panic(err)
	}
}

func hello(w http.ResponseWriter, r *http.Request) {
	qParams := r.URL.Query()
	content, delay, statuscode := qParams["content"], qParams["delay"], qParams["statuscode"]
	if len(statuscode) > 0 && len(statuscode[0]) > 0 { // URL parameter 'statuscode' is present and filled
		sc, _ := strconv.Atoi(statuscode[0])
		w.WriteHeader(sc)
	}
	if len(delay) > 0 && len(delay[0]) > 0 { // URL parameter 'delay' is present and filled
		secs, err := strconv.Atoi(delay[0])
		if err != nil {
			panic(err)
		}
		time.Sleep(time.Duration(secs) * time.Second)
		_, _ = fmt.Fprintf(w, "hello with delay of %d seconds\n", secs)
	} else { // URL parameter 'delay' is missing
		_, _ = fmt.Fprintf(w, "hello without delay\n")
	}
	if len(content) > 0 && len(content[0]) > 0 { // URL parameter 'content' is present and filled
		w.Write([]byte(content[0]))
	}
}

// ===========================================[ scenarios ]===========================================

func scenarioStatsTest(user *User) {
	delay := RandomNumber(0, 3)
	response := user.Step(fmt.Sprintf("request with delay %ds", delay)).ExpectSuccessPercentageAtLeast(100).
		Request(http.MethodGet, fmt.Sprintf("http://127.0.0.1:8765?delay=%d", delay)).
		SendWithTimeout(5 * time.Second).
		AssertStatusCode(http.StatusOK).AssertBodyContains("hello").
		ArchiveStats().PrintStats(os.Stdout)
	interval := response.Timestamps.Done.Sub(response.Timestamps.Start).Seconds()
	deviation := interval - float64(delay)
	if deviation < 0 || deviation > 0.5 {
		panic("deviation has unexpected value")
	}
	checkUserData(user)
}

func scenarioContainsTest(user *User) {
	content := RandomNumber(1111111, 9999999)
	response := user.Step("request with content").ExpectSuccessPercentageAtLeast(100).
		Request(http.MethodGet, fmt.Sprintf("http://127.0.0.1:8765?content=%d", content)).
		SendWithTimeout(5 * time.Second).
		AssertStatusCode(http.StatusOK).AssertBodyContains(strconv.Itoa(content))
	fail := "deliberately wrong check here"
	response.AssertBodyContains(fail).ArchiveStats().PrintStats(os.Stdout)
	if response.AssertionFailed != "assertion of body content failed (response body did not contain expected value): "+fail {
		panic("failed assertion not detected")
	}
	user.ThinkTime(RandomDuration(1500*time.Millisecond, 1750*time.Millisecond))
}

func scenarioStatusCodeTest(user *User) {
	sc := 200
	if RandomNumber(1, 50) == 1 {
		sc = 502
	}
	response := user.Step("request with status code").ExpectSuccessPercentageAtLeast(100).
		Request(http.MethodGet, fmt.Sprintf("http://127.0.0.1:8765?statuscode=%d", sc)).
		SendWithTimeout(5 * time.Second).
		AssertStatusCode(http.StatusOK) // deliberately wrong check here for the about 2% of test cases where sc != 200, so expect ~2% of Failures in stats listed
	response.ArchiveStats()
	if sc != http.StatusOK && response.AssertionFailed != "assertion of status code failed: got "+strconv.Itoa(response.StatusCode)+" want 200" {
		panic("failed assertion not detected")
	}
	user.ThinkTime(RandomDuration(10*time.Millisecond, 20*time.Millisecond))
}

func scenarioTimeoutTest(user *User) {
	timeout := 4
	delay := RandomNumber(0, 9)
	response := user.Step(fmt.Sprintf("request with delay %ds (and timeout defined as %ds)", delay, timeout)).ExpectSuccessPercentageAtLeast(100).
		Request(http.MethodGet, fmt.Sprintf("http://127.0.0.1:8765?delay=%d", delay)).
		SendWithTimeout(time.Duration(timeout) * time.Second)
	if delay > timeout {
		if response.Timeout == nil {
			panic(fmt.Sprintf("expected timeout error, but didn't get one: %ds delay with %ds timeout", delay, timeout))
		}
	} else {
		response.AssertStatusCode(http.StatusOK).AssertBodyContains("hello").ArchiveStats().PrintStats(os.Stdout)
		interval := response.Timestamps.Done.Sub(response.Timestamps.Start).Seconds()
		deviation := interval - float64(delay)
		if deviation < 0 || deviation > 0.5 {
			panic("deviation has unexpected value")
		}
	}
	checkUserData(user)
}

func checkUserData(user *User) {
	if user.CurrentLoop == 1 {
		user.Data["test"] = user.CurrentUser
	} else {
		if user.Data["test"].(int) != user.CurrentUser {
			panic("user data was not carried over consistently")
		}
	}
}
