// auth-portal edge server: serves the statically exported Next.js bundle and
// implements the app's four server-side auth endpoints (login/logout/me/signup
// proxying with the httpOnly jwt cookie) plus the auth redirects that upstream
// implemented as Next middleware. In-process checker topology per the container
// build standard: probe endpoints, cached health checker, signal-driven drain,
// and the exec-style probe subcommand all live in this single static binary.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

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

	apiURL       string
	apiTimeout   time.Duration
	cookieSecure bool
	cookieMaxAge int
	staticDir    string
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
			"logger": "auth-portal-server", "msg": msg,
		})
		fmt.Fprintln(os.Stderr, string(entry))
	} else {
		fmt.Fprintf(os.Stderr, "%s %-5s auth-portal-server: %s\n",
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

func envBool(name string, fallback bool) bool {
	raw := strings.ToLower(envStr(name, strconv.FormatBool(fallback)))
	switch raw {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	fatal(name + "=" + raw + " is not a boolean")
	return false
}

func loadConfig() *config {
	cfg := &config{
		port:          envInt("PORT", 3000, 1, 65535),
		adminPort:     envInt("ADMIN_PORT", 9090, 1, 65535),
		bindAddr:      envStr("BIND_ADDR", "0.0.0.0"),
		logLevel:      envEnum("LOG_LEVEL", "info", "trace", "debug", "info", "warn", "error"),
		logFormat:     envEnum("LOG_FORMAT", "json", "json", "text"),
		checkInterval: time.Duration(envInt("HEALTH_CHECK_INTERVAL", 5, 1, 3600)) * time.Second,
		checkTimeout:  time.Duration(envInt("HEALTH_CHECK_TIMEOUT", 2, 1, 3600)) * time.Second,
		staleFactor:   envFloat("HEALTH_STALE_FACTOR", 3, 1, 100),
		drainDelay:    time.Duration(envInt("SHUTDOWN_DRAIN_DELAY", 3, 0, 600)) * time.Second,
		drainBudget:   time.Duration(envInt("SHUTDOWN_TIMEOUT", 10, 1, 3600)) * time.Second,
		apiURL:        envStr("API_URL", ""),
		apiTimeout:    time.Duration(envInt("API_TIMEOUT", 30, 1, 600)) * time.Second,
		cookieSecure:  envBool("COOKIE_SECURE", true),
		cookieMaxAge:  envInt("COOKIE_MAX_AGE", 604800, 60, 31536000),
		staticDir:     envStr("STATIC_DIR", "/app/static"),
	}
	if cfg.checkTimeout >= cfg.checkInterval {
		fatal(fmt.Sprintf("HEALTH_CHECK_TIMEOUT (%v) must be smaller than HEALTH_CHECK_INTERVAL (%v)",
			cfg.checkTimeout, cfg.checkInterval))
	}
	if cfg.port == cfg.adminPort {
		fatal("PORT and ADMIN_PORT must differ")
	}
	if cfg.apiURL == "" {
		fatal("API_URL is required (base URL of the user-mgmt-service backend)")
	}
	cfg.apiURL = strings.TrimRight(cfg.apiURL, "/")
	if !strings.HasPrefix(cfg.apiURL, "http://") && !strings.HasPrefix(cfg.apiURL, "https://") {
		fatal("API_URL=" + cfg.apiURL + " must be an http(s) URL")
	}
	if info, err := os.Stat(cfg.staticDir); err != nil || !info.IsDir() {
		fatal("STATIC_DIR " + cfg.staticDir + " is not a readable directory")
	}
	return cfg
}

func (c *config) redacted() string {
	return fmt.Sprintf("PORT=%d ADMIN_PORT=%d BIND_ADDR=%s LOG_LEVEL=%s LOG_FORMAT=%s "+
		"HEALTH_CHECK_INTERVAL=%v HEALTH_CHECK_TIMEOUT=%v HEALTH_STALE_FACTOR=%v "+
		"SHUTDOWN_DRAIN_DELAY=%v SHUTDOWN_TIMEOUT=%v API_URL=%s API_TIMEOUT=%v "+
		"COOKIE_SECURE=%v COOKIE_MAX_AGE=%d STATIC_DIR=%s",
		c.port, c.adminPort, c.bindAddr, c.logLevel, c.logFormat,
		c.checkInterval, c.checkTimeout, c.staleFactor, c.drainDelay, c.drainBudget,
		c.apiURL, c.apiTimeout, c.cookieSecure, c.cookieMaxAge, c.staticDir)
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
	go checkerLoop(cfg, st, log)

	appServer := &http.Server{
		Handler:           newAppHandler(cfg, log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.bindAddr, strconv.Itoa(cfg.port)))
	if err != nil {
		log.errorf("cannot bind main listener: %v", err)
		os.Exit(1)
	}
	go func() {
		if err := appServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.errorf("main server failed: %v", err)
			os.Exit(1)
		}
	}()
	log.infof("auth-portal listening: version=%s revision=%s config[%s]",
		envStr("APP_VERSION", "dev"), envStr("APP_REVISION", "unknown"), cfg.redacted())

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	sig := <-signals
	log.infof("received %v: draining (readiness now 503, drain delay %v)", sig, cfg.drainDelay)
	st.draining.Store(true)
	time.Sleep(cfg.drainDelay)
	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.drainBudget)
	defer cancel()
	if err := appServer.Shutdown(ctx); err != nil {
		log.errorf("drain budget of %v exhausted: %v; closing remaining connections", cfg.drainBudget, err)
		_ = appServer.Close()
		os.Exit(1)
	}
	log.infof("graceful shutdown complete in %v", time.Since(begin).Round(time.Millisecond))
	os.Exit(0)
}

// ---------------------------------------------------------------------------

func newAppHandler(cfg *config, log *logger) http.Handler {
	client := &http.Client{Timeout: cfg.apiTimeout}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		handleLogin(cfg, client, w, r)
	})
	mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		handleLogout(cfg, w, r)
	})
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		handleMe(cfg, client, w, r)
	})
	mux.HandleFunc("/api/signup", func(w http.ResponseWriter, r *http.Request) {
		handleSignup(cfg, client, w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handleStatic(cfg, w, r)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func hasJwtCookie(r *http.Request) bool {
	cookie, err := r.Cookie("jwt")
	return err == nil && cookie.Value != ""
}

func setJwtCookie(cfg *config, w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     "jwt",
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cfg.cookieSecure,
	})
}

func handleLogin(cfg *config, client *http.Client, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid body"})
		return
	}
	response, err := client.Post(cfg.apiURL+"/users/login", "application/json", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Server error"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Invalid credentials"})
		return
	}
	token := strings.TrimPrefix(response.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "No token"})
		return
	}
	setJwtCookie(cfg, w, token, cfg.cookieMaxAge)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func handleLogout(cfg *config, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	setJwtCookie(cfg, w, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func handleMe(cfg *config, client *http.Client, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	cookie, err := r.Cookie("jwt")
	if err != nil || cookie.Value == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return
	}
	request, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, cfg.apiURL+"/users/me", nil)
	request.Header.Set("Authorization", "Bearer "+cookie.Value)
	response, err := client.Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Failed to fetch user"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		writeJSON(w, response.StatusCode, map[string]any{"error": "Failed to fetch user"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, response.Body)
}

