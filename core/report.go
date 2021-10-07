package goverrun

import (
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"github.com/aybabtme/uniplot/histogram"
	"github.com/montanaflynn/stats"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultLanguage = "en"

var localizationPrinter = message.NewPrinter(language.Make(defaultLanguage))

type Stats struct {
	Title               string
	HasUnmetExpectation bool

	Counts                                 Counts
	StatusCodes                            map[int]int
	FailureTypes, ErrorTypes, TimeoutTypes map[string]int
	RequestBytes, ResponseBytes            uint64

	TTFB, TARS, TRRT                                                []float64 `json:"-"` // ignore in JSON as instead of raw-data we want the analyzed result data (AnalyzedResults)
	TimeToFirstByte, TimeAfterRequestSent, TotalRequestResponseTime AnalyzedResults
	Expectation                                                     Expectation
}

type AnalyzedResults struct {
	Stats       ResultStats
	Percentiles ResultPercentiles
	Histogram   ResultHistogram
}

type ResultStats struct {
	Minimum, Maximum, Mean, Median, StandardDeviation, FirstQuartile, ThirdQuartile, InterQuartileRange, Midhinge, Trimean float64 // all in nanoseconds
}
type ResultPercentiles struct {
	P80p00, P90p00, P95p00, P99p00, P99p90, P99p99 float64 // all in nanoseconds
}
type ResultHistogram struct {
	Buckets []HistogramBucket
}
type HistogramBucket struct {
	Min, Max float64 // all in nanoseconds
	Count    int
}

type Report struct {
	Environment                   Environment
	ScenariosByClient             map[string]map[string]Scenario
	StepNamesInChronologicalOrder []string
	StatsByStep                   map[string]Stats
	ExampleByStep                 map[string]string
	OverallStats                  Stats
}

// GenerateResultsReport analyzes and prints the loadtest results.
// All index and step files below the given folder are analyzed.
// To merge multiple distributed collected results: place them as subfolders below the given folder.
func GenerateResultsReport(reportPath string) (unmetExpectation bool) {
	if len(reportPath) == 0 {
		return
	}
	var (
		// collect scenarios
		scenariosByClient = make(map[string]map[string]Scenario)

		// collect step files
		stepFiles                     = make(map[string][]string)
		stepNamesInChronologicalOrder []string

		// collect overall total step stats
		overallStatusCodes                                          = make(map[int]int)
		overallFailureTypes, overallErrorTypes, overallTimeoutTypes = make(map[string]int), make(map[string]int), make(map[string]int)
		overallCounts                                               Counts
		overallTTFB, overallPARS, overallTODU                       []float64
		recordingEnv                                                Environment

		// collect traffic amounts
		overallRequestBytes, overallResponseBytes uint64

		// Report collector
		report Report
	)

	report = Report{
		ScenariosByClient: make(map[string]map[string]Scenario),
		StatsByStep:       make(map[string]Stats),
		ExampleByStep:     make(map[string]string),
	}

	// walk the folder to collect any (distributed) result files
	err := filepath.Walk(reportPath,
		func(path string, fileInfo os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			//fmt.Println(path, info.Size())
			if fileInfo.IsDir() {
				return nil
			}
			if fileInfo.Name() == scenariosDefaultFilename {
				// scenarios file
				scenariosFile, err := os.Open(path)
				panicOnErr(err)
				defer scenariosFile.Close()
				var dec *gob.Decoder
				gzr, err := gzip.NewReader(scenariosFile)
				panicOnErr(err)
				dec = gob.NewDecoder(gzr)
				// parse file format version
				var fileFormatVersion int
				if err := dec.Decode(&fileFormatVersion); err != nil {
					return err
				}
				// parse scenario environment meta data
				if err := dec.Decode(&recordingEnv); err != nil {
					return err
				}
				parsedScenarios := make(map[string]Scenario)
				if err := dec.Decode(&parsedScenarios); err != nil {
					return err
				}
				// add them
				scenariosRunner := strings.Replace(filepath.Dir(path), reportPath+"/", "", 1)
				scenariosByClient[scenariosRunner] = parsedScenarios
			} else {
				match, err := filepath.Match(stepDefaultFilenameMatch, fileInfo.Name())
				panicOnErr(err)
				if match {
					LogInfo("Parsing step file to create histogram:", path)
					// just parse the step name (for collecting)
					recordedStepFile, err := os.Open(path)
					panicOnErr(err)
					gzr, err := gzip.NewReader(recordedStepFile)
					panicOnErr(err)
					dec := gob.NewDecoder(gzr)
					// parse file format version
					var fileFormatVersion int
					if err := dec.Decode(&fileFormatVersion); err != nil {
						return err
					}
					// parse the step name
					var parsedStepName string
					if err := dec.Decode(&parsedStepName); err != nil {
						return err
					}
					// close it
					recordedStepFile.Close()
					// add it
					if _, exists := stepFiles[parsedStepName]; !exists {
						stepNamesInChronologicalOrder = append(stepNamesInChronologicalOrder, parsedStepName)
					}
					stepFiles[parsedStepName] = append(stepFiles[parsedStepName], path)
				}
			}
			return nil
		})
	panicOnErr(err)
	report.ScenariosByClient = scenariosByClient
	report.Environment = recordingEnv

	examples := make(map[string]string)
	// parse details & print step results
	report.StepNamesInChronologicalOrder = stepNamesInChronologicalOrder
	for i, stepName := range stepNamesInChronologicalOrder {
		stepStatusCodes := make(map[int]int)
		stepFailureTypes, stepErrorTypes, stepTimeoutTypes := make(map[string]int), make(map[string]int), make(map[string]int)
		var allStepCounts Counts
		var stepTTFB, stepPARS, stepTODU []float64
		var stepRequestBytes, stepResponseBytes uint64
		var latestExpectation Expectation
		for j, stepFile := range stepFiles[stepName] { // could be multiple step-files per step due to merging of directories from distributed runs
			// parse step file
			allCounts, parsedStepExpectation,
				valuesTTFB, valuesTTFBRS, valuesTODU,
				statusCodes, failureTypes, errorTypes, timeoutTypes,
				_, _, _, _, //valuesPerMinuteBlockTTFB, valuesPerMinuteBlockPARS, valuesPerMinuteBlockTODU, countsPerMinuteBlock,
				requestBytes, responseBytes,
				example := parseStepFile(stepFile)

			if j == 0 {
				examples[stepName] = example
			}

			// use the expectation from the latest step file parsed (when multiple are parsed)
			latestExpectation = parsedStepExpectation

			// track results
			stepRequestBytes += requestBytes
			stepResponseBytes += responseBytes
			stepTTFB = append(stepTTFB, valuesTTFB...)
			stepPARS = append(stepPARS, valuesTTFBRS...)
			stepTODU = append(stepTODU, valuesTODU...)
			for k, v := range statusCodes {
				stepStatusCodes[k] += v
			}
			for k, v := range failureTypes {
				stepFailureTypes[k] += v
			}
			for k, v := range errorTypes {
				stepErrorTypes[k] += v
			}
			for k, v := range timeoutTypes {
				stepTimeoutTypes[k] += v
			}
			allStepCounts.Requests += allCounts.Requests
			allStepCounts.Timeouts += allCounts.Timeouts
			allStepCounts.Failures += allCounts.Failures
			allStepCounts.Errors += allCounts.Errors
		}

		// also track overall
		overallTTFB = append(overallTTFB, stepTTFB...)
		overallPARS = append(overallPARS, stepPARS...)
		overallTODU = append(overallTODU, stepTODU...)
		for k, v := range stepStatusCodes {
			overallStatusCodes[k] += v
		}
		for k, v := range stepFailureTypes {
			overallFailureTypes[k] += v
		}
		for k, v := range stepErrorTypes {
			overallErrorTypes[k] += v
		}
		for k, v := range stepTimeoutTypes {
			overallTimeoutTypes[k] += v
		}
		overallCounts.Requests += allStepCounts.Requests
		overallCounts.Timeouts += allStepCounts.Timeouts
		overallCounts.Failures += allStepCounts.Failures
		overallCounts.Errors += allStepCounts.Errors
		overallRequestBytes += stepRequestBytes
		overallResponseBytes += stepResponseBytes

		report.StatsByStep[stepName] = Stats{
			Counts:        allStepCounts,
			TTFB:          stepTTFB,
			TARS:          stepPARS,
			TRRT:          stepTODU,
			StatusCodes:   stepStatusCodes,
			FailureTypes:  stepFailureTypes,
			ErrorTypes:    stepErrorTypes,
			TimeoutTypes:  stepTimeoutTypes,
			RequestBytes:  stepRequestBytes,
			ResponseBytes: stepResponseBytes,
		}
		report.ExampleByStep[stepName] = examples[stepName]

		// print step results file
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("=======================================================================\nStep '%s'\n=======================================================================\n", stepName))
		statsCollected := report.StatsByStep[stepName]
		statsCollected.Title = "Step " + strconv.Itoa(i+1)
		statsCollected.Expectation = latestExpectation
		sb.WriteString("\n\n")
		sb.WriteString(analyzeExpectation(&statsCollected))
		sb.WriteString("\n")
		sb.WriteString(printDistributions(&statsCollected))
		stepFileTxt := filepath.Join(reportPath, "step-"+strconv.Itoa(i+1)+".txt")
		err = ioutil.WriteFile(stepFileTxt, []byte(sb.String()), 0644)
		CheckErrAndLogError(err, "unable to create output file")
		LogSuccess("Step text file written to:", stepFileTxt)

		// print step results as JSON
		data, _ := json.Marshal(statsCollected)
		stepFileJSON := filepath.Join(reportPath, "step-"+strconv.Itoa(i+1)+".json")
		err = ioutil.WriteFile(stepFileJSON, data, 0644)
		CheckErrAndLogError(err, "unable to create output file")
		LogSuccess("Step JSON file written to:", stepFileJSON)

		if statsCollected.HasUnmetExpectation {
			unmetExpectation = true
		}
	}

	report.OverallStats = Stats{
		Counts:        overallCounts,
		TTFB:          overallTTFB,
		TARS:          overallPARS,
		TRRT:          overallTODU,
		StatusCodes:   overallStatusCodes,
		FailureTypes:  overallFailureTypes,
		ErrorTypes:    overallErrorTypes,
		TimeoutTypes:  overallTimeoutTypes,
		RequestBytes:  overallRequestBytes,
		ResponseBytes: overallResponseBytes,
	}

	// print overall results as text
	var sb strings.Builder
	// print scenarios (by client, where client is a load generating box so that having multiple clients means running distributed load tests
	sb.WriteString("=======================================================================\nTotal over all steps\n=======================================================================\n\n")
	sb.WriteString(printDistributions(&report.OverallStats))
	sb.WriteString("\n\n\n\n")
	sb.WriteString(fmt.Sprintln("Recording environment: ", recordingEnv)) // TODO write use custom Stringer (+ also add to JSON marshalled struct)
	for client, scenariosOfClient := range scenariosByClient {
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintln("Scenarios runner:", client))
		for _, scenario := range scenariosOfClient {
			sb.WriteString(fmt.Sprintln(scenario)) // TODO write use custom Stringer (+ also add to JSON marshalled struct)
		}
	}
	scenariosFileTxt := filepath.Join(reportPath, "scenarios.txt")
	err = ioutil.WriteFile(scenariosFileTxt, []byte(sb.String()), 0644)
	CheckErrAndLogError(err, "unable to create output file")
	LogSuccess("Scenarios text file written to:", scenariosFileTxt)

	// print overall results as JSON
	report.OverallStats.Title = "Overall Results"
	report.OverallStats.HasUnmetExpectation = unmetExpectation
	data, _ := json.Marshal(report.OverallStats)
	statsFileJSON := filepath.Join(reportPath, "scenarios.json")
	err = ioutil.WriteFile(statsFileJSON, data, 0644)
	CheckErrAndLogError(err, "unable to create output file")
	LogSuccess("Scenarios JSON file written to:", statsFileJSON)
	return
}

