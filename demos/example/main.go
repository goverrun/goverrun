package main

import (
	"fmt"
	. "github.com/goverrun/goverrun/core"
	"net/http"
	"os"
	"regexp"
	"time"
)

const debug = false

var build string // set during build
var targetURL string

func main() {
	CommandlineDefaults(3, 3, 10, 3, "/tmp/marathon")
	fmt.Println("Build:", build)

	err := AddScenario(&Scenario{
		Title:       "view standings",
		Description: "viewing standings of two different disciplines",
		Runner:      viewStandings,
		LoadConfig:  DefaultLoadConfigFromArgs()})
	CheckErrAndLogError(err, "unable to add scenario")

	err = AddScenario(&Scenario{
		Title:       "search runners",
		Description: "searching for runners in two different ways",
		Runner:      searchRunners,
		LoadConfig:  DefaultLoadConfigFromArgs()})
	CheckErrAndLogError(err, "unable to add scenario")

	err = AddScenario(&Scenario{
		Title:       "view profiles",
		Description: "viewing of runner profiles via search and standings",
		Runner:      viewProfiles,
		LoadConfig:  DefaultLoadConfigFromArgs()})
	CheckErrAndLogError(err, "unable to add scenario")

	err = AddScenario(&Scenario{
		Title:       "edit profiles",
		Description: "editing of runner profiles",
		Runner:      editProfiles,
		LoadConfig:  DefaultLoadConfigFromArgs()})
	CheckErrAndLogError(err, "unable to add scenario")

	if debug {
		Proxy = "http://127.0.0.1:8080"
		SkipCertificateValidation = true
		AddUserLoopHeader = true
		AddScenarioStepHeader = true
	}
	AddRequestInterceptor(func(u *User, r *http.Request) {
		r.Header.Set("Load-Test", "Target 'Demo' - Emergency Contact: mail@example.com")
	})

	if len(CommandlineArgs.SubcommandArgs) == 0 {
		LogError("No final subcommand argument as target provided")
		os.Exit(1)
	}
	targetURL = CommandlineArgs.SubcommandArgs[0]
	RunFromCommandlineArgs()
}

// ===========================================[ scenarios ]===========================================

func viewStandings(user *User) {
	doOpenStartPage(user)
	doViewStandings(user, true)
	doGoBackHome(user)
	doViewStandings(user, false)
}

func searchRunners(user *User) {
	doOpenStartPage(user)
	doSubmitRunnerSearch(user, true)
	doGoBackHome(user)
	doSubmitRunnerSearch(user, false)
}

func viewProfiles(user *User) {
	doOpenStartPage(user)
	doSubmitRunnerSearch(user, true)
	doViewRunnerProfile(user, true)
	doGoBackHome(user)
	doViewStandings(user, true)
	doViewRunnerProfile(user, false)
}

func editProfiles(user *User) {
	doOpenStartPage(user)
	doLogin(user)
	doEditProfile(user)
	doLogout(user)
}

// ===========================================[ helpers ]===========================================

func doOpenStartPage(user *User) {
	user.ThinkTime(RandomDuration(100*time.Millisecond, 500*time.Millisecond))
	user.Step("open start page").
		ExpectSuccessPercentageAtLeast(95).
		Request(http.MethodGet, targetURL+"showMarathons.page").
		SetHeader("Some-Header", "Example").
		SetHeader("Some-Other-Header", "Some Other Example").
		SetCookie("Some-Custom-Cookie", "Some Value (received cookies are handled and sent automatically via per-user cookie-jar)").
		SendWithTimeout(3 * time.Second).
		AssertStatusCode(http.StatusOK).AssertBodyContains("Results table Full Distance Marathon").ArchiveStats()
	user.ThinkTime(RandomDuration(200*time.Millisecond, 2000*time.Millisecond))
}

var reDate = regexp.MustCompile(`.*01\.01\.1970.*`)
var reConnection = regexp.MustCompile(`.*connection.*`)

func doLogin(user *User) {
	user.Step("open login page").
		ExpectSuccessPercentageAtLeast(95).
		Request(http.MethodGet, targetURL+"secured/profile.page").
		SendWithTimeout(3 * time.Second).
		AssertStatusCode(http.StatusOK).AssertBodyContains("Marathon-Login").ArchiveStats()

	user.ThinkTime(RandomDuration(300*time.Millisecond, 1000*time.Millisecond))

	user.Step("submit login").
		ExpectSuccessCountAtLeast(3).
		ExpectSuccessPercentageAtLeast(95).
		ExpectFailurePercentageAtMost(1).
		ExpectErrorPercentageAtMost(1).
		ExpectTimeoutPercentageAtMost(1).
		ExpectTotalRequestResponseTimePercentileLimit(90, 10*time.Millisecond).
		ExpectTotalRequestResponseTimePercentileLimit(95, 50*time.Millisecond).
		ExpectTotalRequestResponseTimePercentileLimit(99, 100*time.Millisecond).
		ExpectTimeToFirstBytePercentileLimit(95, 40*time.Millisecond).
		ExpectTimeAfterRequestSentPercentileLimit(99, 80*time.Millisecond).
		ExpectTotalRequestBytesWithin(1000, 100000).
		ExpectTotalResponseBytesWithin(1000, 100000).
		ExpectStatusCodePercentageAtLeast(http.StatusOK, 95).
		ExpectStatusCodePercentageAtMost(http.StatusBadGateway, 5).
		ExpectFailureTypeMatchesPercentageAtMost(reConnection, 15).
		Request(http.MethodPost, targetURL+"secured/j_security_check").
		SetFormParam("j_username", "test").
		SetFormParam("j_password", "test").
		SendWithTimeout(5 * time.Second).
		AssertStatusCode(http.StatusOK).AssertBodyContains("Welcome <b>test</b>").AssertBodyMatches(reDate).ArchiveStats()

	user.ThinkTime(RandomDuration(200*time.Millisecond, 2000*time.Millisecond))
}

