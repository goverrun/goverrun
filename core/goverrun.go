package goverrun

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/gob"
	"flag"
	"fmt"
	"github.com/PaesslerAG/gval"
	"github.com/PaesslerAG/jsonpath"
	"html"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	scenariosDefaultFilename                             = "scenarios.goverrun"
	stepDefaultFilenamePattern, stepDefaultFilenameMatch = "step-%d.goverrun", "step-*.goverrun"
)

var (
	// exported
	AddUserLoopHeader         bool
	AddScenarioStepHeader     bool
	SkipCertificateValidation bool
	Proxy                     string
	UserAgent                 string

	// internal
	verbose              bool
	scenarios            = make(map[string]*Scenario)
	requestInterceptors  = make([]func(u *User, r *http.Request), 0)
	currentLoopingUsers  = safeTracker{counters: make(map[string]int)}
	folder               string
	scenariosWriter      *scenariosGobWriter
	stepHistogramWriters = make(map[string]*stepGobWriter)

	printLock     sync.Mutex
	histogramLock sync.Mutex
)

func Reset() {
	AddUserLoopHeader = false
	AddScenarioStepHeader = false
	SkipCertificateValidation = false
	Proxy = ""
	verbose = false
	scenarios = make(map[string]*Scenario)
	requestInterceptors = make([]func(u *User, r *http.Request), 0)
	currentLoopingUsers = safeTracker{counters: make(map[string]int)}
	folder = ""
	scenariosWriter = nil
	stepHistogramWriters = make(map[string]*stepGobWriter)
}

type CommandlineArguments struct {
	Run struct {
		LoopingUsers, RampUpSeconds, PlateauSeconds, RampDownSeconds *int
		Folder                                                       *string
	}
	Report struct {
		Folder *string
	}
	SubcommandArgs []string
}

var (
	SubcommandReport *flag.FlagSet
	SubcommandRun    *flag.FlagSet
	CommandlineArgs  = &CommandlineArguments{}
)

func CommandlineDefaults(users, RampUpSeconds, plateauSeconds, rampDownSeconds int, reportPath string) {
	fmt.Println(`
   ______                                    
  / ____/___ _   _____  ____________  ______ 
 / / __/ __ \ | / / _ \/ ___/ ___/ / / / __ \
/ /_/ / /_/ / |/ /  __/ /  / /  / /_/ / / / /
\____/\____/|___/\___/_/  /_/   \__,_/_/ /_/
Agile Load Testing - https://goverrun.io`)
	fmt.Println()

	SubcommandRun = flag.NewFlagSet("run", flag.ExitOnError)
	SubcommandRun.SetOutput(os.Stdout)
	CommandlineArgs.Run.LoopingUsers = SubcommandRun.Int("users", users, "number of looping users")
	CommandlineArgs.Run.RampUpSeconds = SubcommandRun.Int("ramp-up", RampUpSeconds, "ramp-up duration in seconds")
	CommandlineArgs.Run.PlateauSeconds = SubcommandRun.Int("plateau", plateauSeconds, "plateau duration in seconds")
	CommandlineArgs.Run.RampDownSeconds = SubcommandRun.Int("ramp-down", rampDownSeconds, "ramp-down duration in seconds")
	CommandlineArgs.Run.Folder = SubcommandRun.String("path", reportPath, "report output folder")
	// use the Base-URL as last argument

	SubcommandReport = flag.NewFlagSet("report", flag.ExitOnError)
	SubcommandReport.SetOutput(os.Stdout)
	CommandlineArgs.Report.Folder = SubcommandReport.String("path", reportPath, "report input folder")

	// Verify that a subcommand has been provided
	// os.Arg[0] is the main command
	// os.Arg[1] will be the subcommand
	if len(os.Args) < 2 {
		PrintMissingSubcommandAndExit(SubcommandRun, SubcommandReport)
	}

	switch os.Args[1] {
	case SubcommandRun.Name():
		err := SubcommandRun.Parse(os.Args[2:])
		panicOnErr(err)
		CommandlineArgs.SubcommandArgs = SubcommandRun.Args()
		/* left to be decided by the caller
		if len(CommandlineArgs.SubcommandArgs) == 0 {
			LogFatal("Missing required subcommand target (use as argument at the end)")
			os.Exit(1)
		}
		*/
	case SubcommandReport.Name():
		err := SubcommandReport.Parse(os.Args[2:])
		panicOnErr(err)
		CommandlineArgs.SubcommandArgs = SubcommandReport.Args()
	default:
		PrintMissingSubcommandAndExit(SubcommandRun, SubcommandReport)
	}
}

func RunFromCommandlineArgs() {
	var reportPath string
	if SubcommandRun.Parsed() {
		reportPath = *CommandlineArgs.Run.Folder
		Run(reportPath, verbose)
	} else if SubcommandReport.Parsed() {
		reportPath = *CommandlineArgs.Run.Folder
	}
	unmetExpectation := GenerateResultsReport(reportPath)
	if unmetExpectation {
		LogWarning("Unmet expectation")
		os.Exit(3)
	}
}

type Expectation struct {
	SuccessPercentageAtLeast                 *PercentageExpectation
	FailurePercentageAtMost                  *PercentageExpectation
	ErrorPercentageAtMost                    *PercentageExpectation
	TimeoutPercentageAtMost                  *PercentageExpectation
	SuccessCountAtLeast                      *CountExpectation
	FailureCountAtMost                       *CountExpectation
	ErrorCountAtMost                         *CountExpectation
	TimeoutCountAtMost                       *CountExpectation
	TotalRequestResponseTimePercentileLimits []*PercentileExpectation
	TimeToFirstBytePercentileLimits          []*PercentileExpectation
	TimeAfterRequestSentPercentileLimits     []*PercentileExpectation
	TotalRequestBytesWithin                  *RangeExpectation
	TotalResponseBytesWithin                 *RangeExpectation
	StatusCodeThresholds                     []*StatusCodeExpectation
	FailureTypeMatchesThresholds             []*TypeMatchesThreshold
	ErrorTypeMatchesThresholds               []*TypeMatchesThreshold
	TimeoutTypeMatchesThresholds             []*TypeMatchesThreshold
}

type PercentageExpectation struct {
	Percentage  float64
	Unmet       bool
	ActualValue float64
}

type CountExpectation struct {
	Count       uint64
	Unmet       bool
	ActualValue uint64
}