func parseStepFile(stepFile string) (allCounts Counts, parsedStepExpectation Expectation,
	valuesTTFB, valuesPARS, valuesTODU []float64,
	statusCodes map[int]int,
	failureTypes, errorTypes, timeoutTypes map[string]int,
	valuesPerMinuteBlockTTFB, valuesPerMinuteBlockPARS, valuesPerMinuteBlockTODU [][]float64,
	countsPerMinuteBlock []Counts,
	requestBytes, responseBytes uint64,
	example string) {
	recordedStepFile, err := os.Open(stepFile)
	panicOnErr(err)
	defer recordedStepFile.Close()
	gzr, err := gzip.NewReader(recordedStepFile)
	panicOnErr(err)
	dec := gob.NewDecoder(gzr)
	// parse file format version
	var fileFormatVersion int
	if err := dec.Decode(&fileFormatVersion); err != nil {
		LogError("unable to decode file format version:", err)
	}
	// parse the step name
	var parsedStepName string
	if err := dec.Decode(&parsedStepName); err != nil {
		LogError("unable to decode step name:", err)
	}
	// parse the step expectation
	if err := dec.Decode(&parsedStepExpectation); err != nil {
		LogError("unable to decode expectation:", err)
	}
	// tracking maps
	statusCodes = make(map[int]int)
	failureTypes, errorTypes, timeoutTypes = make(map[string]int), make(map[string]int), make(map[string]int)
	// values per minute blocks
	valuesPerMinuteBlockTTFB, valuesPerMinuteBlockPARS, valuesPerMinuteBlockTODU = make([][]float64, 0), make([][]float64, 0), make([][]float64, 0)
	countsPerMinuteBlock = make([]Counts, 0)
	currentMinuteBlock := -1
	// parse the complete list of stepEntry (until EOF) - thereby also testing if it is parsable
	for { // read them all until EOF
		var stepEntry StepEntry
		if err := dec.Decode(&stepEntry); err != nil {
			if err == io.EOF {
				break
			} else {
				LogError(err)
			}
		}
		allCounts.Requests++
		requestBytes += uint64(stepEntry.RequestSize)
		responseBytes += uint64(stepEntry.ResponseSize)
		if len(example) == 0 {
			// TODO Record one sample request for the detailed report
			// example = stepEntry.Example
		}
		// populate values per minute blocks
		currentMinute := stepEntry.Timestamps.Start.Minute()
		if currentMinuteBlock != currentMinute {
			currentMinuteBlock = currentMinute
			valuesPerMinuteBlockTTFB = append(valuesPerMinuteBlockTTFB, make([]float64, 0))
			valuesPerMinuteBlockPARS = append(valuesPerMinuteBlockPARS, make([]float64, 0))
			valuesPerMinuteBlockTODU = append(valuesPerMinuteBlockTODU, make([]float64, 0))
			countsPerMinuteBlock = append(countsPerMinuteBlock, Counts{})
		}
		// track the timestamps
		if ttfb, completed := stepEntry.Timestamps.TimeToFirstByte(false); completed {
			valuesTTFB = append(valuesTTFB, float64(ttfb.Nanoseconds()))
			valuesPerMinuteBlockTTFB[len(valuesPerMinuteBlockTTFB)-1] = append(valuesPerMinuteBlockTTFB[len(valuesPerMinuteBlockTTFB)-1], float64(ttfb.Nanoseconds()))
		}
		if pars, completed := stepEntry.Timestamps.TimeToFirstByte(true); completed {
			valuesPARS = append(valuesPARS, float64(pars.Nanoseconds()))
			valuesPerMinuteBlockPARS[len(valuesPerMinuteBlockPARS)-1] = append(valuesPerMinuteBlockPARS[len(valuesPerMinuteBlockPARS)-1], float64(pars.Nanoseconds()))
		}
		if todu, completed := stepEntry.Timestamps.TotalDuration(); completed {
			valuesTODU = append(valuesTODU, float64(todu.Nanoseconds()))
			valuesPerMinuteBlockTODU[len(valuesPerMinuteBlockTODU)-1] = append(valuesPerMinuteBlockTODU[len(valuesPerMinuteBlockTODU)-1], float64(todu.Nanoseconds()))
		}
		// track the status codes
		if stepEntry.StatusCode > 0 {
			statusCodes[stepEntry.StatusCode]++
		}
		// track the Requests
		countsPerMinuteBlock[len(countsPerMinuteBlock)-1].Requests++
		// track the Failures
		if stepEntry.AssertionFailed {
			allCounts.Failures++
			failureTypes[stepEntry.AssertionFailedRootCause]++
			countsPerMinuteBlock[len(countsPerMinuteBlock)-1].Failures++
		}
		// track the Errors
		if stepEntry.Error {
			allCounts.Errors++
			errorTypes[stepEntry.ErrorRootCause]++
			countsPerMinuteBlock[len(countsPerMinuteBlock)-1].Errors++
		}
		// track the Timeouts
		if stepEntry.Timeout {
			allCounts.Timeouts++
			timeoutTypes[stepEntry.TimeoutRootCause]++
			countsPerMinuteBlock[len(countsPerMinuteBlock)-1].Timeouts++
		}
	}
	return
}

