package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Cached health checker (container build standard section 6): one loop owns
// the real checks, the admin endpoints only read the last atomic snapshot.

type checkResult struct {
	OK          bool          `json:"ok"`
	Detail      string        `json:"detail"`
	Duration    time.Duration `json:"-"`
	LastSuccess time.Time     `json:"-"`
}

type snapshot struct {
	Results map[string]checkResult `json:"checks"`
	TakenAt time.Time              `json:"takenAt"`
}

type state struct {
	snap     atomic.Value
	started  atomic.Bool
	draining atomic.Bool
	cycles   atomic.Uint64
}

func newState() *state {
	st := &state{}
	st.snap.Store(snapshot{Results: map[string]checkResult{}})
	return st
}

func (st *state) snapshot() snapshot { return st.snap.Load().(snapshot) }

func (st *state) fresh(cfg *config) bool {
	snap := st.snapshot()
	return !snap.TakenAt.IsZero() &&
		time.Since(snap.TakenAt) <= time.Duration(cfg.staleFactor*float64(cfg.checkInterval))
}

func (st *state) ready(cfg *config) bool {
	return st.fresh(cfg) && allOk(st.snapshot().Results) && !st.draining.Load()
}

func allOk(results map[string]checkResult) bool {
	for _, result := range results {
		if !result.OK {
			return false
		}
	}
	return len(results) > 0
}

func selfChecksOk(checks []healthCheck, results map[string]checkResult) bool {
	for _, check := range checks {
		if !check.dependency && !results[check.name].OK {
			return false
		}
	}
	return true
}

// A dependency check gates /readyz only; the self checks gate /startupz
// as well, so an unreachable backend never holds startup back.
type healthCheck struct {
	name       string
	dependency bool
	run        func(ctx context.Context) error
}

// registeredChecks derives the checks from the service's function: the bundle
// it serves must be present (a self check), and the backend it proxies to
// must be reachable (a dependency; any HTTP answer counts, including 401/403).
func registeredChecks(cfg *config) []healthCheck {
	client := &http.Client{Timeout: cfg.checkTimeout}
	return []healthCheck{
		{name: "static-root", run: func(ctx context.Context) error {
			_, err := os.Stat(filepath.Join(cfg.staticDir, "index.html"))
			if err != nil {
				return fmt.Errorf("index.html missing: %w", err)
			}
			return nil
		}},
		{name: "backend-api", dependency: true, run: func(ctx context.Context) error {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.apiURL+"/users", nil)
			if err != nil {
				return err
			}
			response, err := client.Do(request)
			if err != nil {
				return err
			}
			_ = response.Body.Close()
			return nil
		}},
	}
}

func checkerLoop(cfg *config, st *state, log *logger, checks []healthCheck) {
	previous := map[string]checkResult{}
	for {
		cycleStart := time.Now()
		results := runCycle(cfg, checks, previous)
		st.snap.Store(snapshot{Results: results, TakenAt: time.Now()})
		st.cycles.Add(1)
		if !st.started.Load() && selfChecksOk(checks, results) {
			st.started.Store(true)
			log.infof("startup complete: self checks passed")
		}
		for name, result := range results {
			if before, seen := previous[name]; seen && before.OK != result.OK {
				log.log("info", "server", fmt.Sprintf("health check '%s' transitioned %v -> %v (%s)",
					name, before.OK, result.OK, result.Detail), fields{"health.check": name, "health.up": result.OK})
			}
		}
		previous = results
		if sleep := cfg.checkInterval - time.Since(cycleStart); sleep > 0 {
			time.Sleep(sleep)
		}
	}
}

// runCycle executes every check concurrently, each bounded by the per-check
// timeout, and carries the last success time over when a check fails.
func runCycle(cfg *config, checks []healthCheck, previous map[string]checkResult) map[string]checkResult {
	results := make(map[string]checkResult, len(checks))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, check := range checks {
		wg.Add(1)
		go func(check healthCheck) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), cfg.checkTimeout)
			defer cancel()
			start := time.Now()
			done := make(chan error, 1)
			go func() { done <- check.run(ctx) }()
			var err error
			select {
			case err = <-done:
			case <-ctx.Done():
				err = fmt.Errorf("timed out after %v", cfg.checkTimeout)
			}
			result := checkResult{Duration: time.Since(start), LastSuccess: previous[check.name].LastSuccess}
			if err == nil {
				result.OK, result.Detail, result.LastSuccess = true, "ok", time.Now()
			} else {
				result.Detail = err.Error()
			}
			mu.Lock()
			results[check.name] = result
			mu.Unlock()
		}(check)
	}
	wg.Wait()
	return results
}

// ---------------------------------------------------------------------------

func startAdmin(cfg *config, st *state, log *logger, m *metrics) *http.Server {
	handler := func(up func() bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ok := up()
			status := http.StatusOK
			if !ok {
				status = http.StatusServiceUnavailable
			}
			if r.URL.Query().Get("verbose") == "1" {
				snap := st.snapshot()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				body, _ := json.Marshal(map[string]any{
					"status":   map[bool]string{true: "ok", false: "unavailable"}[ok],
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
	mux.HandleFunc("/livez", handler(func() bool { return st.fresh(cfg) }))
	mux.HandleFunc("/readyz", handler(func() bool { return st.ready(cfg) }))
	mux.Handle("/metrics", m.handler())
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.bindAddr, strconv.Itoa(cfg.adminPort)))
	if err != nil {
		fatal("cannot bind admin listener: " + err.Error())
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ConnState: m.connState("admin")}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.errorf("admin server failed: %v", err)
			os.Exit(1)
		}
	}()
	return server
}