func handleSignup(cfg *config, client *http.Client, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid body"})
		return
	}
	response, err := client.Post(cfg.apiURL+"/users/register", "application/json", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Server error"})
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

// handleStatic serves the exported bundle and reproduces the upstream Next
// middleware: "/" redirects to /dashboard, /dashboard* requires the jwt
// cookie, /login bounces authenticated users back to the dashboard.
func handleStatic(cfg *config, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method not allowed"})
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	authed := hasJwtCookie(r)
	switch {
	case clean == "/":
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	case clean == "/login" && authed:
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	case (clean == "/dashboard" || strings.HasPrefix(clean, "/dashboard/")) && !authed:
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	full := filepath.Join(cfg.staticDir, filepath.FromSlash(clean))
	info, err := os.Stat(full)
	if err == nil && info.IsDir() {
		full = filepath.Join(full, "index.html")
		info, err = os.Stat(full)
	}
	if err != nil && path.Ext(clean) == "" {
		full = filepath.Join(cfg.staticDir, filepath.FromSlash(clean)+".html")
		info, err = os.Stat(full)
	}
	if err != nil || info.IsDir() {
		notFound := filepath.Join(cfg.staticDir, "404.html")
		if _, statErr := os.Stat(notFound); statErr == nil {
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusNotFound)
			content, _ := os.ReadFile(notFound)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(content)
			return
		}
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(clean, "/_next/static/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if strings.HasSuffix(full, ".html") || strings.HasSuffix(full, ".txt") {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeFile(w, r, full)
}

// ---------------------------------------------------------------------------

func checkerLoop(cfg *config, st *state, log *logger) {
	client := &http.Client{Timeout: cfg.checkTimeout}
	previous := map[string]checkResult{}
	for {
		cycleStart := time.Now()
		results := map[string]checkResult{}

		if _, err := os.Stat(filepath.Join(cfg.staticDir, "index.html")); err == nil {
			results["static-root"] = checkResult{OK: true, Detail: "ok"}
		} else {
			results["static-root"] = checkResult{OK: false, Detail: "index.html missing: " + err.Error()}
		}

		response, err := client.Get(cfg.apiURL + "/users")
		if err != nil {
			results["backend-api"] = checkResult{OK: false, Detail: err.Error()}
		} else {
			_ = response.Body.Close()
			results["backend-api"] = checkResult{OK: true, Detail: "ok"}
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

func probeHost(addr string) string {
	switch addr {
	case "", "0.0.0.0", "*":
		return "127.0.0.1"
	case "::":
		return "::1"
	}
	return addr
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