func analyzeExpectation(stats *Stats) (result string) {
	var sb strings.Builder
	sb.WriteString("Expectations\n")
	sb.WriteString("-----------------------------------------------------------------------\n")
	s, unmet := writePercentageExpectation(stats.Expectation.SuccessPercentageAtLeast, stats.Counts.SuccessPercentage(), "minimum success percentage expectation", false)
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writePercentageExpectation(stats.Expectation.FailurePercentageAtMost, stats.Counts.FailurePercentage(), "maximum failure percentage expectation", true)
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writePercentageExpectation(stats.Expectation.ErrorPercentageAtMost, stats.Counts.ErrorPercentage(), "maximum error percentage expectation", true)
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writePercentageExpectation(stats.Expectation.TimeoutPercentageAtMost, stats.Counts.TimeoutPercentage(), "maximum timeout percentage expectation", true)
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeCountExpectation(stats.Expectation.SuccessCountAtLeast, stats.Counts.Successes(), "minimum success count expectation", false)
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeCountExpectation(stats.Expectation.FailureCountAtMost, stats.Counts.Failures, "maximum failure count expectation", true)
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeCountExpectation(stats.Expectation.ErrorCountAtMost, stats.Counts.Errors, "maximum error count expectation", true)
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeCountExpectation(stats.Expectation.TimeoutCountAtMost, stats.Counts.Timeouts, "maximum timeout count expectation", true)
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writePercentileDurationExpectations(stats.Expectation.TotalRequestResponseTimePercentileLimits, stats.TRRT, "percentile duration expectation of Total-Request-Response-Time (TRRT)")
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writePercentileDurationExpectations(stats.Expectation.TimeToFirstBytePercentileLimits, stats.TTFB, "percentile duration expectation of Time-To-First-Byte (TTFB)")
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writePercentileDurationExpectations(stats.Expectation.TimeAfterRequestSentPercentileLimits, stats.TARS, "percentile duration expectation of Time-After-Request-Sent (TARS)")
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeTotalBytesExpectation(stats.Expectation.TotalRequestBytesWithin, stats.RequestBytes, "total request bytes expectation")
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeTotalBytesExpectation(stats.Expectation.TotalResponseBytesWithin, stats.ResponseBytes, "total response bytes expectation")
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeStatusCodeExpectations(stats.Expectation.StatusCodeThresholds, stats.StatusCodes)
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeTypeMatchesPercentageExpectations(stats.Expectation.FailureTypeMatchesThresholds, stats.FailureTypes, "failure")
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeTypeMatchesPercentageExpectations(stats.Expectation.ErrorTypeMatchesThresholds, stats.ErrorTypes, "error")
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	s, unmet = writeTypeMatchesPercentageExpectations(stats.Expectation.TimeoutTypeMatchesThresholds, stats.TimeoutTypes, "timeout")
	sb.WriteString(s)
	if unmet {
		stats.HasUnmetExpectation = true
	}
	return sb.String()
}

