// traefiksupervisor: PID 1 of the Traefik image. Checker-as-parent topology per
// the container build standard: supervises the traefik process, serves the
// probe endpoints and metrics on ADMIN_PORT (backed by traefik's /ping), maps
// SIGTERM/SIGINT to the drain sequence, prepares the ACME storage file with the
// 0600 mode traefik enforces, reaps orphans, and propagates the child's exit
// status so a dead traefik can never hide behind a green probe.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
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
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type snapshot struct {
	Results map[string]checkResult `json:"checks"`
	TakenAt time.Time              `json:"takenAt"`
}

type state struct {
	snap     atomic.Value
	started  atomic.Bool
	draining atomic.Bool
}

var logLevels = map[string]int{"trace": 0, "debug": 1, "info": 2, "warn": 3, "error": 4}

type logger struct {
	level  int
	format string
}

func (l *logger) log(level, msg string) {
	if logLevels[level] < l.level {
		return
	}
	if l.format == "json" {
		entry, _ := json.Marshal(map[string]string{
			"time": time.Now().UTC().Format(time.RFC3339), "level": level,
			"logger": "traefiksupervisor", "msg": msg,
		})
		fmt.Fprintln(os.Stderr, string(entry))
	} else {
		fmt.Fprintf(os.Stderr, "%s %-5s traefiksupervisor: %s\n",
			time.Now().UTC().Format(time.RFC3339), strings.ToUpper(level), msg)
	}
}

func (l *logger) infof(format string, args ...any)  { l.log("info", fmt.Sprintf(format, args...)) }
func (l *logger) warnf(format string, args ...any)  { l.log("warn", fmt.Sprintf(format, args...)) }
func (l *logger) errorf(format string, args ...any) { l.log("error", fmt.Sprintf(format, args...)) }

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "fatal configuration error: "+msg)
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

func loadConfig() *config {
	cfg := &config{
		adminPort:     envInt("ADMIN_PORT", 9090, 1, 65535),
		bindAddr:      envStr("BIND_ADDR", "0.0.0.0"),
		logLevel:      envEnum("LOG_LEVEL", "info", "trace", "debug", "info", "warn", "error"),
		logFormat:     envEnum("LOG_FORMAT", "json", "json", "text"),
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
	log := &logger{level: logLevels[cfg.logLevel], format: cfg.logFormat}
	st := &state{}
	st.snap.Store(snapshot{Results: map[string]checkResult{}})

	prepareAcmeStorage(cfg, log)
	startAdmin(cfg, st, log)

	childExited := make(chan int, 1)
	child, err := startTraefik(cfg, childExited)
	if err != nil {
		log.errorf("failed to start traefik: %v", err)
		os.Exit(1)
	}
	log.infof("traefik started: version=%s revision=%s pid=%d config[%s]",
		envStr("APP_VERSION", "dev"), envStr("APP_REVISION", "unknown"), child.Pid, cfg.redacted())

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

func startTraefik(cfg *config, exited chan<- int) (*os.Process, error) {
	cmd := exec.Command(cfg.traefikBin, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	mainPid := cmd.Process.Pid

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
		results := map[string]checkResult{}
		response, err := client.Get(cfg.pingURL)
		if err != nil {
			results["traefik-ping"] = checkResult{OK: false, Detail: err.Error()}
		} else {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				results["traefik-ping"] = checkResult{OK: true, Detail: "ok"}
			} else {
				results["traefik-ping"] = checkResult{OK: false,
					Detail: "ping returned HTTP " + strconv.Itoa(response.StatusCode)}
			}
		}
		st.snap.Store(snapshot{Results: results, TakenAt: time.Now()})
		if !st.started.Load() && allOk(results) {
			st.started.Store(true)
			log.infof("startup complete: first fully successful health cycle")
		}
		for name, result := range results {
			if before, seen := previous[name]; seen && before.OK != result.OK {
				log.infof("health check '%s' transitioned %v -> %v (%s)",
					name, before.OK, result.OK, result.Detail)
			}
		}
		previous = results
		if sleep := cfg.checkInterval - time.Since(cycleStart); sleep > 0 {
			time.Sleep(sleep)
		}
	}
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

func startAdmin(cfg *config, st *state, log *logger) {
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
					"status": map[bool]string{true: "ok", false: "unavailable"}[ok],
					"draining": st.draining.Load(), "checks": snap.Results, "takenAt": snap.TakenAt,
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
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		snap := st.snap.Load().(snapshot)
		var body strings.Builder
		body.WriteString("# TYPE build_info gauge\n")
		fmt.Fprintf(&body, "build_info{version=%q,revision=%q} 1\n",
			envStr("APP_VERSION", "dev"), envStr("APP_REVISION", "unknown"))
		body.WriteString("# TYPE health_check_up gauge\n")
		for name, result := range snap.Results {
			up := 0
			if result.OK {
				up = 1
			}
			fmt.Fprintf(&body, "health_check_up{check=%q} %d\n", name, up)
		}
		body.WriteString("# EOF\n")
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		_, _ = w.Write([]byte(body.String()))
	})
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.bindAddr, strconv.Itoa(cfg.adminPort)))
	if err != nil {
		fatal("cannot bind admin listener: " + err.Error())
	}
	server := &http.Server{Handler: mux}
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