type TypeMatchesThreshold struct {
	IsAtLeast   bool
	RegExp      string
	Percentage  float64
	Unmet       bool
	ActualValue float64
}

type StatusCodeExpectation struct {
	IsAtLeast   bool
	StatusCode  int
	Percentage  float64
	Unmet       bool
	ActualValue float64
}

type RangeExpectation struct {
	Min, Max    uint64
	Unmet       bool
	ActualValue uint64
}

type PercentileExpectation struct {
	Percentile  float64
	Duration    time.Duration
	Unmet       bool
	ActualValue time.Duration
}

func AddRequestInterceptor(fn func(u *User, r *http.Request)) {
	requestInterceptors = append(requestInterceptors, fn)
}

type Counts struct {
	Requests uint64
	Timeouts uint64
	Failures uint64
	Errors   uint64
}

func (c Counts) Successes() uint64 {
	return c.Requests - c.Failures - c.Errors - c.Timeouts
}

func (c Counts) SuccessPercentage() float64 {
	return float64(c.Successes()) / float64(c.Requests) * 100
}

func (c Counts) FailurePercentage() float64 {
	return float64(c.Failures) / float64(c.Requests) * 100
}

func (c Counts) ErrorPercentage() float64 {
	return float64(c.Errors) / float64(c.Requests) * 100
}

func (c Counts) TimeoutPercentage() float64 {
	return float64(c.Timeouts) / float64(c.Requests) * 100
}

type Environment struct {
	Hostname string
	Start    time.Time
}

type User struct {
	Scenario                 string
	CurrentUser, CurrentLoop int
	HttpClient               *http.Client
	Disabled                 bool
	Data                     map[string]interface{} // intended to set custom values
}

func (user *User) printStep(step *Step) {
	LogInfof("[%d:%d] %s\n", user.CurrentUser, user.CurrentLoop, step.Name)
}

func (user *User) ThinkTime(d time.Duration) *User {
	if user.Disabled {
		return user
	}
	// TODO track (i.e. sum up) also total sleep times for this user for stats
	time.Sleep(d)
	return user
}

func (user *User) ThinkTimeRandom(min, max time.Duration) *User {
	if user.Disabled {
		return user
	}
	// TODO track (i.e. sum up) also total sleep times for this user for stats
	time.Sleep(RandomDuration(min, max))
	return user
}

func (user *User) Step(name string) *Step {
	return &Step{
		Name: name,
		User: user,
		Expectation: &Expectation{
			SuccessPercentageAtLeast: &PercentageExpectation{Percentage: 0},
			FailurePercentageAtMost:  &PercentageExpectation{Percentage: 100},
			ErrorPercentageAtMost:    &PercentageExpectation{Percentage: 100},
			TimeoutPercentageAtMost:  &PercentageExpectation{Percentage: 100},
		},
	}
}

type Step struct {
	Name        string
	User        *User
	Expectation *Expectation
	// TODO add optional field "Description"?
}

func isValidPercentage(percentage float64) bool {
	if percentage < 0 || percentage > 100 {
		LogWarning("invalid percentage provided (expected between 0.0 and 100.0)", percentage)
		return false
	}
	return true
}

// ExpectSuccessPercentageAtLeast sets the minimum success percentage level which is expected for this step.
// Values may range from 0.0 to 100.0 percent.
//
// When invoked multiple times, only the percentage level when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectSuccessPercentageAtLeast(percentage float64) *Step {
	if isValidPercentage(percentage) {
		step.Expectation.SuccessPercentageAtLeast = &PercentageExpectation{Percentage: percentage}
	}
	return step
}

// ExpectSuccessCountAtLeast sets the minimum success count which is expected for this step.
//
// When invoked multiple times, only the count when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectSuccessCountAtLeast(count uint64) *Step {
	step.Expectation.SuccessCountAtLeast = &CountExpectation{Count: count}
	return step
}

// ExpectFailurePercentageAtMost sets the maximum failure percentage level which is expected for this step.
// Values may range from 0.0 to 100.0 percent.
//
// When invoked multiple times, only the percentage level when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectFailurePercentageAtMost(percentage float64) *Step {
	if isValidPercentage(percentage) {
		step.Expectation.FailurePercentageAtMost = &PercentageExpectation{Percentage: percentage}
	}
	return step
}

// ExpectFailureCountAtMost sets the maximum failure count which is expected for this step.
//
// When invoked multiple times, only the count when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectFailureCountAtMost(count uint64) *Step {
	step.Expectation.FailureCountAtMost = &CountExpectation{Count: count}
	return step
}

// ExpectErrorPercentageAtMost sets the maximum failure percentage level which is expected for this step.
// Values may range from 0.0 to 100.0 percent.
//
// When invoked multiple times, only the percentage level when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectErrorPercentageAtMost(percentage float64) *Step {
	if isValidPercentage(percentage) {
		step.Expectation.ErrorPercentageAtMost = &PercentageExpectation{Percentage: percentage}
	}
	return step
}

// ExpectErrorCountAtMost sets the maximum error count which is expected for this step.
//
// When invoked multiple times, only the count when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectErrorCountAtMost(count uint64) *Step {
	step.Expectation.ErrorCountAtMost = &CountExpectation{Count: count}
	return step
}

// ExpectTimeoutPercentageAtMost sets the maximum failure percentage level which is expected for this step.
// Values may range from 0.0 to 100.0 percent.
//
// When invoked multiple times, only the percentage level when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectTimeoutPercentageAtMost(percentage float64) *Step {
	if isValidPercentage(percentage) {
		step.Expectation.TimeoutPercentageAtMost = &PercentageExpectation{Percentage: percentage}
	}
	return step
}

// ExpectTimeoutCountAtMost sets the maximum timeout count which is expected for this step.
//
// When invoked multiple times, only the count when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectTimeoutCountAtMost(count uint64) *Step {
	step.Expectation.TimeoutCountAtMost = &CountExpectation{Count: count}
	return step
}

// ExpectTotalRequestResponseTimePercentileLimit sets the expectation of the duration for the given percentile
// is within the given maximum duration. Percent values may range from 0.0 to 100.0 percent.
//
// When invoked multiple times, only the percentile expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectTotalRequestResponseTimePercentileLimit(percentile float64, duration time.Duration) *Step {
	if isValidPercentage(percentile) {
		step.Expectation.TotalRequestResponseTimePercentileLimits = append(step.Expectation.TotalRequestResponseTimePercentileLimits, &PercentileExpectation{
			Percentile: percentile,
			Duration:   duration,
		})
	}
	return step
}