func writeTypeMatchesPercentageExpectations(thresholds []*TypeMatchesThreshold, types map[string]int, label string) (result string, unmetExpectation bool) {
	var sb strings.Builder
	for _, t := range thresholds {
		re := regexp.MustCompile(t.RegExp)
		actualCount, totalCount := 0, 0
		for k, v := range types {
			totalCount += v
			if re.MatchString(k) {
				actualCount += v
			}
		}
		actualPercentage := 0.0
		if totalCount > 0 {
			actualPercentage = float64(actualCount) / float64(totalCount) * 100
			met := "Met"
			var which string
			if t.IsAtLeast {
				which = "at least"
				if actualPercentage < t.Percentage {
					met = "Unmet"
					t.Unmet = true
					unmetExpectation = true
				}
			} else {
				which = "at most"
				if actualPercentage > t.Percentage {
					met = "Unmet"
					t.Unmet = true
					unmetExpectation = true
				}
			}
			t.ActualValue = actualPercentage
			sb.WriteString(localizationPrinter.Sprintf("%s %s type matches percentage expectation: wanted %s %4.2f%% of %s types matching %s: got %4.2f%%\n", met, label, which, t.Percentage, label, t.RegExp, actualPercentage))
		}
	}
	return sb.String(), unmetExpectation
}

