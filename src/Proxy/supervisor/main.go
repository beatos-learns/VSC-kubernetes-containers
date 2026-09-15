// traefiksupervisor: PID 1 of the Traefik image. Checker-as-parent topology per
// the container build standard: supervises the traefik process, serves the
// probe endpoints and OpenMetrics on ADMIN_PORT (backed by traefik's /ping),
// maps SIGTERM/SIGINT to the drain sequence, prepares the ACME storage file
// with the 0600 mode traefik enforces, reaps orphans, and propagates the
// child's exit status so a dead traefik can never hide behind a green probe.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/common/expfmt"
)

const (
	serviceName = "traefik"
	loggerName  = "supervisor"
	ecsVersion  = "8.11"
	pingCheck   = "traefik-ping"
	openMetrics = "application/openmetrics-text; version=1.0.0; charset=utf-8"
)

type config struct {
	adminPort     int
	bindAddr      string
	logLevel      string
	logFormat     string
	checkInterval time.Duration
	checkTimeout  time.Duration
	staleFactor   float64
	drainDelay    time.Duration
	drainBudget   time.Duration

	traefikBin  string
	pingURL     string
	acmeStorage string
}

type checkResult struct {
	OK              bool      `json:"ok"`
	Detail          string    `json:"detail"`
	DurationSeconds float64   `json:"durationSeconds"`
	LastSuccess     time.Time `json:"lastSuccess"`
}

type snapshot struct {
	Results map[string]checkResult `json:"checks"`
	TakenAt time.Time              `json:"takenAt"`
}

type state struct {
	snap       atomic.Value
	started    atomic.Bool
	draining   atomic.Bool
	cycles     atomic.Uint64
	childUp    atomic.Bool
	childStart atomic.Int64
}

var logLevels = map[string]int{"trace": 0, "debug": 1, "info": 2, "warn": 3, "error": 4}

// logger writes ECS-shaped JSON (or a one-line text form) to stdout: the
// supervisor's own lines; traefik's output passes through untouched.
type logger struct {
	level   int
	format  string
	version string
	mu      sync.Mutex
}