// ExpectTimeToFirstBytePercentileLimit sets the expectation of the duration for the given percentile
// is within the given maximum duration. Percent values may range from 0.0 to 100.0 percent.
//
// When invoked multiple times, only the percentile expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectTimeToFirstBytePercentileLimit(percentile float64, duration time.Duration) *Step {
	if isValidPercentage(percentile) {
		step.Expectation.TimeToFirstBytePercentileLimits = append(step.Expectation.TimeToFirstBytePercentileLimits, &PercentileExpectation{
			Percentile: percentile,
			Duration:   duration,
		})
	}
	return step
}

// ExpectTimeAfterRequestSentPercentileLimit sets the expectation of the duration for the given percentile
// is within the given maximum duration. Percent values may range from 0.0 to 100.0 percent.
//
// When invoked multiple times, only the percentile expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectTimeAfterRequestSentPercentileLimit(percentile float64, duration time.Duration) *Step {
	if isValidPercentage(percentile) {
		step.Expectation.TimeAfterRequestSentPercentileLimits = append(step.Expectation.TimeAfterRequestSentPercentileLimits, &PercentileExpectation{
			Percentile: percentile,
			Duration:   duration,
		})
	}
	return step
}

// ExpectTotalRequestBytesWithin sets the expectation of the total request byte count for the given step.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectTotalRequestBytesWithin(min, max uint64) *Step {
	step.Expectation.TotalRequestBytesWithin = &RangeExpectation{
		Min: min,
		Max: max,
	}
	return step
}

// ExpectTotalResponseBytesWithin sets the expectation of the total request byte count for the given step.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectTotalResponseBytesWithin(min, max uint64) *Step {
	step.Expectation.TotalResponseBytesWithin = &RangeExpectation{
		Min: min,
		Max: max,
	}
	return step
}

// ExpectStatusCodePercentageAtLeast sets the expectation of the status code count (of the received status codes) for the given step to be at least the given value.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectStatusCodePercentageAtLeast(statusCode int, percentage float64) *Step {
	addStatusCodeCheck(step, statusCode, percentage, true)
	return step
}

// ExpectStatusCodePercentageAtMost sets the expectation of the status code count (of the received status codes) for the given step to be at most the given value.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectStatusCodePercentageAtMost(statusCode int, percentage float64) *Step {
	addStatusCodeCheck(step, statusCode, percentage, false)
	return step
}

// ExpectFailureTypeMatchesPercentageAtLeast sets the expectation of the failure type count (of the received failure types) for the given step to be at least the given value.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectFailureTypeMatchesPercentageAtLeast(re *regexp.Regexp, percentage float64) *Step {
	step.Expectation.FailureTypeMatchesThresholds = append(step.Expectation.FailureTypeMatchesThresholds, &TypeMatchesThreshold{
		IsAtLeast:  true,
		RegExp:     re.String(),
		Percentage: percentage,
	})
	return step
}

// ExpectFailureTypeMatchesPercentageAtMost sets the expectation of the failure type count (of the received failure types) for the given step to be at most the given value.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectFailureTypeMatchesPercentageAtMost(re *regexp.Regexp, percentage float64) *Step {
	step.Expectation.FailureTypeMatchesThresholds = append(step.Expectation.FailureTypeMatchesThresholds, &TypeMatchesThreshold{
		IsAtLeast:  false,
		RegExp:     re.String(),
		Percentage: percentage,
	})
	return step
}

// ExpectErrorTypeMatchesPercentageAtLeast sets the expectation of the error type count (of the received error types) for the given step to be at least the given value.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectErrorTypeMatchesPercentageAtLeast(re *regexp.Regexp, percentage float64) *Step {
	step.Expectation.ErrorTypeMatchesThresholds = append(step.Expectation.ErrorTypeMatchesThresholds, &TypeMatchesThreshold{
		IsAtLeast:  true,
		RegExp:     re.String(),
		Percentage: percentage,
	})
	return step
}

// ExpectErrorTypeMatchesPercentageAtMost sets the expectation of the error type count (of the received error types) for the given step to be at most the given value.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectErrorTypeMatchesPercentageAtMost(re *regexp.Regexp, percentage float64) *Step {
	step.Expectation.ErrorTypeMatchesThresholds = append(step.Expectation.ErrorTypeMatchesThresholds, &TypeMatchesThreshold{
		IsAtLeast:  false,
		RegExp:     re.String(),
		Percentage: percentage,
	})
	return step
}

// ExpectTimeoutTypeMatchesPercentageAtLeast sets the expectation of the timeout type count (of the received timeout types) for the given step to be at least the given value.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectTimeoutTypeMatchesPercentageAtLeast(re *regexp.Regexp, percentage float64) *Step {
	step.Expectation.TimeoutTypeMatchesThresholds = append(step.Expectation.TimeoutTypeMatchesThresholds, &TypeMatchesThreshold{
		IsAtLeast:  true,
		RegExp:     re.String(),
		Percentage: percentage,
	})
	return step
}

// ExpectTimeoutTypeMatchesPercentageAtMost sets the expectation of the timeout type count (of the received timeout types) for the given step to be at most the given value.
//
// When invoked multiple times, only the expectation value when archiving the step's stats for the first time is used
// (i.e. subsequent invocations post-archive are silently ignored).
func (step *Step) ExpectTimeoutTypeMatchesPercentageAtMost(re *regexp.Regexp, percentage float64) *Step {
	step.Expectation.TimeoutTypeMatchesThresholds = append(step.Expectation.TimeoutTypeMatchesThresholds, &TypeMatchesThreshold{
		IsAtLeast:  false,
		RegExp:     re.String(),
		Percentage: percentage,
	})
	return step
}

func addStatusCodeCheck(step *Step, statusCode int, percentage float64, isAtLeast bool) {
	expectation := &StatusCodeExpectation{
		IsAtLeast:  isAtLeast,
		StatusCode: statusCode,
		Percentage: percentage,
	}
	step.Expectation.StatusCodeThresholds = append(step.Expectation.StatusCodeThresholds, expectation)
}