func writeStatusCodeExpectations(thresholds []*StatusCodeExpectation, codes map[int]int) (result string, unmetExpectation bool) {
	var sb strings.Builder
	for _, t := range thresholds {
		totalCount := 0
		for _, v := range codes {
			totalCount += v
		}
		actualCount := 0
		if count, ok := codes[t.StatusCode]; ok {
			actualCount = count
		}
		actualPercentage := 0.0
		if totalCount > 0 {
			actualPercentage = float64(actualCount) / float64(totalCount) * 100
			met := "Met"
			var which string
			if t.IsAtLeast {
				which = "at least"
				if actualPercentage < t.Percentage {
					met = "Unmet"
					t.Unmet = true
					unmetExpectation = true
				}
			} else {
				which = "at most"
				if actualPercentage > t.Percentage {
					met = "Unmet"
					t.Unmet = true
					unmetExpectation = true
				}
			}
			t.ActualValue = actualPercentage
			sb.WriteString(localizationPrinter.Sprintf("%s status code percentage expectation: wanted %s %4.2f%% of status code %d: got %4.2f%%\n", met, which, t.Percentage, t.StatusCode, actualPercentage))
		}
	}
	return sb.String(), unmetExpectation
}

func writeTotalBytesExpectation(within *RangeExpectation, bytes uint64, label string) (result string, unmetExpectation bool) {
	if within == nil {
		return
	}
	if within.Max > 0 {
		met := "Met"
		if bytes < within.Min || bytes > within.Max {
			met = "Unmet"
			within.Unmet = true
			unmetExpectation = true
		}
		within.ActualValue = bytes
		return localizationPrinter.Sprintf("%s %s: wanted within (%d - %d): got %d\n", met, label, within.Min, within.Max, bytes), unmetExpectation
	}
	return
}

