package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ECS-shaped JSON logging (flat dotted keys, one object per line) on stdout,
// or a one-line text form for local development. Every image of the stack
// emits the same fixed fields so one log query works across all of them.

const (
	serviceName = "auth-portal"
	ecsVersion  = "8.11"
)

var logLevels = map[string]int{"trace": 0, "debug": 1, "info": 2, "warn": 3, "error": 4}

type logger struct {
	level   int
	format  string
	version string
	mu      sync.Mutex
}

type fields map[string]any

func newLogger(level, format string) *logger {
	return &logger{level: logLevels[level], format: format, version: envStr("APP_VERSION", "dev")}
}

func (l *logger) enabled(level string) bool { return logLevels[level] >= l.level }

// log writes one line; component becomes log.logger, extra fields are merged
// verbatim (callers pass ECS field names).
func (l *logger) log(level, component, msg string, extra fields) {
	if !l.enabled(level) {
		return
	}
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == "json" {
		entry := map[string]any{
			"@timestamp":      now.Format(time.RFC3339Nano),
			"log.level":       level,
			"log.logger":      component,
			"message":         msg,
			"ecs.version":     ecsVersion,
			"service.name":    serviceName,
			"service.version": l.version,
		}
		for k, v := range extra {
			entry[k] = v
		}
		line, err := json.Marshal(entry)
		if err != nil {
			line = []byte(fmt.Sprintf(`{"@timestamp":%q,"log.level":"error","log.logger":"logger","message":"unmarshalable log entry: %s"}`,
				now.Format(time.RFC3339Nano), strings.ReplaceAll(err.Error(), `"`, `'`)))
		}
		fmt.Fprintln(os.Stdout, string(line))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %s[%s]: %s", now.Format(time.RFC3339), strings.ToUpper(level), serviceName, component, msg)
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, extra[k])
	}
	fmt.Fprintln(os.Stdout, b.String())
}

func (l *logger) infof(format string, args ...any) {
	l.log("info", "server", fmt.Sprintf(format, args...), nil)
}
func (l *logger) warnf(format string, args ...any) {
	l.log("warn", "server", fmt.Sprintf(format, args...), nil)
}
func (l *logger) errorf(format string, args ...any) {
	l.log("error", "server", fmt.Sprintf(format, args...), nil)
}
