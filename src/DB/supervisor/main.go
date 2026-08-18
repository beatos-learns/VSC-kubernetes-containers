// pgsupervisor: PID 1 of the PostgreSQL image. Checker-as-parent topology per
// the container build standard: supervises the postmaster, serves the probe
// endpoints and metrics on ADMIN_PORT, runs initdb on an empty PGDATA, maps
// SIGTERM/SIGINT to the drain sequence, reaps orphans, and propagates the
// child's exit status so a dead postmaster can never hide behind a green probe.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const initMarker = ".pgsupervisor-init-incomplete"

type config struct {
	port          int
	adminPort     int
	bindAddr      string
	logLevel      string
	logFormat     string
	checkInterval time.Duration
	checkTimeout  time.Duration
	staleFactor   float64
	drainDelay    time.Duration
	drainBudget   time.Duration

	pgData        string
	pgBinDir      string
	pgUser        string
	pgDatabase    string
	pgPassword    string
	pgExtraArgs   []string
	initScriptDir string
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
			"logger": "pgsupervisor", "msg": msg,
		})
		fmt.Fprintln(os.Stderr, string(entry))
	} else {
		fmt.Fprintf(os.Stderr, "%s %-5s pgsupervisor: %s\n",
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
		port:          envInt("PORT", 5432, 1, 65535),
		adminPort:     envInt("ADMIN_PORT", 9090, 1, 65535),
		bindAddr:      envStr("BIND_ADDR", "0.0.0.0"),
		logLevel:      envEnum("LOG_LEVEL", "info", "trace", "debug", "info", "warn", "error"),
		logFormat:     envEnum("LOG_FORMAT", "json", "json", "text"),
		checkInterval: time.Duration(envInt("HEALTH_CHECK_INTERVAL", 5, 1, 3600)) * time.Second,
		checkTimeout:  time.Duration(envInt("HEALTH_CHECK_TIMEOUT", 3, 1, 3600)) * time.Second,
		staleFactor:   envFloat("HEALTH_STALE_FACTOR", 3, 1, 100),
		drainDelay:    time.Duration(envInt("SHUTDOWN_DRAIN_DELAY", 0, 0, 600)) * time.Second,
		drainBudget:   time.Duration(envInt("SHUTDOWN_TIMEOUT", 30, 1, 3600)) * time.Second,
		pgData:        envStr("PGDATA", "/var/lib/postgresql/data"),
		pgBinDir:      envStr("PG_BINDIR", "/usr/pgsql-16/bin"),
		pgUser:        envStr("POSTGRES_USER", "postgres"),
		pgDatabase:    envStr("POSTGRES_DB", ""),
		initScriptDir: envStr("INITDB_SCRIPT_DIR", "/docker-entrypoint-initdb.d"),
	}
	if cfg.checkTimeout >= cfg.checkInterval {
		fatal(fmt.Sprintf("HEALTH_CHECK_TIMEOUT (%v) must be smaller than HEALTH_CHECK_INTERVAL (%v)",
			cfg.checkTimeout, cfg.checkInterval))
	}
	if cfg.port == cfg.adminPort {
		fatal("PORT and ADMIN_PORT must differ")
	}
	if extra := envStr("POSTGRES_EXTRA_ARGS", ""); extra != "" {
		cfg.pgExtraArgs = strings.Fields(extra)
	}
	if file := envStr("POSTGRES_PASSWORD_FILE", ""); file != "" {
		content, err := os.ReadFile(file)
		if err != nil {
			fatal("POSTGRES_PASSWORD_FILE unreadable: " + err.Error())
		}
		cfg.pgPassword = strings.TrimSpace(string(content))
	} else {
		cfg.pgPassword = os.Getenv("POSTGRES_PASSWORD")
	}
	return cfg
}