func (l *logger) log(level, msg string, fields ...any) {
	if logLevels[level] < l.level {
		return
	}
	now := time.Now().UTC()
	var line []byte
	if l.format == "json" {
		entry := map[string]any{
			"@timestamp":      now.Format(time.RFC3339Nano),
			"log.level":       level,
			"log.logger":      loggerName,
			"message":         msg,
			"ecs.version":     ecsVersion,
			"service.name":    serviceName,
			"service.version": l.version,
		}
		for i := 0; i+1 < len(fields); i += 2 {
			if key, ok := fields[i].(string); ok {
				entry[key] = fields[i+1]
			}
		}
		line, _ = json.Marshal(entry)
	} else {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %-5s %s: %s", now.Format(time.RFC3339), strings.ToUpper(level), loggerName, msg)
		for i := 0; i+1 < len(fields); i += 2 {
			fmt.Fprintf(&b, " %v=%v", fields[i], fields[i+1])
		}
		line = []byte(b.String())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = os.Stdout.Write(append(line, '\n'))
}

func (l *logger) infof(format string, args ...any)  { l.log("info", fmt.Sprintf(format, args...)) }
func (l *logger) warnf(format string, args ...any)  { l.log("warn", fmt.Sprintf(format, args...)) }
func (l *logger) errorf(format string, args ...any) { l.log("error", fmt.Sprintf(format, args...)) }

func fatal(msg string) {
	boot := &logger{level: 0, format: "json", version: envStr("APP_VERSION", "dev")}
	boot.log("error", "fatal configuration error: "+msg)
	os.Exit(1)
}

func envStr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func envInt(name string, fallback, min, max int) int {
	raw := envStr(name, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil {
		fatal(name + "=" + raw + " is not an integer")
	}
	if value < min || value > max {
		fatal(fmt.Sprintf("%s=%d is outside [%d, %d]", name, value, min, max))
	}
	return value
}

func envFloat(name string, fallback, min, max float64) float64 {
	raw := envStr(name, strconv.FormatFloat(fallback, 'f', -1, 64))
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		fatal(name + "=" + raw + " is not a number")
	}
	if value < min || value > max {
		fatal(fmt.Sprintf("%s=%v is outside [%v, %v]", name, value, min, max))
	}
	return value
}

func envEnum(name, fallback string, allowed ...string) string {
	value := strings.ToLower(envStr(name, fallback))
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	fatal(name + "=" + value + " is invalid; allowed: " + strings.Join(allowed, ", "))
	return ""
}

// The supervisor's own log level and format follow traefik's unless LOG_LEVEL /
// LOG_FORMAT are set explicitly, so one deployment-side logging block drives
// both processes.
func traefikLogLevel() string {
	switch strings.ToLower(envStr("TRAEFIK_LOG_LEVEL", "info")) {
	case "trace":
		return "trace"
	case "debug":
		return "debug"
	case "warn", "warning":
		return "warn"
	case "error", "fatal", "panic":
		return "error"
	}
	return "info"
}

func traefikLogFormat() string {
	if strings.EqualFold(envStr("TRAEFIK_LOG_FORMAT", "json"), "common") {
		return "text"
	}
	return "json"
}

func loadConfig() *config {
	cfg := &config{
		adminPort:     envInt("ADMIN_PORT", 9090, 1, 65535),
		bindAddr:      envStr("BIND_ADDR", "0.0.0.0"),
		logLevel:      envEnum("LOG_LEVEL", traefikLogLevel(), "trace", "debug", "info", "warn", "error"),
		logFormat:     envEnum("LOG_FORMAT", traefikLogFormat(), "json", "text"),
		checkInterval: time.Duration(envInt("HEALTH_CHECK_INTERVAL", 5, 1, 3600)) * time.Second,
		checkTimeout:  time.Duration(envInt("HEALTH_CHECK_TIMEOUT", 2, 1, 3600)) * time.Second,
		staleFactor:   envFloat("HEALTH_STALE_FACTOR", 3, 1, 100),
		drainDelay:    time.Duration(envInt("SHUTDOWN_DRAIN_DELAY", 3, 0, 600)) * time.Second,
		drainBudget:   time.Duration(envInt("SHUTDOWN_TIMEOUT", 15, 1, 3600)) * time.Second,
		traefikBin:    envStr("TRAEFIK_BIN", "/traefik"),
		pingURL:       envStr("PING_URL", "http://127.0.0.1:8082/ping"),
		acmeStorage:   envStr("ACME_STORAGE_PREPARE", "/data/acme.json"),
	}
	if cfg.checkTimeout >= cfg.checkInterval {
		fatal(fmt.Sprintf("HEALTH_CHECK_TIMEOUT (%v) must be smaller than HEALTH_CHECK_INTERVAL (%v)",
			cfg.checkTimeout, cfg.checkInterval))
	}
	return cfg
}

func (c *config) redacted() string {
	return fmt.Sprintf("ADMIN_PORT=%d BIND_ADDR=%s LOG_LEVEL=%s LOG_FORMAT=%s "+
		"HEALTH_CHECK_INTERVAL=%v HEALTH_CHECK_TIMEOUT=%v HEALTH_STALE_FACTOR=%v "+
		"SHUTDOWN_DRAIN_DELAY=%v SHUTDOWN_TIMEOUT=%v PING_URL=%s ACME_STORAGE_PREPARE=%s",
		c.adminPort, c.bindAddr, c.logLevel, c.logFormat,
		c.checkInterval, c.checkTimeout, c.staleFactor, c.drainDelay, c.drainBudget,
		c.pingURL, c.acmeStorage)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(probe(os.Args[2:]))
	}
	cfg := loadConfig()
	version := envStr("APP_VERSION", "dev")
	revision := envStr("APP_REVISION", "unknown")
	log := &logger{level: logLevels[cfg.logLevel], format: cfg.logFormat, version: version}
	st := &state{}
	st.snap.Store(snapshot{Results: map[string]checkResult{pingCheck: {Detail: "not checked yet"}}})

	log.infof("traefik supervisor starting: version=%s revision=%s config[%s]", version, revision, cfg.redacted())

	prepareAcmeStorage(cfg, log)
	startAdmin(cfg, st, log, newRegistry(st, version, revision))

	childExited := make(chan int, 1)
	child, err := startTraefik(cfg, st, childExited)
	if err != nil {
		log.errorf("failed to start traefik: %v", err)
		os.Exit(1)
	}
	log.log("info", "traefik started", "process.pid", child.Pid)

	go checkerLoop(cfg, st, log)

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	select {
	case code := <-childExited:
		log.errorf("traefik exited unexpectedly with code %d", code)
		if code == 0 {
			code = 1
		}
		os.Exit(code)
	case sig := <-signals:
		log.infof("received %v: draining (readiness now 503, drain delay %v)", sig, cfg.drainDelay)
		st.draining.Store(true)
		time.Sleep(cfg.drainDelay)
		begin := time.Now()
		_ = child.Signal(syscall.SIGTERM)
		select {
		case code := <-childExited:
			if code != 0 {
				log.warnf("traefik shutdown reported exit code %d", code)
				os.Exit(code)
			}
			log.infof("graceful shutdown complete in %v", time.Since(begin).Round(time.Millisecond))
			os.Exit(0)
		case <-time.After(cfg.drainBudget):
			log.errorf("drain budget of %v exhausted; sending SIGKILL", cfg.drainBudget)
			_ = child.Signal(syscall.SIGKILL)
			os.Exit(1)
		}
	}
}