type Request struct {
	Step       *Step
	Method     string
	URL        string
	Disabled   bool
	Raw        bool
	User       *User
	Headers    map[string]string
	Cookies    map[string]string
	FormParams map[string]string
	Timeout    time.Duration
	Body       *io.Reader
	Request    *http.Request
}

func (req *Request) SetBody(body *io.Reader) *Request {
	req.Body = body
	return req
}

func (req *Request) SetHeader(key, value string) *Request {
	if req.Headers == nil {
		req.Headers = make(map[string]string)
	}
	req.Headers[key] = value
	return req
}

func (req *Request) SetCookie(key, value string) *Request {
	if req.Cookies == nil {
		req.Cookies = make(map[string]string)
	}
	req.Cookies[key] = value
	return req
}

func (req *Request) SetFormParam(key, value string) *Request {
	if req.FormParams == nil {
		req.FormParams = make(map[string]string)
	}
	req.FormParams[key] = value
	if _, ok := req.Headers["Content-Type"]; !ok {
		req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
	}
	return req
}

func (req *Request) SendWithoutTimeout() *Response {
	return sendRequest(req)
}

func (req *Request) SendWithTimeout(timeout time.Duration) *Response {
	req.Timeout = timeout
	return sendRequest(req)
}

func sendRequest(req *Request) *Response {
	if !req.Raw {
		var r *http.Request
		var err error
		if len(req.FormParams) > 0 {
			if req.Body != nil {
				LogWarning("Custom form post used but standard form params provided")
			}
			formParams := url.Values{}
			for k, v := range req.FormParams {
				formParams.Set(k, v)
			}
			r, err = http.NewRequest(req.Method, req.URL, strings.NewReader(formParams.Encode()))
		} else {
			if req.Body == nil {
				r, err = http.NewRequest(req.Method, req.URL, nil)
			} else {
				r, err = http.NewRequest(req.Method, req.URL, *req.Body)
			}
		}
		req.Request = r
		CheckErrAndLogError(err, "unable to send request")
	}
	return req.User.executeRequestWithTracing(req)
}

func (step *Step) Request(method, url string) *Request {
	request := &Request{}
	if step.User.Disabled {
		request.Disabled = true
		return request
	}
	request.Method = method
	request.URL = url
	return fillRequest(request, step)
}

func (step *Step) RequestRawFromFile(targetURL string, filename string) *Request {
	f, err := os.Open(filename)
	CheckErrAndLogError(err, "unable to read raw request file")
	defer f.Close()
	return step.RequestRaw(targetURL, bufio.NewReader(f))
}

// NOTE: You (caller) need to set Content-Length explicitly in raw request input correctly
// AND don't set Accept-Encoding header, so that it will automatically added by transport and then automatically decompressed:
// see https://stackoverflow.com/questions/13130341/reading-gzipped-http-response-in-go
func (step *Step) RequestRaw(targetURL string, buf *bufio.Reader) *Request {
	request := &Request{}
	if step.User.Disabled {
		request.Disabled = true
		return request
	}
	//buf := bufio.NewReader(bytes.NewReader(raw))
	req, err := http.ReadRequest(buf)
	CheckErrAndLogError(err, "unable to read raw request buffer")
	// https://stackoverflow.com/questions/19595860/http-request-requesturi-field-when-making-request-in-go
	// We can't have this set. And it only contains "/pkg/net/http/" anyway
	req.RequestURI = ""
	// Since the req.URL will not have all the information set,
	// such as protocol scheme and host, we create a new URL
	u, err := url.Parse(targetURL)
	CheckErrAndLogError(err, "unable to parse raw request url")
	req.URL = u
	request.Request = req
	request.Raw = true
	return fillRequest(request, step)
}

func fillRequest(request *Request, step *Step) *Request {
	request.Step = step
	request.User = step.User
	return request
}

func addHeaders(req *http.Request, reqHeaders map[string]string) {
	for k, v := range reqHeaders {
		req.Header.Set(k, v)
	}
}

func addCookies(req *http.Request, reqCookies map[string]string) {
	for k, v := range reqCookies {
		req.AddCookie(&http.Cookie{
			Name:  k,
			Value: v,
		})
	}
}

func (user *User) executeRequestWithTracing(request *Request) *Response {
	addHeaders(request.Request, request.Headers)
	addCookies(request.Request, request.Cookies)
	if AddScenarioStepHeader {
		request.Request.Header.Set("Goverrun-Scenario-Step", user.Scenario+": "+request.Step.Name)
	}
	if AddUserLoopHeader {
		request.Request.Header.Set("Goverrun-User-Loop", strconv.Itoa(user.CurrentUser)+"/"+strconv.Itoa(user.CurrentLoop))
	}
	rsp := &Response{
		Scenario:   user.Scenario,
		Step:       request.Step,
		RequestURL: request.Request.URL.String(),
		Timestamps: &Timestamps{},
	}
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: rsp.gotFirstResponseByte,
		WroteRequest:         rsp.wroteRequest,
		/*
			GotConn: rsp.gotConn,
			DNSStart:             rsp.dnsStart,
			DNSDone:              rsp.dnsDone,
			TLSHandshakeStart:    rsp.tlsHandshakeStart,
			TLSHandshakeDone:     rsp.tlsHandshakeDone,
			ConnectStart:         rsp.connectStart,
			ConnectDone:          rsp.connectDone,
		*/
	}

	// call all registered request interceptors
	for _, fn := range requestInterceptors {
		fn(user, request.Request)
	}

	if verbose {
		user.printStep(request.Step)
		/*
			dump, _ := httputil.DumpRequest(req, true)
			fmt.Println(">>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>")
			fmt.Println(string(dump))
			fmt.Println("<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<")
		*/
	}

	rsp.Timestamps.Start = time.Now()
	user.HttpClient.Timeout = request.Timeout
	responseOfCall, err := user.HttpClient.Do(request.Request.WithContext(httptrace.WithClientTrace(request.Request.Context(), trace)))
	rsp.Timestamps.Done = time.Now()
	// https://stackoverflow.com/questions/48077098/getting-ttfb-time-to-first-byte-value-in-golang
	// https://blog.golang.org/http-tracing
	// https://github.com/davecheney/httpstat
	if err != nil {
		netErr, ok := err.(net.Error) // here "ok" is simply false when the type assertion failed (i.e. other type of error)
		if ok && netErr.Timeout() && rsp.Error == nil {
			rsp.Timeout = err
		} else if rsp.Timeout == nil { // other type of error
			rsp.Error = err
			if verbose {
				log.Println(">>>>>>>>>>>>>>>>>>>>>>")
				log.Println(err)
				log.Println("<<<<<<<<<<<<<<<<<<<<<<")
			}
		}
	}
	var (
		respBody   []byte
		statusCode int
		status     string
		headerSize int
	)
	if responseOfCall != nil && responseOfCall.Body != nil {
		defer responseOfCall.Body.Close()
		respBody, err = io.ReadAll(responseOfCall.Body)
		if err != nil {
			netErr, ok := err.(net.Error) // here "ok" is simply false when the type assertion failed (i.e. other type of error)
			if ok && netErr.Timeout() && rsp.Error == nil {
				rsp.Timeout = err
			} else if rsp.Timeout == nil { // other type of error
				rsp.Error = err
				if verbose {
					log.Println(">>>>>>>>>>>>>>>>>>>>>>")
					log.Println(err)
					log.Println("<<<<<<<<<<<<<<<<<<<<<<")
				}
			}
		}
		headerSize = HeaderSize(responseOfCall.Header)
		statusCode = responseOfCall.StatusCode
		status = responseOfCall.Status
	}
	rsp.StatusCode = statusCode
	rsp.Status = status
	if responseOfCall != nil {
		rsp.FinalURL = responseOfCall.Request.URL.String()
	}
	rsp.Body = respBody
	rsp.RequestSize = HeaderSize(request.Request.Header) + int(request.Request.ContentLength)
	rsp.ResponseSize = headerSize + len(respBody)
	/*
		if detailsWriter != nil {
			err := detailsWriter.writeArchiveEntry(rsp.archiveEntry())
			if err != nil {
				panic(err)
			}
		}
	*/

	// return response
	return rsp
}