func doEditProfile(user *User) {
	user.Step("submit profile edit").
		ExpectSuccessPercentageAtLeast(95).
		Request(http.MethodPost, targetURL+"secured/updateRunnerProfile.page").
		SetFormParam("username", "test").
		SetFormParam("firstname", "Test-was-edited").
		SetFormParam("lastname", "Test").
		SetFormParam("street", "Teststr. 123a").
		SetFormParam("zip", "12345").
		SetFormParam("city", "Test").
		SetFormParam("creditcardNumber", "1234123412345678").
		SetFormParam("dateOfBirth", "01.01.1970").
		SetFormParam("id", "45").
		SetFormParam("state", "PG1hcC8+").
		SendWithTimeout(3 * time.Second).AssertStatusCode(http.StatusOK).
		AssertBodyContains("Your data has been saved").AssertBodyContains("Test-was-edited").ArchiveStats()
	user.ThinkTime(RandomDuration(200*time.Millisecond, 2000*time.Millisecond))
}

func doViewStandings(user *User, fullDistance bool) {
	if fullDistance {
		user.Step("view standing").
			ExpectSuccessPercentageAtLeast(95).
			Request(http.MethodGet, targetURL+"showResults.page?marathon=0").
			SendWithTimeout(3 * time.Second).
			AssertBodyContains("Marathon Results Full Distance Marathon").AssertStatusCode(http.StatusOK).ArchiveStats()
	} else {
		user.Step("view standing").
			ExpectSuccessPercentageAtLeast(95).
			Request(http.MethodGet, targetURL+"showResults.page?marathon=1").
			SendWithTimeout(3 * time.Second).
			AssertBodyContains("Marathon Results Half Distance Marathon").AssertStatusCode(http.StatusOK).ArchiveStats()
	}
	user.ThinkTime(RandomDuration(2000*time.Millisecond, 4500*time.Millisecond))
	return
}

func doGoBackHome(user *User) {
	user.Step("go back home").
		ExpectSuccessPercentageAtLeast(95).
		Request(http.MethodGet, targetURL+"showMarathons.page").
		SendWithTimeout(3 * time.Second).
		AssertStatusCode(http.StatusOK).AssertBodyContains("Results table Full Distance Marathon").ArchiveStats()
	user.ThinkTime(RandomDuration(200*time.Millisecond, 500*time.Millisecond))
}

func doLogout(user *User) {
	user.Step("logout").
		ExpectSuccessPercentageAtLeast(95).
		Request(http.MethodGet, targetURL+"logout.page").
		SendWithTimeout(3 * time.Second).
		AssertStatusCode(http.StatusOK).AssertBodyContains("Enter Marathon application").ArchiveStats()
	user.ThinkTime(RandomDuration(200*time.Millisecond, 500*time.Millisecond))
}

func doSubmitRunnerSearch(user *User, john bool) {
	if john {
		user.Step("submit runner search").
			ExpectSuccessPercentageAtLeast(95).
			Request(http.MethodPost, targetURL+"searchRunner.page").
			SetFormParam("searchTerm", "john").
			SendWithTimeout(3 * time.Second).
			AssertBodyContains("<b>John Jogger</b>").AssertStatusCode(http.StatusOK).ArchiveStats()
	} else {
		user.Step("submit runner search").
			ExpectSuccessPercentageAtLeast(95).
			Request(http.MethodPost, targetURL+"searchRunner.page").
			SetFormParam("searchTerm", "jane").
			SendWithTimeout(3 * time.Second).
			AssertBodyContains("<b>jane Jane</b>").AssertStatusCode(http.StatusOK).ArchiveStats()
	}
	user.ThinkTime(RandomDuration(2000*time.Millisecond, 4500*time.Millisecond))
	return
}

func doViewRunnerProfile(user *User, john bool) {
	if john {
		user.Step("view profile").
			ExpectSuccessPercentageAtLeast(95).
			Request(http.MethodGet, targetURL+"showRunner.page?runner=50").
			SendWithTimeout(3 * time.Second).
			AssertBodyContains("Runner John Jogger").AssertStatusCode(http.StatusOK).ArchiveStats()
	} else {
		user.Step("view profile").
			ExpectSuccessPercentageAtLeast(95).
			Request(http.MethodGet, targetURL+"showRunner.page?runner=51").
			SendWithTimeout(3 * time.Second).
			AssertBodyContains("Runner jane Jane").AssertStatusCode(http.StatusOK).ArchiveStats()
	}
	user.ThinkTime(RandomDuration(750*time.Millisecond, 3500*time.Millisecond))
	return
}