func (c *config) redacted() string {
	return fmt.Sprintf("PORT=%d ADMIN_PORT=%d BIND_ADDR=%s LOG_LEVEL=%s LOG_FORMAT=%s "+
		"HEALTH_CHECK_INTERVAL=%v HEALTH_CHECK_TIMEOUT=%v HEALTH_STALE_FACTOR=%v "+
		"SHUTDOWN_DRAIN_DELAY=%v SHUTDOWN_TIMEOUT=%v PGDATA=%s POSTGRES_USER=%s POSTGRES_DB=%s "+
		"POSTGRES_PASSWORD=<redacted>",
		c.port, c.adminPort, c.bindAddr, c.logLevel, c.logFormat,
		c.checkInterval, c.checkTimeout, c.staleFactor, c.drainDelay, c.drainBudget,
		c.pgData, c.pgUser, c.pgDatabase)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(probe(os.Args[2:]))
	}
	cfg := loadConfig()
	log := &logger{level: logLevels[cfg.logLevel], format: cfg.logFormat}
	st := &state{}
	st.snap.Store(snapshot{Results: map[string]checkResult{}})

	startAdmin(cfg, st, log)

	if needsInit(cfg, log) {
		if cfg.pgPassword == "" {
			fatal("first initialization of an empty PGDATA requires POSTGRES_PASSWORD or POSTGRES_PASSWORD_FILE")
		}
		initSignals := make(chan os.Signal, 1)
		signal.Notify(initSignals, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			if _, open := <-initSignals; open {
				log.warnf("shutdown signal during first initialization: wiping partial PGDATA")
				wipePgData(cfg)
				os.Exit(130)
			}
		}()
		heartbeatDone := make(chan struct{})
		go initHeartbeat(cfg, st, heartbeatDone)
		if err := runInitdb(cfg, log); err != nil {
			log.errorf("initdb failed: %v", err)
			wipePgData(cfg)
			os.Exit(1)
		}
		if err := os.WriteFile(filepath.Join(cfg.pgData, initMarker), []byte("provisioning\n"), 0o600); err != nil {
			log.errorf("cannot write init marker: %v", err)
			wipePgData(cfg)
			os.Exit(1)
		}
		if err := runInitScripts(cfg, log); err != nil {
			log.errorf("first-init provisioning failed: %v (marker kept: next start reinitializes)", err)
			os.Exit(1)
		}
		_ = os.Remove(filepath.Join(cfg.pgData, initMarker))
		close(heartbeatDone)
		signal.Stop(initSignals)
		close(initSignals)
	}

	childExited := make(chan int, 1)
	child, err := startPostgres(cfg, log, childExited)
	if err != nil {
		log.errorf("failed to start postgres: %v", err)
		os.Exit(1)
	}
	log.infof("postgresql started: version=%s revision=%s pid=%d config[%s]",
		envStr("APP_VERSION", "dev"), envStr("APP_REVISION", "unknown"), child.Pid, cfg.redacted())

	go checkerLoop(cfg, st, log)

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	select {
	case code := <-childExited:
		log.errorf("postgres exited unexpectedly with code %d", code)
		if code == 0 {
			code = 1
		}
		os.Exit(code)
	case sig := <-signals:
		log.infof("received %v: draining (readiness now 503, drain delay %v)", sig, cfg.drainDelay)
		st.draining.Store(true)
		time.Sleep(cfg.drainDelay)
		begin := time.Now()
		_ = child.Signal(syscall.SIGINT)
		select {
		case code := <-childExited:
			if code != 0 {
				log.warnf("postgres shutdown reported exit code %d", code)
				os.Exit(code)
			}
			log.infof("graceful shutdown complete in %v", time.Since(begin).Round(time.Millisecond))
			os.Exit(0)
		case <-time.After(cfg.drainBudget):
			log.errorf("drain budget of %v exhausted; sending SIGQUIT", cfg.drainBudget)
			_ = child.Signal(syscall.SIGQUIT)
			select {
			case <-childExited:
			case <-time.After(5 * time.Second):
				_ = child.Signal(syscall.SIGKILL)
			}
			os.Exit(1)
		}
	}
}

func needsInit(cfg *config, log *logger) bool {
	if _, err := os.Stat(filepath.Join(cfg.pgData, initMarker)); err == nil {
		log.warnf("previous initialization was interrupted: wiping PGDATA and reinitializing")
		wipePgData(cfg)
		return true
	}
	entries, err := os.ReadDir(cfg.pgData)
	if err != nil {
		if os.IsNotExist(err) {
			// Volume mounts commonly provide only the parent (root-owned mount
			// root, possibly with lost+found); a PGDATA created here is owned by
			// this process, which initdb's permission check requires.
			if mkErr := os.MkdirAll(cfg.pgData, 0o700); mkErr != nil {
				fatal("PGDATA " + cfg.pgData + " does not exist and could not be created (" + mkErr.Error() + "); mount a writable volume at PGDATA or its parent")
			}
			return true
		}
		fatal("PGDATA " + cfg.pgData + " unreadable: " + err.Error())
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") && entry.Name() != "lost+found" {
			return false
		}
	}
	return true
}

func wipePgData(cfg *config) {
	entries, err := os.ReadDir(cfg.pgData)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Name() == "lost+found" {
			continue
		}
		_ = os.RemoveAll(filepath.Join(cfg.pgData, entry.Name()))
	}
}

func initHeartbeat(cfg *config, st *state, done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		st.snap.Store(snapshot{Results: map[string]checkResult{
			"postgresql": {OK: false, Detail: "first initialization in progress"},
		}, TakenAt: time.Now()})
		select {
		case <-done:
			return
		case <-ticker.C:
		}
	}
}