// Traefik refuses an ACME storage file with permissions wider than 0600, and a
// freshly mounted volume starts empty; pre-creating the file removes the manual
// touch+chmod step. Set ACME_STORAGE_PREPARE=off to disable.
func prepareAcmeStorage(cfg *config, log *logger) {
	if cfg.acmeStorage == "" || strings.EqualFold(cfg.acmeStorage, "off") {
		return
	}
	dir := filepath.Dir(cfg.acmeStorage)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		log.warnf("ACME storage preparation skipped: %s is not a mounted directory", dir)
		return
	}
	if _, err := os.Stat(cfg.acmeStorage); err == nil {
		if err := os.Chmod(cfg.acmeStorage, 0o600); err != nil {
			log.warnf("could not chmod existing ACME storage %s to 0600: %v", cfg.acmeStorage, err)
		}
		return
	}
	file, err := os.OpenFile(cfg.acmeStorage, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.warnf("could not pre-create ACME storage %s: %v", cfg.acmeStorage, err)
		return
	}
	_ = file.Close()
	log.infof("pre-created ACME storage %s with mode 0600", cfg.acmeStorage)
}

func startTraefik(cfg *config, st *state, exited chan<- int) (*os.Process, error) {
	cmd := exec.Command(cfg.traefikBin, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	mainPid := cmd.Process.Pid
	st.childStart.Store(time.Now().Unix())
	st.childUp.Store(true)

	// Sole wait4 owner: reaps traefik and any orphan reparented to PID 1.
	go func() {
		chld := make(chan os.Signal, 8)
		signal.Notify(chld, syscall.SIGCHLD)
		chld <- syscall.SIGCHLD
		for range chld {
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
				if pid == mainPid {
					code := status.ExitStatus()
					if status.Signaled() {
						code = 128 + int(status.Signal())
					}
					st.childUp.Store(false)
					exited <- code
				}
			}
		}
	}()
	return cmd.Process, nil
}

func checkerLoop(cfg *config, st *state, log *logger) {
	client := &http.Client{Timeout: cfg.checkTimeout}
	previous := map[string]checkResult{}
	for {
		cycleStart := time.Now()
		results := map[string]checkResult{
			pingCheck: checkPing(client, cfg.pingURL, previous[pingCheck]),
		}
		st.snap.Store(snapshot{Results: results, TakenAt: time.Now()})
		st.cycles.Add(1)
		if !st.started.Load() && allOk(results) {
			st.started.Store(true)
			log.infof("startup complete: first fully successful health cycle")
		}
		for name, result := range results {
			if before, seen := previous[name]; seen && before.OK != result.OK {
				log.log("info", fmt.Sprintf("health check '%s' transitioned %v -> %v (%s)",
					name, before.OK, result.OK, result.Detail), "health.check", name, "health.up", result.OK)
			}
		}
		previous = results
		if sleep := cfg.checkInterval - time.Since(cycleStart); sleep > 0 {
			time.Sleep(sleep)
		}
	}
}

