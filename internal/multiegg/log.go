package multiegg

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
)

type Logger struct {
	mu  sync.Mutex
	out io.Writer
}

func NewLogger(out io.Writer) *Logger { return &Logger{out: out} }

var secretPattern = regexp.MustCompile(`(?i)(token|password|secret|authorization|credential)\s*[:=]?\s*[^\s&]+`)

func Redact(value string) string {
	value = secretPattern.ReplaceAllString(value, "$1=[REDACTED]")
	if i := strings.Index(value, "://"); i >= 0 {
		if at := strings.Index(value[i+3:], "@"); at >= 0 {
			value = value[:i+3] + "[REDACTED]@" + value[i+3+at+1:]
		}
	}
	return value
}

func (l *Logger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, "[MULTIEGG] %s\n", Redact(fmt.Sprintf(format, args...)))
}