func writePercentileDurationExpectations(pctlExpcts []*PercentileExpectation, values []float64, label string) (result string, unmetExpectation bool) {
	var sb strings.Builder
	for _, pctlExpct := range pctlExpcts {
		if pctlExpct.Percentile == 0 {
			return
		}
		met := "Met"
		if len(values) < int(math.Ceil(100/pctlExpct.Percentile)) {
			// need at least 100/n values for n% percentile
			return "Not enough values for percentile calculation", unmetExpectation
		}
		percentile, err := stats.Percentile(values, pctlExpct.Percentile)
		CheckErrAndLogError(err, "unable to calculate percentile")
		actualDuration := time.Duration(percentile)
		if actualDuration > pctlExpct.Duration {
			met = "Unmet"
			pctlExpct.Unmet = true
			unmetExpectation = true
		}
		pctlExpct.ActualValue = actualDuration
		sb.WriteString(fmt.Sprintf("%s %4.2f %s: wanted within %s got %s\n", met, pctlExpct.Percentile, label, pctlExpct.Duration, actualDuration))
	}
	return sb.String(), unmetExpectation
}

func writePercentageExpectation(target *PercentageExpectation, value float64, label string, smallerIsBetter bool) (result string, unmetExpectation bool) {
	if target == nil {
		return
	}
	met, what := "Met", "wanted at least"
	if smallerIsBetter && value > target.Percentage || !smallerIsBetter && value < target.Percentage {
		met = "Unmet"
		target.Unmet = true
		unmetExpectation = true
	}
	if smallerIsBetter {
		what = "wanted at most"
	}
	target.ActualValue = value
	return fmt.Sprintf("%s %s: %s %4.2f%% got %4.2f%%\n", met, label, what, target.Percentage, value), unmetExpectation
}

func writeCountExpectation(target *CountExpectation, value uint64, label string, smallerIsBetter bool) (result string, unmetExpectation bool) {
	if target == nil {
		return
	}
	met, what := "Met", "wanted at least"
	if smallerIsBetter && value > target.Count || !smallerIsBetter && value < target.Count {
		met = "Unmet"
		target.Unmet = true
		unmetExpectation = true
	}
	if smallerIsBetter {
		what = "wanted at most"
	}
	target.ActualValue = value
	return fmt.Sprintf("%s %s: %s %d got %d\n", met, label, what, target.Count, value), unmetExpectation
}

