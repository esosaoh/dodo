package cmd

import (
	"os"

	"github.com/esosaoh/dodo/internal/classify"
)

const (
	colorReset   = "\033[0m"
	colorRed     = "\033[31m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorMagenta = "\033[35m"
	colorCyan    = "\033[36m"
	colorDim     = "\033[2m"
	colorBold    = "\033[1m"
)

var (
	noColorEnv     = os.Getenv("NO_COLOR") == ""
	colorEnabled   = noColorEnv && isTerminal(os.Stdout)
	stderrLive     = isTerminal(os.Stderr)
	stderrColorize = noColorEnv && stderrLive
)

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func colorize(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + colorReset
}

func colorizeErr(code, s string) string {
	if !stderrColorize {
		return s
	}
	return code + s + colorReset
}

var classColor = map[classify.Class]string{
	classify.ClassDead:      colorRed,
	classify.ClassSoft404:   colorMagenta,
	classify.ClassMalformed: colorYellow,
	classify.ClassBlocked:   colorCyan,
	classify.ClassUnknown:   colorDim,
	classify.ClassAlive:     colorGreen,
}