// checkPing verifies traefik's /ping on the internal ping entrypoint; the
// last-success timestamp survives a failing cycle.
func checkPing(client *http.Client, url string, previous checkResult) checkResult {
	result := checkResult{LastSuccess: previous.LastSuccess}
	begin := time.Now()
	response, err := client.Get(url)
	result.DurationSeconds = time.Since(begin).Seconds()
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		result.Detail = "ping returned HTTP " + strconv.Itoa(response.StatusCode)
		return result
	}
	result.OK = true
	result.Detail = "ok"
	result.LastSuccess = time.Now()
	return result
}

func probeHost(addr string) string {
	switch addr {
	case "", "0.0.0.0", "*":
		return "127.0.0.1"
	case "::":
		return "::1"
	}
	return addr
}

func allOk(results map[string]checkResult) bool {
	for _, result := range results {
		if !result.OK {
			return false
		}
	}
	return len(results) > 0
}

// ---------------------------------------------------------------------------
// metrics

func newRegistry(st *state, version, revision string) *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newOpsCollector(st, version, revision),
	)
	return registry
}

// opsCollector renders the cached health snapshot and the supervisor state as
// the baseline families every image of this repository serves.
type opsCollector struct {
	st       *state
	version  string
	revision string

	buildInfo        *prometheus.Desc
	checkUp          *prometheus.Desc
	checkDuration    *prometheus.Desc
	checkLastSuccess *prometheus.Desc
	snapshotAge      *prometheus.Desc
	cycles           *prometheus.Desc
	draining         *prometheus.Desc
	childUp          *prometheus.Desc
	childStart       *prometheus.Desc
}

func newOpsCollector(st *state, version, revision string) *opsCollector {
	return &opsCollector{
		st: st, version: version, revision: revision,
		buildInfo: prometheus.NewDesc("build_info",
			"The running artifact; always 1.", []string{"version", "revision"}, nil),
		checkUp: prometheus.NewDesc("health_check_up",
			"1 when the check passed in the last checker cycle.", []string{"check"}, nil),
		checkDuration: prometheus.NewDesc("health_check_duration_seconds",
			"Duration of the last run of the check.", []string{"check"}, nil),
		checkLastSuccess: prometheus.NewDesc("health_check_last_success_timestamp_seconds",
			"Unix time of the last pass of the check; 0 until it passed once.", []string{"check"}, nil),
		snapshotAge: prometheus.NewDesc("health_snapshot_age_seconds",
			"Age of the cached health snapshot; 0 before the first checker cycle.", nil, nil),
		cycles: prometheus.NewDesc("health_checker_cycles_total",
			"Completed health checker cycles.", nil, nil),
		draining: prometheus.NewDesc("health_draining",
			"1 once the shutdown drain latch is set (readiness reports 503).", nil, nil),
		childUp: prometheus.NewDesc("supervisor_child_up",
			"1 while the supervised traefik process is running.", nil, nil),
		childStart: prometheus.NewDesc("supervisor_child_start_time_seconds",
			"Unix time the supervised traefik process was started; 0 before it started.", nil, nil),
	}
}

func (c *opsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{c.buildInfo, c.checkUp, c.checkDuration, c.checkLastSuccess,
		c.snapshotAge, c.cycles, c.draining, c.childUp, c.childStart} {
		ch <- desc
	}
}