type Timestamps struct {
	Start                time.Time
	WroteRequest         time.Time
	GotFirstResponseByte time.Time
	Done                 time.Time
	/*
		GotConn     time.Time
		ConnReused           bool
		DNSStart, DNSDone                   time.Time
		TLSHandshakeStart, TLSHandshakeDone time.Time
		ConnectStart, ConnectDone           time.Time
	*/
}

type Response struct {
	Scenario        string
	Step            *Step
	RequestSize     int
	ResponseSize    int
	RequestURL      string
	FinalURL        string
	StatusCode      int
	Status          string
	Timestamps      *Timestamps
	Timeout         error
	Error           error
	AssertionFailed string
	Body            []byte
	// internal
	archived bool
}

type StepEntry struct {
	Scenario                 string
	Timestamps               Timestamps
	Timeout                  bool
	TimeoutRootCause         string
	Error                    bool
	ErrorRootCause           string
	AssertionFailed          bool
	AssertionFailedRootCause string
	StatusCode               int
	RequestSize              int
	ResponseSize             int
}

func (response *Response) IsFailed() bool {
	return len(response.AssertionFailed) > 0
}

func (response *Response) MarkAsFailed(message string) {
	response.AssertionFailed = message
	if verbose {
		log.Println(message)
	}
}

func (response *Response) Assert(fn func(response *Response) (message string, ok bool)) *Response {
	if response.ConsideredUnsuccessful() {
		return response // earlier checked assertion already failed or error or timeout happened
	}
	message, ok := fn(response)
	if !ok {
		response.MarkAsFailed(fmt.Sprint("assertion of function on response failed ", message))
	}
	return response
}

func (response *Response) AssertStatusCode(statusCode int) *Response {
	if response.ConsideredUnsuccessful() {
		return response // earlier checked assertion already failed or error or timeout happened
	}
	ok := response.StatusCode == statusCode
	if !ok {
		response.MarkAsFailed(fmt.Sprint("assertion of status code failed: got ", response.StatusCode, " want ", statusCode))
	}
	return response
}

func (response *Response) AssertStatus(status string) *Response {
	if response.ConsideredUnsuccessful() {
		return response // earlier checked assertion already failed or error or timeout happened
	}
	ok := response.Status == status
	if !ok {
		response.MarkAsFailed(fmt.Sprint("assertion of status failed: got ", response.Status, " want ", status))
	}
	return response
}

func (response *Response) AssertBodyMatches(re *regexp.Regexp) *Response {
	if response.ConsideredUnsuccessful() {
		return response // earlier checked assertion already failed or error or timeout happened
	}
	ok := re.Match(response.Body)
	if !ok {
		response.MarkAsFailed(fmt.Sprint("assertion of body content failed (response body did not match expected regular expression): ", re))
	}
	return response
}

func (response *Response) AssertBodyContains(s string) *Response {
	if response.ConsideredUnsuccessful() {
		return response // earlier checked assertion already failed or error or timeout happened
	}
	ok := strings.Contains(string(response.Body), s)
	if !ok {
		response.MarkAsFailed(fmt.Sprint("assertion of body content failed (response body did not contain expected value): ", s))
	}
	return response
}

func (response *Response) AssertBodySizeAtLeast(bytes int) *Response {
	if response.ConsideredUnsuccessful() {
		return response // earlier checked assertion already failed or error or timeout happened
	}
	length := len(response.Body)
	ok := length >= bytes
	if !ok {
		response.MarkAsFailed(fmt.Sprint("assertion of body size failed (response body was shorter than expected value): got ", length, " want >=", bytes))
	}
	return response
}

func (response *Response) AssertBodySizeAtMost(bytes int) *Response {
	if response.ConsideredUnsuccessful() {
		return response // earlier checked assertion already failed or error or timeout happened
	}
	length := len(response.Body)
	ok := length <= bytes
	if !ok {
		response.MarkAsFailed(fmt.Sprint("assertion of body size failed (response body was longer than expected value): got ", length, " want <=", bytes))
	}
	return response
}

func (response *Response) ConsideredUnsuccessful() bool {
	return len(response.AssertionFailed) > 0 || response.Error != nil || response.Timeout != nil
}