func printDistributions(stats *Stats) (result string) {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(localizationPrinter.Sprintf("Requests: %d\n", stats.Counts.Requests))
	sb.WriteString("-----------------------------------------------------------------------\n")
	successes := stats.Counts.Successes()
	sb.WriteString(localizationPrinter.Sprintf("%9d = %6.2f%%: Successes\n", successes, stats.Counts.SuccessPercentage()))
	sb.WriteString(localizationPrinter.Sprintf("%9d = %6.2f%%: Failures\n", stats.Counts.Failures, stats.Counts.FailurePercentage()))
	sb.WriteString(localizationPrinter.Sprintf("%9d = %6.2f%%: Errors\n", stats.Counts.Errors, stats.Counts.ErrorPercentage()))
	sb.WriteString(localizationPrinter.Sprintf("%9d = %6.2f%%: Timeouts\n", stats.Counts.Timeouts, stats.Counts.TimeoutPercentage()))

	statusCodesSum := 0
	for _, count := range stats.StatusCodes {
		statusCodesSum += count
	}
	sb.WriteString("\n")
	sb.WriteString("\n")
	sb.WriteString(localizationPrinter.Sprintln("Status Codes:", statusCodesSum))
	sb.WriteString("-----------------------------------------------------------------------\n")
	for _, pair := range sortByCountInt(stats.StatusCodes) {
		sb.WriteString(localizationPrinter.Sprintf("%9d = %6.2f%%: Response Status %d\n", pair.value, float64(pair.value)/float64(stats.Counts.Requests)*100, pair.key))
	}

	sb.WriteString("\n")
	sb.WriteString("\n")
	sb.WriteString(localizationPrinter.Sprintln("Failures:", stats.Counts.Failures))
	sb.WriteString("-----------------------------------------------------------------------\n")
	for _, pair := range sortByCount(stats.FailureTypes) {
		sb.WriteString(localizationPrinter.Sprintf("%9d = %6.2f%%: %s\n", pair.value, float64(pair.value)/float64(stats.Counts.Failures)*100, pair.key))
	}

	sb.WriteString("\n")
	sb.WriteString("\n")
	sb.WriteString(localizationPrinter.Sprintln("Errors:", stats.Counts.Errors))
	sb.WriteString("-----------------------------------------------------------------------\n")
	for _, pair := range sortByCount(stats.ErrorTypes) {
		sb.WriteString(localizationPrinter.Sprintf("%9d = %6.2f%%: %s\n", pair.value, float64(pair.value)/float64(stats.Counts.Errors)*100, pair.key))
	}

	sb.WriteString("\n")
	sb.WriteString("\n")
	sb.WriteString(localizationPrinter.Sprintln("Timeouts:", stats.Counts.Timeouts))
	sb.WriteString("-----------------------------------------------------------------------\n")
	for _, pair := range sortByCount(stats.TimeoutTypes) {
		sb.WriteString(localizationPrinter.Sprintf("%9d = %6.2f%%: %s\n", pair.value, float64(pair.value)/float64(stats.Counts.Timeouts)*100, pair.key))
	}

	sb.WriteString("\n")
	sb.WriteString("\n")
	sb.WriteString(localizationPrinter.Sprintf("Traffic Bytes:  %15d\n", stats.RequestBytes+stats.ResponseBytes))
	sb.WriteString("-----------------------------------------------------------------------\n")
	sb.WriteString(localizationPrinter.Sprintf("Request Bytes:  %15d\n", stats.RequestBytes))
	sb.WriteString(localizationPrinter.Sprintf("Response Bytes: %15d\n", stats.ResponseBytes))

	sb.WriteString("\n")
	sb.WriteString("\n")
	sb.WriteString(localizationPrinter.Sprintln("Total-Request-Response-Time (TRRT):", len(stats.TRRT), "Requests"))
	sb.WriteString("-----------------------------------------------------------------------")
	sb.WriteString("\n>>> Stats <<<\n")
	s, resultStats := printStats(stats.TRRT)
	stats.TotalRequestResponseTime.Stats = resultStats
	sb.WriteString(s)
	sb.WriteString("\n>>> Percentiles <<<\n")
	s, resultPercentiles := printPercentiles(stats.TRRT)
	stats.TotalRequestResponseTime.Percentiles = resultPercentiles
	sb.WriteString(s)
	sb.WriteString("\n>>> Histogram <<<\n")
	s, resultHistogram := printHistogram(stats.TRRT)
	stats.TotalRequestResponseTime.Histogram = resultHistogram
	sb.WriteString(s)

	sb.WriteString("\n")
	sb.WriteString("\n")
	sb.WriteString(localizationPrinter.Sprintln("Time-To-First-Byte (TTFB):", len(stats.TTFB), "Requests"))
	sb.WriteString("-----------------------------------------------------------------------")
	sb.WriteString("\n>>> Stats <<<\n")
	s, resultStats = printStats(stats.TTFB)
	stats.TimeToFirstByte.Stats = resultStats
	sb.WriteString(s)
	sb.WriteString("\n>>> Percentiles <<<\n")
	s, resultPercentiles = printPercentiles(stats.TTFB)
	stats.TimeToFirstByte.Percentiles = resultPercentiles
	sb.WriteString(s)
	sb.WriteString("\n>>> Histogram <<<\n")
	s, resultHistogram = printHistogram(stats.TTFB)
	stats.TimeToFirstByte.Histogram = resultHistogram
	sb.WriteString(s)

	sb.WriteString("\n")
	sb.WriteString("\n")
	sb.WriteString(localizationPrinter.Sprintln("Time-After-Request-Sent (TARS):", len(stats.TARS), "Requests"))
	sb.WriteString("-----------------------------------------------------------------------")
	sb.WriteString("\n>>> Stats <<<\n")
	s, resultStats = printStats(stats.TARS)
	stats.TimeAfterRequestSent.Stats = resultStats
	sb.WriteString(s)
	sb.WriteString("\n>>> Percentiles <<<\n")
	s, resultPercentiles = printPercentiles(stats.TARS)
	stats.TimeAfterRequestSent.Percentiles = resultPercentiles
	sb.WriteString(s)
	sb.WriteString("\n>>> Histogram <<<\n")
	s, resultHistogram = printHistogram(stats.TARS)
	stats.TimeAfterRequestSent.Histogram = resultHistogram
	sb.WriteString(s)

	sb.WriteString("\n")
	return sb.String()
}

func printHistogram(values []float64) (result string, analyzed ResultHistogram) {
	if len(values) == 0 {
		return
	}
	buf := new(bytes.Buffer)
	hist := histogram.Hist(10, values)
	err := histogram.Fprintf(buf, hist, histogram.Linear(20), func(v float64) string {
		return localizationPrinter.Sprint(time.Duration(v))
	})
	for _, b := range hist.Buckets {
		analyzed.Buckets = append(analyzed.Buckets, HistogramBucket{
			Min:   b.Min,
			Max:   b.Max,
			Count: b.Count,
		})
	}
	if err != nil {
		LogError(err, "unable to create histogram")
	}
	return buf.String(), analyzed
}