func (c *opsCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.buildInfo, prometheus.GaugeValue, 1, c.version, c.revision)
	snap := c.st.snap.Load().(snapshot)
	names := make([]string, 0, len(snap.Results))
	for name := range snap.Results {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result := snap.Results[name]
		ch <- prometheus.MustNewConstMetric(c.checkUp, prometheus.GaugeValue, boolGauge(result.OK), name)
		ch <- prometheus.MustNewConstMetric(c.checkDuration, prometheus.GaugeValue, result.DurationSeconds, name)
		lastSuccess := 0.0
		if !result.LastSuccess.IsZero() {
			lastSuccess = float64(result.LastSuccess.UnixNano()) / 1e9
		}
		ch <- prometheus.MustNewConstMetric(c.checkLastSuccess, prometheus.GaugeValue, lastSuccess, name)
	}
	age := 0.0
	if !snap.TakenAt.IsZero() {
		age = time.Since(snap.TakenAt).Seconds()
	}
	ch <- prometheus.MustNewConstMetric(c.snapshotAge, prometheus.GaugeValue, age)
	ch <- prometheus.MustNewConstMetric(c.cycles, prometheus.CounterValue, float64(c.st.cycles.Load()))
	ch <- prometheus.MustNewConstMetric(c.draining, prometheus.GaugeValue, boolGauge(c.st.draining.Load()))
	ch <- prometheus.MustNewConstMetric(c.childUp, prometheus.GaugeValue, boolGauge(c.st.childUp.Load()))
	ch <- prometheus.MustNewConstMetric(c.childStart, prometheus.GaugeValue, float64(c.st.childStart.Load()))
}

func boolGauge(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// metricsHandler always answers OpenMetrics (no content negotiation): the body
// is rendered into a buffer first so a gather failure never yields a partial
// 200, and ends with the `# EOF` terminator the encoder writes on Close.
func metricsHandler(registry *prometheus.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		families, err := registry.Gather()
		if err != nil {
			http.Error(w, "metrics gather failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var body bytes.Buffer
		encoder := expfmt.NewEncoder(&body, expfmt.NewFormat(expfmt.TypeOpenMetrics))
		for _, family := range families {
			if err := encoder.Encode(family); err != nil {
				http.Error(w, "metrics encode failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if closer, ok := encoder.(expfmt.Closer); ok {
			if err := closer.Close(); err != nil {
				http.Error(w, "metrics encode failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", openMetrics)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body.Bytes())
	}
}

// ---------------------------------------------------------------------------
// admin listener

func startAdmin(cfg *config, st *state, log *logger, registry *prometheus.Registry) {
	fresh := func() bool {
		snap := st.snap.Load().(snapshot)
		return !snap.TakenAt.IsZero() &&
			time.Since(snap.TakenAt) <= time.Duration(cfg.staleFactor*float64(cfg.checkInterval))
	}
	handler := func(up func() bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ok := up()
			status := http.StatusOK
			if !ok {
				status = http.StatusServiceUnavailable
			}
			if r.URL.Query().Get("verbose") == "1" {
				snap := st.snap.Load().(snapshot)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				body, _ := json.Marshal(map[string]any{
					"status":   map[bool]string{true: "ok", false: "unavailable"}[ok],
					"draining": st.draining.Load(), "checks": snap.Results, "takenAt": snap.TakenAt,
					"cycles": st.cycles.Load(), "childUp": st.childUp.Load(),
				})
				_, _ = w.Write(body)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(map[bool]string{true: "ok", false: "unavailable"}[ok]))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/startupz", handler(func() bool { return st.started.Load() }))
	mux.HandleFunc("/livez", handler(fresh))
	mux.HandleFunc("/readyz", handler(func() bool {
		snap := st.snap.Load().(snapshot)
		return fresh() && allOk(snap.Results) && !st.draining.Load()
	}))
	mux.Handle("/metrics", metricsHandler(registry))
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.bindAddr, strconv.Itoa(cfg.adminPort)))
	if err != nil {
		fatal("cannot bind admin listener: " + err.Error())
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.errorf("admin server failed: %v", err)
			os.Exit(1)
		}
	}()
}

func probe(args []string) int {
	endpoint := "livez"
	for _, arg := range args {
		if strings.HasPrefix(arg, "--endpoint=") {
			endpoint = strings.TrimPrefix(arg, "--endpoint=")
		}
	}
	switch endpoint {
	case "startupz", "livez", "readyz":
	default:
		fmt.Fprintln(os.Stderr, "unknown endpoint '"+endpoint+"'; allowed: startupz, livez, readyz")
		return 1
	}
	adminPort := envStr("ADMIN_PORT", "9090")
	adminHost := probeHost(envStr("BIND_ADDR", "0.0.0.0"))
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort(adminHost, adminPort) + "/" + endpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe failed: "+err.Error())
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}
