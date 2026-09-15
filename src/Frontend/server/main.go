// auth-portal edge server: serves the statically exported Next.js bundle and
// implements the app's four server-side auth endpoints (login/logout/me/signup
// proxying with the httpOnly jwt cookie) plus the auth redirects that upstream
// implemented as Next middleware. In-process checker topology per the container
// build standard: probe endpoints, cached health checker, signal-driven drain,
// the exec-style probe subcommand, the metrics contract of the stack and the
// ECS access log all live in this single static binary.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	port          int
	adminPort     int
	bindAddr      string
	logLevel      string
	logFormat     string
	accessLog     bool
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
		accessLog:     envBool("ACCESS_LOG", true),
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
	return fmt.Sprintf("PORT=%d ADMIN_PORT=%d BIND_ADDR=%s LOG_LEVEL=%s LOG_FORMAT=%s ACCESS_LOG=%v "+
		"HEALTH_CHECK_INTERVAL=%v HEALTH_CHECK_TIMEOUT=%v HEALTH_STALE_FACTOR=%v "+
		"SHUTDOWN_DRAIN_DELAY=%v SHUTDOWN_TIMEOUT=%v API_URL=%s API_TIMEOUT=%v "+
		"COOKIE_SECURE=%v COOKIE_MAX_AGE=%d STATIC_DIR=%s",
		c.port, c.adminPort, c.bindAddr, c.logLevel, c.logFormat, c.accessLog,
		c.checkInterval, c.checkTimeout, c.staleFactor, c.drainDelay, c.drainBudget,
		c.apiURL, c.apiTimeout, c.cookieSecure, c.cookieMaxAge, c.staticDir)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(probe(os.Args[2:]))
	}
	cfg := loadConfig()
	log := newLogger(cfg.logLevel, cfg.logFormat)
	st := newState()
	m := newMetrics(cfg, st)

	startAdmin(cfg, st, log, m)
	go checkerLoop(cfg, st, log, registeredChecks(cfg))

	appServer := &http.Server{
		Handler:           newAppHandler(cfg, log, m),
		ReadHeaderTimeout: 10 * time.Second,
		ConnState:         m.connState("main"),
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
	log.log("info", "server", fmt.Sprintf("auth-portal listening: version=%s revision=%s config[%s]",
		envStr("APP_VERSION", "dev"), envStr("APP_REVISION", "unknown"), cfg.redacted()),
		fields{"service.revision": envStr("APP_REVISION", "unknown"), "process.pid": os.Getpid()})

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