func printPercentiles(values []float64) (result string, analyzed ResultPercentiles) {
	if len(values) < 10 {
		return
	}
	var sb strings.Builder
	pctl80, err := stats.Percentile(values, 80)
	CheckErrAndLogError(err, "unable to calculate percentile")
	pctl90, err := stats.Percentile(values, 90)
	CheckErrAndLogError(err, "unable to calculate percentile")
	pctl95, err := stats.Percentile(values, 95)
	CheckErrAndLogError(err, "unable to calculate percentile")
	pctl99, err := stats.Percentile(values, 99)
	CheckErrAndLogError(err, "unable to calculate percentile")
	pctl99p9, err := stats.Percentile(values, 99.9)
	CheckErrAndLogError(err, "unable to calculate percentile")
	pctl99p99, err := stats.Percentile(values, 99.99)
	CheckErrAndLogError(err, "unable to calculate percentile")

	// write the values
	sb.WriteString(localizationPrinter.Sprintln("Percent 80.00%:", time.Duration(pctl80)))
	analyzed.P80p00 = pctl80
	sb.WriteString(localizationPrinter.Sprintln("Percent 90.00%:", time.Duration(pctl90)))
	analyzed.P90p00 = pctl90
	sb.WriteString(localizationPrinter.Sprintln("Percent 95.00%:", time.Duration(pctl95)))
	analyzed.P95p00 = pctl95
	sb.WriteString(localizationPrinter.Sprintln("Percent 99.00%:", time.Duration(pctl99)))
	analyzed.P99p00 = pctl99
	sb.WriteString(localizationPrinter.Sprintln("Percent 99.90%:", time.Duration(pctl99p9)))
	analyzed.P99p90 = pctl99p9
	sb.WriteString(localizationPrinter.Sprintln("Percent 99.99%:", time.Duration(pctl99p99)))
	analyzed.P99p99 = pctl99p99

	return sb.String(), analyzed
}

func printStats(values []float64) (result string, analyzed ResultStats) {
	if len(values) == 0 {
		return
	}
	var sb strings.Builder
	min, err := stats.Min(values)
	CheckErrAndLogError(err, "unable to calculate stats")
	max, err := stats.Max(values)
	CheckErrAndLogError(err, "unable to calculate stats")
	mean, err := stats.Mean(values)
	CheckErrAndLogError(err, "unable to calculate stats")
	median, err := stats.Median(values)
	CheckErrAndLogError(err, "unable to calculate stats")
	stdev, err := stats.StandardDeviation(values)
	CheckErrAndLogError(err, "unable to calculate stats")
	qrtls, err := stats.Quartile(values)
	CheckErrAndLogError(err, "unable to calculate stats")
	iqtr, err := stats.InterQuartileRange(values)
	CheckErrAndLogError(err, "unable to calculate stats")
	midhinge, err := stats.Midhinge(values)
	CheckErrAndLogError(err, "unable to calculate stats")
	trimean, err := stats.Trimean(values)
	CheckErrAndLogError(err, "unable to calculate stats")

	// write the values
	sb.WriteString(localizationPrinter.Sprintln("Minimum:", time.Duration(min)))
	analyzed.Minimum = min
	sb.WriteString(localizationPrinter.Sprintln("Maximum:", time.Duration(max)))
	analyzed.Maximum = max
	sb.WriteString(localizationPrinter.Sprintln("Mean:", time.Duration(mean)))
	analyzed.Mean = mean
	sb.WriteString(localizationPrinter.Sprintln("Median:", time.Duration(median)))
	analyzed.Median = median
	sb.WriteString(localizationPrinter.Sprintln("Standard Deviation:", time.Duration(stdev)))
	analyzed.StandardDeviation = stdev
	sb.WriteString(localizationPrinter.Sprintln("First Quartile:", time.Duration(qrtls.Q1)))
	analyzed.FirstQuartile = qrtls.Q1
	sb.WriteString(localizationPrinter.Sprintln("Third Quartile:", time.Duration(qrtls.Q3)))
	analyzed.ThirdQuartile = qrtls.Q3
	sb.WriteString(localizationPrinter.Sprintln("Inter-Quartile Range:", time.Duration(iqtr)))
	analyzed.InterQuartileRange = iqtr
	sb.WriteString(localizationPrinter.Sprintln("Midhinge:", time.Duration(midhinge)))
	analyzed.Midhinge = midhinge
	sb.WriteString(localizationPrinter.Sprintln("Trimean:", time.Duration(trimean)))
	analyzed.Trimean = trimean

	return sb.String(), analyzed
}