func runInitdb(cfg *config, log *logger) error {
	log.infof("empty PGDATA: running initdb")
	pwFile, err := os.CreateTemp("/tmp", "pgpw")
	if err != nil {
		return err
	}
	defer os.Remove(pwFile.Name())
	if _, err := pwFile.WriteString(cfg.pgPassword + "\n"); err != nil {
		return err
	}
	_ = pwFile.Close()
	cmd := exec.Command(filepath.Join(cfg.pgBinDir, "initdb"),
		"-D", cfg.pgData, "-U", cfg.pgUser, "--pwfile", pwFile.Name(),
		"--encoding=UTF8", "--locale=C.UTF-8",
		"--auth-host=scram-sha-256", "--auth-local=scram-sha-256")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	// initdb's template only covers localhost; without this rule every client
	// from another container is rejected by pg_hba (same append the official
	// image performs, password-authenticated via scram).
	hba, err := os.OpenFile(filepath.Join(cfg.pgData, "pg_hba.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := hba.WriteString("host all all all scram-sha-256\n"); err != nil {
		_ = hba.Close()
		return err
	}
	return hba.Close()
}

// runInitScripts mirrors the conventional first-init flow: a socket-only
// postgres, optional POSTGRES_DB creation, *.sql scripts from the init
// directory, then a clean stop before the real server starts.
func runInitScripts(cfg *config, log *logger) error {
	scripts := []string{}
	if entries, err := os.ReadDir(cfg.initScriptDir); err == nil {
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".sql") {
				scripts = append(scripts, filepath.Join(cfg.initScriptDir, entry.Name()))
			}
		}
		sort.Strings(scripts)
	}
	needDatabase := cfg.pgDatabase != "" && cfg.pgDatabase != "postgres"
	if !needDatabase && len(scripts) == 0 {
		return nil
	}

	log.infof("first-init provisioning: database=%v scripts=%d", needDatabase, len(scripts))
	cmd := exec.Command(filepath.Join(cfg.pgBinDir, "postgres"),
		"-D", cfg.pgData, "-c", "listen_addresses=", "-c", "unix_socket_directories=/tmp")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGINT)
		_, _ = cmd.Process.Wait()
	}()

	env := append(os.Environ(), "PGPASSWORD="+cfg.pgPassword)
	psql := func(database string, args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		full := append([]string{"-v", "ON_ERROR_STOP=1", "-h", "/tmp", "-U", cfg.pgUser, "-d", database}, args...)
		run := exec.CommandContext(ctx, filepath.Join(cfg.pgBinDir, "psql"), full...)
		run.Env = env
		run.Stdout = os.Stderr
		run.Stderr = os.Stderr
		return run.Run()
	}

	ready := false
	for attempt := 0; attempt < 60; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := exec.CommandContext(ctx, filepath.Join(cfg.pgBinDir, "pg_isready"),
			"-h", "/tmp", "-U", cfg.pgUser).Run()
		cancel()
		if err == nil {
			ready = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		return fmt.Errorf("socket-only postgres did not become ready for provisioning")
	}

	if needDatabase {
		quoted := strings.ReplaceAll(cfg.pgDatabase, "\"", "\"\"")
		if err := psql("postgres", "-c", "CREATE DATABASE \""+quoted+"\""); err != nil {
			return err
		}
	}
	target := cfg.pgDatabase
	if target == "" {
		target = "postgres"
	}
	for _, script := range scripts {
		log.infof("running init script %s", script)
		if err := psql(target, "-f", script); err != nil {
			return err
		}
	}
	return nil
}

func startPostgres(cfg *config, log *logger, exited chan<- int) (*os.Process, error) {
	args := []string{
		"-D", cfg.pgData,
		"-c", "listen_addresses=" + cfg.bindAddr,
		"-c", "port=" + strconv.Itoa(cfg.port),
		"-c", "unix_socket_directories=/tmp",
		"-c", "log_destination=stderr",
	}
	args = append(args, cfg.pgExtraArgs...)
	args = append(args, os.Args[1:]...)
	cmd := exec.Command(filepath.Join(cfg.pgBinDir, "postgres"), args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	mainPid := cmd.Process.Pid

	// Sole wait4 owner: reaps the postmaster and any orphan reparented to PID 1.
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
	host := probeHost(cfg.bindAddr)
	previous := map[string]checkResult{}
	for {
		cycleStart := time.Now()
		results := map[string]checkResult{}
		if err := checkPostgres(host, cfg.port, cfg.pgUser, cfg.checkTimeout); err == nil {
			results["postgresql"] = checkResult{OK: true, Detail: "ok"}
		} else {
			results["postgresql"] = checkResult{OK: false, Detail: err.Error()}
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

// checkPostgres performs the pg_isready handshake in-process — no child
// process, so the PID-1 reaper stays the sole wait4 owner: send a v3
// StartupMessage, expect an authentication request ('R') back; anything else
// (e.g. ErrorResponse while the server is starting or shutting down) fails.
func checkPostgres(host string, port int, user string, timeout time.Duration) error {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	payload := []byte{0, 3, 0, 0}
	payload = append(payload, []byte("user\x00")...)
	payload = append(payload, []byte(user)...)
	payload = append(payload, 0)
	payload = append(payload, []byte("database\x00postgres\x00\x00")...)
	length := uint32(len(payload) + 4)
	message := []byte{byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
	message = append(message, payload...)
	if _, err := conn.Write(message); err != nil {
		return err
	}
	kind := make([]byte, 1)
	if _, err := io.ReadFull(conn, kind); err != nil {
		return err
	}
	if kind[0] != 'R' {
		return fmt.Errorf("postgres not accepting connections (response %q)", kind[0])
	}
	return nil
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
