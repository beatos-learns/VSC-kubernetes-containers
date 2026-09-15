package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var logLevels = map[string]int{"trace": 0, "debug": 1, "info": 2, "warn": 3, "error": 4}

// logger writes the supervisor's own lines to stdout: ECS JSON (one object
// per line, flat dotted keys) by default, a one-line text form for local
// runs. PostgreSQL's own output stays on stderr in its own format.
type logger struct {
	mu      sync.Mutex
	level   int
	format  string
	service string
	version string
}

// activeLog lets fatal() use the structured logger once it exists.
var activeLog *logger

func newLogger(level, format string) *logger {
	l := &logger{
		level:   logLevels[level],
		format:  format,
		service: "postgresql",
		version: envStr("APP_VERSION", "dev"),
	}
	activeLog = l
	return l
}

func (l *logger) log(level, msg string, fields ...any) {
	if logLevels[level] < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == "json" {
		entry := map[string]any{
			"@timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
			"log.level":       level,
			"log.logger":      "supervisor",
			"message":         msg,
			"ecs.version":     "8.11",
			"service.name":    l.service,
			"service.version": l.version,
		}
		for i := 0; i+1 < len(fields); i += 2 {
			if key, ok := fields[i].(string); ok {
				entry[key] = fields[i+1]
			}
		}
		line, err := json.Marshal(entry)
		if err != nil {
			line = []byte(fmt.Sprintf(`{"log.level":"error","message":"log entry not serializable: %s"}`, err))
		}
		fmt.Fprintln(os.Stdout, string(line))
		return
	}
	var extra strings.Builder
	for i := 0; i+1 < len(fields); i += 2 {
		fmt.Fprintf(&extra, " %v=%v", fields[i], fields[i+1])
	}
	fmt.Fprintf(os.Stdout, "%s %-5s supervisor: %s%s\n",
		time.Now().UTC().Format(time.RFC3339), strings.ToUpper(level), msg, extra.String())
}

func (l *logger) infof(format string, args ...any)  { l.log("info", fmt.Sprintf(format, args...)) }
func (l *logger) warnf(format string, args ...any)  { l.log("warn", fmt.Sprintf(format, args...)) }
func (l *logger) errorf(format string, args ...any) { l.log("error", fmt.Sprintf(format, args...)) }

func (l *logger) info(msg string, fields ...any)  { l.log("info", msg, fields...) }
func (l *logger) warn(msg string, fields ...any)  { l.log("warn", msg, fields...) }
func (l *logger) error(msg string, fields ...any) { l.log("error", msg, fields...) }

// fatal aborts the process with one clear message: structured once the
// logger exists, plain text on stderr while the configuration is still
// being parsed.
func fatal(msg string) {
	if activeLog != nil {
		activeLog.error("fatal: " + msg)
	} else {
		fmt.Fprintln(os.Stderr, "fatal configuration error: "+msg)
	}
	os.Exit(1)
}
