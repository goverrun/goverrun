package goverrun

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"
)

func DynamicJSON(jsonBytes []byte) interface{} {
	var parsed interface{}
	err := json.Unmarshal(jsonBytes, &parsed)
	if err != nil {
		log.Println(err) // TODO track it
	}
	return parsed
}

func RandomDuration(min, max time.Duration) time.Duration {
	if max < min {
		panic("max less than min")
	}
	if max == 0 {
		return 0
	}
	if max == min {
		return max
	} else {
		return time.Duration(rand.Int63n(int64(max-min)) + int64(min))
	}
}

func RandomNumber(min, max int) int {
	return rand.Intn(max+1-min) + min
}

func RandomElement(s []string) string {
	return s[RandomNumber(0, len(s)-1)]
}

func UnwrapDeepestError(currentErr error) string {
	for errors.Unwrap(currentErr) != nil {
		currentErr = errors.Unwrap(currentErr)
	}
	if currentErr != nil {
		return currentErr.Error()
	}
	return ""
}

func PrintMissingSubcommandAndExit(validCommands ...*flag.FlagSet) {
	var valids strings.Builder
	for i, valid := range validCommands {
		if i > 0 {
			valids.WriteString(", ")
		}
		valids.WriteString("'")
		valids.WriteString(valid.Name())
		valids.WriteString("'")
	}
	LogFatal("Missing required subcommand, choose from: ", valids.String())
	os.Exit(1)
}

func panicOnErr(err error) {
	if err != nil {
		panic(err)
	}
}

func HeaderSize(headerMap map[string][]string) (headerSize int) {
	if headerMap != nil {
		for header, values := range headerMap {
			headerSize += len(header) * len(values)
			for _, value := range values {
				headerSize += len(value) + 3 // colon, space, carriage return
			}
		}
	}
	return
}

func durationMeasurement(d time.Duration, completed bool) string {
	if completed {
		return fmt.Sprint(d)
	} else {
		return "n/a"
	}
}

// TODO use Generics in Go 1.18
func sortByCount(frequencies map[string]int) pairList {
	pl := make(pairList, len(frequencies))
	i := 0
	for k, v := range frequencies {
		pl[i] = pair{k, v}
		i++
	}
	sort.Sort(sort.Reverse(pl))
	return pl
}

func sortByCountInt(frequencies map[int]int) pairList {
	pl := make(pairList, len(frequencies))
	i := 0
	for k, v := range frequencies {
		pl[i] = pair{k, v}
		i++
	}
	sort.Sort(sort.Reverse(pl))
	return pl
}

type pair struct {
	key   interface{}
	value int
}

type pairList []pair

func (p pairList) Len() int           { return len(p) }
func (p pairList) Less(i, j int) bool { return p[i].value < p[j].value }
func (p pairList) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