func (response *Response) ArchiveStats() *Response {
	// histogram tracking
	if len(folder) > 0 && !response.archived {
		histogramLock.Lock()
		step, expectation := response.Step.Name, response.Step.Expectation
		if _, exists := stepHistogramWriters[step]; !exists {
			stepFilename := filepath.Join(folder, fmt.Sprintf(stepDefaultFilenamePattern, len(stepHistogramWriters)+1))
			var err error
			stepFile, err := os.Create(stepFilename)
			CheckErrAndLogError(err, "unable to create step file")
			stepGZW := gzip.NewWriter(stepFile)
			stepHistogramWriters[step] = &stepGobWriter{
				gobWriter: gobWriter{
					file:       stepFile,
					gzw:        stepGZW,
					gobEncoder: gob.NewEncoder(stepGZW),
				},
			}
			err = stepHistogramWriters[step].writeStepNameInit(step, *expectation) // to init file with step name
			CheckErrAndLogError(err, "unable to create write step init")
		}
		shgw := stepHistogramWriters[step]
		histogramLock.Unlock()
		// here now via concurrent-safe receiver method
		err := shgw.writeStepEntry(response.stepEntry())
		CheckErrAndLogError(err, "unable to write step entry")
		response.archived = true
	}
	return response
}

/* check if connection and tls handshake values are correct (when connections are reused?)

func (response *Response) gotConn(info httptrace.GotConnInfo) {
	response.Timestamps.GotConn = time.Now()
	response.Timestamps.ConnReused = info.Reused
}

func (response *Response) dnsStart(dsi httptrace.DNSStartInfo) {
	response.Timestamps.DNSStart = time.Now()
}

func (response *Response) dnsDone(ddi httptrace.DNSDoneInfo) {
	response.Timestamps.DNSDone = time.Now()
}

func (response *Response) tlsHandshakeStart() {
	response.Timestamps.TLSHandshakeStart = time.Now()
}

func (response *Response) tlsHandshakeDone(cs tls.ConnectionState, err error) {
	response.Timestamps.TLSHandshakeDone = time.Now()
}

func (response *Response) connectStart(network, addr string) {
	response.Timestamps.ConnectStart = time.Now()
}

func (response *Response) connectDone(network, addr string, err error) {
	response.Timestamps.ConnectDone = time.Now()
}
*/
func (response *Response) gotFirstResponseByte() {
	// for calculating the time from start to first byte (TTFB)
	response.Timestamps.GotFirstResponseByte = time.Now()
}

func (response *Response) wroteRequest(info httptrace.WroteRequestInfo) {
	// for more precise calculating of timing when request sent: Request-Sending-Duration (RSDU)
	response.Timestamps.WroteRequest = time.Now()
}

func (response *Response) TotalDuration() time.Duration {
	return response.Timestamps.Done.Sub(response.Timestamps.Start)
}

/*
func (response *Response) archiveEntry() ArchiveEntry {
	archive := ArchiveEntry{
		ScenarioTitle: response.Scenario.Title,
		Step:          response.Step,
		Timeout:       response.Timeout,
		Error:         response.Error != nil,
		StatusCode:    response.StatusCode,
		Timestamps:    *response.Timestamps,
	}
	if response.Error != nil {
		archive.ErrorMsg = response.Error.Error()
	}
	return archive
}
*/

func (response *Response) stepEntry() *StepEntry {
	stepEntry := &StepEntry{
		Scenario:                 response.Scenario,
		Timeout:                  response.Timeout != nil,
		TimeoutRootCause:         UnwrapDeepestError(response.Timeout),
		Error:                    response.Error != nil,
		ErrorRootCause:           UnwrapDeepestError(response.Error),
		AssertionFailed:          len(response.AssertionFailed) > 0,
		AssertionFailedRootCause: response.AssertionFailed,
		StatusCode:               response.StatusCode,
		Timestamps:               *response.Timestamps,
		RequestSize:              response.RequestSize,
		ResponseSize:             response.ResponseSize,
	}
	const logErrorDetailsForDebugging = false
	if logErrorDetailsForDebugging {
		if response.Error != nil {
			fmt.Printf("%d %T -> %s -> %s\n", response.StatusCode, response.Error, response.Error.Error(), stepEntry.ErrorRootCause)
		}
	}
	return stepEntry
}

func (stats *Timestamps) TotalDuration() (d time.Duration, completed bool) {
	if stats.Done.IsZero() {
		return 0, false
	}
	res := stats.Done.Sub(stats.Start)
	if res < 0 {
		res = 0
	}
	return res, true
}

func (stats *Timestamps) TimeToFirstByte(afterRequestSent bool) (d time.Duration, completed bool) {
	start := stats.Start
	if afterRequestSent {
		start = stats.WroteRequest
	}
	if stats.GotFirstResponseByte.IsZero() || stats.WroteRequest.IsZero() {
		return 0, false
	}
	res := stats.GotFirstResponseByte.Sub(start)
	if res < 0 {
		res = 0
	}
	return res, true
}

func (response *Response) PrintStats(w io.Writer) *Response {
	printLock.Lock()
	defer printLock.Unlock()
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "------------------------------------------------------------------")
	_, _ = fmt.Fprintln(w, response.RequestURL)
	_, _ = fmt.Fprintln(w, response.Status)
	_, _ = fmt.Fprintln(w, "Total-Duration:", durationMeasurement(response.Timestamps.TotalDuration()))
	_, _ = fmt.Fprintln(w, "Time-to-First-Byte:", durationMeasurement(response.Timestamps.TimeToFirstByte(false)))
	_, _ = fmt.Fprintln(w, "Time-to-First-Byte (after Request-Sent):", durationMeasurement(response.Timestamps.TimeToFirstByte(true)))
	/*
		_, _ = fmt.Fprintln(w,"Connection reused:", response.Timestamps.ConnReused)
		_, _ = fmt.Fprintln(w,"DNS Lookup:", response.Timestamps.DNSDone.Sub(response.Timestamps.DNSStart))
		_, _ = fmt.Fprintln(w,"Connect:", response.Timestamps.ConnectDone.Sub(response.Timestamps.ConnectStart))
		_, _ = fmt.Fprintln(w,"TLS Handshake:", response.Timestamps.TLSHandshakeDone.Sub(response.Timestamps.TLSHandshakeStart))
		_, _ = fmt.Fprintln(w,"Time To First Byte (from TLS Handshake Done):", response.Timestamps.GotFirstResponseByte.Sub(response.Timestamps.TLSHandshakeDone))
		_, _ = fmt.Fprintln(w,"Time To First Byte (from Connect Done):", response.Timestamps.GotFirstResponseByte.Sub(response.Timestamps.ConnectDone))
		_, _ = fmt.Fprintln(w,"Time To First Byte (from DNS Lookup Done):", response.Timestamps.GotFirstResponseByte.Sub(response.Timestamps.DNSDone))
	*/
	_, _ = fmt.Fprintln(w, "------------------------------------------------------------------")
	_, _ = fmt.Fprintln(w)
	return response
}

