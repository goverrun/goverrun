package goverrun

import (
	"log"
	"os"
	"strings"
)

const (
	ansiReset       = "\u001b[0m"
	ansiWhite       = "\u001b[97m"
	ansiRed         = "\u001b[31m"
	ansiLightRed    = "\u001b[91m"
	ansiBlack       = "\u001b[30m"
	ansiGreen       = "\u001b[32m"
	ansiYellow      = "\u001b[33m"
	ansiBlue        = "\u001b[34m"
	ansiMagenta     = "\u001b[35m"
	ansiPurple      = "\u001b[95m"
	ansiCyan        = "\u001b[36m"
	ansiLightGray   = "\u001b[37m"
	ansiLightGreen  = "\u001b[92m"
	ansiLightYellow = "\u001b[93m"
	ansiOrange      = "\u001b[38;5;202m" // See "8-bit (256) colors" chart on https://stackoverflow.com/questions/4842424/list-of-ansi-color-escape-sequences

	ansiBold = "\u001B[1m"

	useIcons  = false
	useColors = true
)

var (
	debugLogger, infoLogger, successLogger, warningLogger, errorLogger, fatalLogger *log.Logger
)

func init() {
	var prefixDebug, prefixInfo, prefixSuccess, prefixWarning, prefixError, prefixFatal string

	if useIcons {
		prefixDebug = ansiLightGray + "➖️ " + ansiReset + " "
		prefixInfo = ansiWhite + "ℹ️ " + ansiReset + " "
		prefixSuccess = ansiLightGreen + "✅ " + ansiReset + " "
		prefixWarning = ansiLightYellow + "✴️ " + ansiReset + " "
		prefixError = ansiRed + "🆘 " + ansiReset + " "
		prefixFatal = ansiLightRed + "❌ " + ansiReset + " "
	} else {
		if useColors {
			prefixDebug = ansiLightGray + ansiBold + "[-]" + ansiReset + " "
			prefixInfo = ansiWhite + ansiBold + "[=]" + ansiReset + " "
			prefixSuccess = ansiLightGreen + ansiBold + "[+]" + ansiReset + " "
			prefixWarning = ansiLightYellow + ansiBold + "[*]" + ansiReset + " "
			prefixError = ansiRed + ansiBold + "[!]" + ansiReset + " "
			prefixFatal = ansiLightRed + ansiBold + "[X]" + ansiReset + " "
		} else {
			prefixDebug = "[-] "
			prefixInfo = "[=] "
			prefixSuccess = "[+] "
			prefixWarning = "[*] "
			prefixError = "[!] "
			prefixFatal = "[X] "
		}
	}

	debugLogger = log.New(os.Stdout, prefixDebug, 0 /*log.Ldate|log.Ltime|log.Lshortfile*/)
	infoLogger = log.New(os.Stdout, prefixInfo, 0 /*log.Ldate|log.Ltime|log.Lshortfile*/)
	successLogger = log.New(os.Stdout, prefixSuccess, 0 /*log.Ldate|log.Ltime|log.Lshortfile*/)
	warningLogger = log.New(os.Stdout, prefixWarning, 0 /*log.Ldate|log.Ltime|log.Lshortfile*/)
	errorLogger = log.New(os.Stdout, prefixError, 0 /*log.Ldate|log.Ltime|log.Lshortfile*/)
	fatalLogger = log.New(os.Stdout, prefixFatal, 0 /*log.Ldate|log.Ltime|log.Lshortfile*/)
}

func LogDateTime(b bool) {
	if b {
		debugLogger.SetFlags(log.Ldate | log.Ltime)
		infoLogger.SetFlags(log.Ldate | log.Ltime)
		successLogger.SetFlags(log.Ldate | log.Ltime)
		warningLogger.SetFlags(log.Ldate | log.Ltime)
		errorLogger.SetFlags(log.Ldate | log.Ltime)
		fatalLogger.SetFlags(log.Ldate | log.Ltime)
	} else {
		debugLogger.SetFlags(0)
		infoLogger.SetFlags(0)
		successLogger.SetFlags(0)
		warningLogger.SetFlags(0)
		errorLogger.SetFlags(0)
		fatalLogger.SetFlags(0)
	}
}

func LogDebug(v ...interface{}) {
	if v != nil {
		debugLogger.Println(v...)
	}
}
func LogDebugf(format string, v ...interface{}) {
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	if v != nil {
		debugLogger.Printf(format, v...)
	}
}

func LogInfo(v ...interface{}) {
	if v != nil {
		infoLogger.Println(v...)
	}
}
func LogInfof(format string, v ...interface{}) {
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	if v != nil {
		infoLogger.Printf(format, v...)
	}
}

func LogSuccess(v ...interface{}) {
	if v != nil {
		successLogger.Println(v...)
	}
}
func LogSuccessf(format string, v ...interface{}) {
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	if v != nil {
		successLogger.Printf(format, v...)
	}
}

func LogWarning(v ...interface{}) {
	if v != nil {
		warningLogger.Println(v...)
	}
}
func LogWarningf(format string, v ...interface{}) {
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	if v != nil {
		warningLogger.Printf(format, v...)
	}
}

func LogError(v ...interface{}) {
	if v != nil {
		errorLogger.Println(v...)
	}
}
func LogErrorf(format string, v ...interface{}) {
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	if v != nil {
		errorLogger.Printf(format, v...)
	}
}
func CheckErrAndLogError(err error, s string) {
	if err != nil {
		errorLogger.Println(s+":", err)
		const DEBUG_PANIC = false
		if DEBUG_PANIC {
			panic(err)
		}
	}
}

func LogFatal(v ...interface{}) {
	if v != nil {
		fatalLogger.Fatal(v...)
	}
}
func LogFatalf(format string, v ...interface{}) {
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	if v != nil {
		fatalLogger.Fatalf(format, v...)
	}
}
func CheckErrAndLogFatal(err error, s string) {
	if err != nil {
		fatalLogger.Fatal(s+": ", err)
	}
}