func (response *Response) ExtractCaptureGroup(re *regexp.Regexp) string {
	s := re.FindStringSubmatch(string(response.Body))
	if len(s) > 0 {
		return html.UnescapeString(s[1])
	}
	return ""
}

func (response *Response) ExtractStringFromJSON(expression string) string {
	res := response.EvalExpressionOnJSON(expression)
	if res == nil {
		return ""
	} else {
		return res.(string)
	}
}

func (response *Response) ExtractSliceFromJSON(expression string) (result []string) {
	res := response.EvalExpressionOnJSON(expression)
	if res != nil {
		result = res.([]string)
	}
	return
}

func (response *Response) EvalExpressionOnJSON(expression string) interface{} {
	builder := gval.Full(jsonpath.PlaceholderExtension())
	// see https://goessner.net/articles/JsonPath/
	// and https://godoc.org/github.com/PaesslerAG/jsonpath#example-package--Gval
	// expressions like {#1: $..[?@.ping && @.speed > 100].name}
	// or simpler examples like $.store.book[*].author
	// or simpler examples like $["user-agent"]
	path, err := builder.NewEvaluable(expression)
	if err != nil {
		panic(err)
	}
	result, err := path(context.Background(), DynamicJSON(response.Body))
	if err != nil {
		LogError(err) // TODO track it as unable to extract (i.e. not found?)
	}
	return result
}

type Scenario struct {
	Title, Description string
	Runner             func(user *User)
	LoadConfig         LoadConfig
	Ignored            bool
	ExecutionCount     uint64
}
type RandomInterval struct {
	Min, Max time.Duration
}

func (ri RandomInterval) String() string {
	return fmt.Sprint("random interval between ", ri.Min, " and ", ri.Max)
}

type LoadConfig struct { // TODO add also cool-down period to slowly reduce users to see at the end if system is responsive again
	StartDelay                RandomInterval
	LoopingUsers              int
	LoopDelay                 RandomInterval
	RampUp, Plateau, RampDown time.Duration
	ClearCookieJarOnEveryLoop bool
}

// safeTracker is safe to use concurrently.
type safeTracker struct {
	lock     sync.RWMutex
	counters map[string]int
}

// Inc increments the counter for the given key and returns the new value.
func (sk *safeTracker) Inc(key string) int {
	sk.lock.Lock()
	defer sk.lock.Unlock()
	sk.counters[key]++
	return sk.counters[key]
}

// Dec decrements the counter for the given key and returns the new value.
func (sk *safeTracker) Dec(key string) int {
	sk.lock.Lock()
	defer sk.lock.Unlock()
	sk.counters[key]--
	return sk.counters[key]
}

// Value returns the current value of the counter for the given key.
func (sk *safeTracker) Value(key string) int {
	sk.lock.RLock()
	defer sk.lock.RUnlock()
	return sk.counters[key]
}

// Values returns the map of all values.
func (sk *safeTracker) Values() map[string]int {
	sk.lock.RLock()
	defer sk.lock.RUnlock()
	return sk.counters
}

type gobWriter struct {
	file       *os.File
	gzw        *gzip.Writer
	gobEncoder *gob.Encoder
}

// stepGobWriter is safe to use concurrently.
type stepGobWriter struct {
	lock sync.Mutex
	gobWriter
}

func (sgw *stepGobWriter) writeStepNameInit(name string, expectation Expectation) error {
	sgw.lock.Lock()
	defer sgw.lock.Unlock()
	err := sgw.gobEncoder.Encode(1) // file format version (to be compatible with updated content later)
	if err != nil {
		return err
	}
	err = sgw.gobEncoder.Encode(name)
	if err != nil {
		return err
	}
	return sgw.gobEncoder.Encode(expectation)
}

func (sgw *stepGobWriter) writeStepEntry(stepEntry *StepEntry) error {
	sgw.lock.Lock()
	defer sgw.lock.Unlock()
	return sgw.gobEncoder.Encode(*stepEntry)
}

// scenariosGobWriter is NOT safe to be used concurrently and doesn't need to (simply not necessary to use concurrently).
type scenariosGobWriter struct {
	gobWriter
}

func (sgw *scenariosGobWriter) writeScenarios(scenarios map[string]*Scenario) error {
	err := sgw.gobEncoder.Encode(1) // file format version (to be compatible with updated content later)
	if err != nil {
		return err
	}
	hn, err := os.Hostname()
	if err != nil {
		return err
	}
	env := Environment{
		Hostname: hn,
		Start:    time.Now(),
	}
	err = sgw.gobEncoder.Encode(env)
	if err != nil {
		return err
	}
	return sgw.gobEncoder.Encode(scenarios)
}

func init() { // special func init() is called automatically and only once (before the other special func main() which is the entry point)
	rand.Seed(time.Now().UnixNano())
	// handle CTRL-C
	go func() {
		sigchan := make(chan os.Signal)
		signal.Notify(sigchan, os.Interrupt)
		<-sigchan
		LogInfo("Goverrun stopped")
		// do last actions and wait for all write operations to end
		writeSummaryAndCloseFiles()
		os.Exit(0)
	}()
}

var (
	closed    = false
	closeLock sync.Mutex
)

func writeSummaryAndCloseFiles() {
	closeLock.Lock()
	defer closeLock.Unlock()
	if !closed {
		if scenariosWriter != nil {
			err := scenariosWriter.writeScenarios(scenarios)
			if err != nil {
				panic(err)
			}
			// close everything properly
			err = scenariosWriter.gzw.Close()
			if err != nil {
				panic(err)
			}
			scenariosWriter.file.Close()
			LogInfo("Scenarios written to:", scenariosWriter.file.Name())
		}
		for step, stepWriter := range stepHistogramWriters {
			stepWriter.lock.Lock()
			defer stepWriter.lock.Unlock()
			// close everything properly
			err := stepWriter.gzw.Close()
			if err != nil {
				panic(err)
			}
			stepWriter.file.Close()
			LogInfof("Step '%s' written to: %s\n", step, stepWriter.file.Name())
		}
		closed = true
	}
}

func AddScenario(scenario *Scenario) error {
	if scenario.LoadConfig.LoopingUsers <= 0 {
		panic("zero or negative LoopingUsers")
	}
	if scenario.LoadConfig.RampUp < 0 {
		panic("negative RampUp")
	}
	if scenario.LoadConfig.RampDown < 0 {
		panic("negative RampDown")
	}
	if scenario.LoadConfig.Plateau < 0 {
		panic("negative Plateau")
	}
	if _, exists := scenarios[scenario.Title]; exists {
		return fmt.Errorf("scenario already exists '%s'", scenario.Title)
	}
	scenarios[scenario.Title] = scenario
	return nil
}

func DefaultLoadConfigFromArgs() LoadConfig {
	return LoadConfig{
		StartDelay: RandomInterval{
			Min: 0 * time.Millisecond,
			Max: 0 * time.Millisecond,
		},
		LoopingUsers: *CommandlineArgs.Run.LoopingUsers,
		LoopDelay: RandomInterval{
			Min: 0 * time.Millisecond,
			Max: 0 * time.Millisecond,
		},
		RampUp:                    time.Duration(*CommandlineArgs.Run.RampUpSeconds) * time.Second,
		Plateau:                   time.Duration(*CommandlineArgs.Run.PlateauSeconds) * time.Second,
		RampDown:                  time.Duration(*CommandlineArgs.Run.RampDownSeconds) * time.Second,
		ClearCookieJarOnEveryLoop: true,
	}
}

func Run(outputFolder string, verboseLogs bool) {
	defer writeSummaryAndCloseFiles()

	// log every 10 seconds (via ticker) the current state
	logTicker := time.NewTicker(10 * time.Second) // TODO make number of seconds to tick (10 default) configurable via Run() call?
	logTickerDone := make(chan bool)
	go func() {
		for {
			select {
			case <-logTickerDone:
				return
			case _ /*t*/ = <-logTicker.C:
				//fmt.Println("Tick at", t)
				for _, scenario := range scenarios {
					LogInfof("Looping users of scenario '%s': %d\n", scenario.Title, currentLoopingUsers.Value(scenario.Title))
				}
			}
		}
	}()
	// end ticker when this func exits
	defer func() {
		logTicker.Stop()
		logTickerDone <- true
	}()

	folder = outputFolder
	indexFilename := ""
	if len(folder) > 0 {
		if _, err := os.Stat(folder); os.IsNotExist(err) {
			err := os.Mkdir(folder, 0755)
			if err != nil {
				panic(err)
			}
		}
		indexFilename = filepath.Join(folder, scenariosDefaultFilename)
	}
	verbose = verboseLogs
	if len(indexFilename) > 0 {
		var err error
		statsFile, err := os.Create(indexFilename)
		if err != nil {
			panic(err)
		}
		statsGZW := gzip.NewWriter(statsFile)
		scenariosWriter = &scenariosGobWriter{
			gobWriter: gobWriter{
				file:       statsFile,
				gzw:        statsGZW,
				gobEncoder: gob.NewEncoder(statsGZW),
			},
		}
	}
	var wg sync.WaitGroup
	for _, scenario := range scenarios {
		if scenario.Ignored {
			continue
		}
		LogInfo("Running scenario:", scenario.Title)
		wg.Add(1)
		go func(scenario *Scenario) {
			defer wg.Done()
			scenario.ExecutionCount = 0
			time.Sleep(RandomDuration(scenario.LoadConfig.StartDelay.Min, scenario.LoadConfig.StartDelay.Max))
			end := time.Now().Add(scenario.LoadConfig.RampUp).Add(scenario.LoadConfig.Plateau).Add(scenario.LoadConfig.RampDown)
			rampDownPhaseEntry := end.Add(-scenario.LoadConfig.RampDown)
			rampDownStep := int64(scenario.LoadConfig.RampDown) / int64(scenario.LoadConfig.LoopingUsers)
			for currentUserCount := 1; currentUserCount <= scenario.LoadConfig.LoopingUsers; currentUserCount++ {
				rampDownCutoffForCurrentUser := rampDownPhaseEntry.Add(time.Duration(int64(currentUserCount) * rampDownStep))
				wg.Add(1)
				go func(scenario *Scenario, currentUser int) {
					defer wg.Done()
					currentLoopingCount := currentLoopingUsers.Inc(scenario.Title)
					if verbose {
						LogInfof("Ramp-up: adding looping user to scenario '%s': %d looping\n", scenario.Title, currentLoopingCount)
					}
					user := User{
						Scenario:    scenario.Title,
						CurrentUser: currentUser,
						HttpClient: &http.Client{
							Transport: NewRoundTripperWrapper(SkipCertificateValidation, Proxy),
						},
						Data: make(map[string]interface{}),
					}
					for time.Now().Before(end) {
						user.CurrentLoop++
						if user.HttpClient.Jar == nil || scenario.LoadConfig.ClearCookieJarOnEveryLoop {
							jar, err := cookiejar.New(nil)
							CheckErrAndLogError(err, "unable to initialize cookie jar")
							user.HttpClient.Jar = jar
						}
						scenario.Runner(&user)
						atomic.AddUint64(&scenario.ExecutionCount, 1)
						if time.Now().After(rampDownCutoffForCurrentUser) {
							newCount := currentLoopingUsers.Dec(scenario.Title)
							user.Disabled = true
							if verbose {
								LogInfof("Ramp-down: removing looping user from scenario '%s': %d looping\n", scenario.Title, newCount)
							}
							break
						}
						user.ThinkTime(RandomDuration(scenario.LoadConfig.LoopDelay.Min, scenario.LoadConfig.LoopDelay.Max))
					}
				}(scenario, currentUserCount) // to not capture loop variables in goroutine the undesired way
				// sleep due to ramp-up time
				if scenario.LoadConfig.LoopingUsers > 1 {
					time.Sleep(time.Duration(int64(scenario.LoadConfig.RampUp) / int64(scenario.LoadConfig.LoopingUsers-1)))
				}
			}
		}(scenario) // to not capture loop variables in goroutine the undesired way
	}
	wg.Wait()
}
