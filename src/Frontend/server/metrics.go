package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/common/expfmt"
)

// Metrics contract of the stack (README "Metrics contract"): the baseline
// families from the health snapshot, the route families of the main listener,
// the client families of the backend hop, and the connection gauges. Label
// values are closed enumerations: routes are templates, never raw paths.

// The shared latency buckets of every server in the stack (Traefik, backend,
// this server and the Node variant), so edge and server percentiles compare.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

const (
	routeUnknown            = "UNKNOWN"
	routeStatic             = "/static/**"
	routeModuleSubscription = "/api/users/{id}/modules/{moduleId}"
	peerBackend             = "backend"
)

// Route classes of the main listener, in match order.
var namedRoutes = []string{"/", "/login", "/signup", "/dashboard", "/api/login", "/api/logout", "/api/me", "/api/signup", "/api/modules"}

// Backend route templates the proxied calls map to (the backend's own `uri`
// values, so both ends of the hop carry the same label): the exact ones, then
// the ones with ids.
var backendRoutes = []string{"/users/register", "/users/login", "/users/me", "/users", "/modules"}

var backendIDRoutes = []string{"/users/{id}", "/users/{id}/modules/{moduleId}"}

var knownMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}

var (
	uuidPattern      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

type metrics struct {
	registry *prometheus.Registry

	serverRequests  *prometheus.HistogramVec
	serverActive    *prometheus.GaugeVec
	serverReqBytes  *prometheus.CounterVec
	serverRespBytes *prometheus.CounterVec

	clientRequests  *prometheus.HistogramVec
	clientActive    *prometheus.GaugeVec
	clientReqBytes  *prometheus.CounterVec
	clientRespBytes *prometheus.CounterVec

	connections *prometheus.GaugeVec
}

func newMetrics(cfg *config, st *state) *metrics {
	m := &metrics{registry: prometheus.NewRegistry()}
	m.serverRequests = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "http_server_requests_seconds", Help: "Duration of requests served on the main listener, by route template.",
		Buckets: latencyBuckets}, []string{"method", "uri", "status", "outcome", "exception"})
	m.serverActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "http_server_requests_active", Help: "Requests currently in flight on the main listener."},
		[]string{"method", "uri"})
	m.serverReqBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_server_request_bytes_total", Help: "Request body bytes read on the main listener, by route template."}, []string{"uri"})
	m.serverRespBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_server_response_bytes_total", Help: "Response body bytes written on the main listener, by route template."}, []string{"uri"})
	m.clientRequests = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "http_client_requests_seconds", Help: "Duration of proxied calls to a peer component, by the peer's route template.",
		Buckets: latencyBuckets}, []string{"peer", "method", "uri", "status", "outcome"})
	m.clientActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "http_client_requests_active", Help: "Proxied calls currently in flight."}, []string{"peer", "method", "uri"})
	m.clientReqBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_client_request_bytes_total", Help: "Request body bytes sent to a peer component."}, []string{"peer", "uri"})
	m.clientRespBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_client_response_bytes_total", Help: "Response body bytes received from a peer component."}, []string{"peer", "uri"})
	m.connections = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "http_server_connections_active", Help: "Open TCP connections per listener."}, []string{"listener"})

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		&opsCollector{cfg: cfg, st: st},
		m.serverRequests, m.serverActive, m.serverReqBytes, m.serverRespBytes,
		m.clientRequests, m.clientActive, m.clientReqBytes, m.clientRespBytes,
		m.connections,
	)
	// Counters exist from the first scrape for every route class, so rate()
	// works before the first request and the enumeration is visible.
	for _, route := range append(append([]string{}, namedRoutes...), routeModuleSubscription, routeStatic, routeUnknown) {
		m.serverReqBytes.WithLabelValues(route)
		m.serverRespBytes.WithLabelValues(route)
	}
	for _, route := range append(append(append([]string{}, backendRoutes...), backendIDRoutes...), routeUnknown) {
		m.clientReqBytes.WithLabelValues(peerBackend, route)
		m.clientRespBytes.WithLabelValues(peerBackend, route)
	}
	m.connections.WithLabelValues("main")
	m.connections.WithLabelValues("admin")
	return m
}

// handler serves OpenMetrics unconditionally (no content negotiation): the
// media type, `# TYPE` per family and the `# EOF` terminator are the contract.
func (m *metrics) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		families, err := m.registry.Gather()
		if err != nil {
			http.Error(w, "gathering metrics failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		encoder := expfmt.NewEncoder(w, expfmt.NewFormat(expfmt.TypeOpenMetrics))
		for _, family := range families {
			if err := encoder.Encode(family); err != nil {
				return
			}
		}
		if closer, ok := encoder.(expfmt.Closer); ok {
			_ = closer.Close()
		}
	})
}

// connState feeds http_server_connections_active{listener} from the
// net/http connection state hook.
func (m *metrics) connState(listener string) func(net.Conn, http.ConnState) {
	gauge := m.connections.WithLabelValues(listener)
	return func(_ net.Conn, cs http.ConnState) {
		switch cs {
		case http.StateNew:
			gauge.Inc()
		case http.StateClosed, http.StateHijacked:
			gauge.Dec()
		}
	}
}

// ---------------------------------------------------------------------------
// baseline families read from the health snapshot

type opsCollector struct {
	cfg *config
	st  *state

	buildInfo   *prometheus.Desc
	up          *prometheus.Desc
	duration    *prometheus.Desc
	lastSuccess *prometheus.Desc
	age         *prometheus.Desc
	cycles      *prometheus.Desc
	draining    *prometheus.Desc
}

func (c *opsCollector) descs() []*prometheus.Desc {
	if c.buildInfo == nil {
		c.buildInfo = prometheus.NewDesc("build_info", "The running artifact; always 1.", []string{"version", "revision"}, nil)
		c.up = prometheus.NewDesc("health_check_up", "1 when the check passed in the last checker cycle.", []string{"check"}, nil)
		c.duration = prometheus.NewDesc("health_check_duration_seconds", "Duration of the last run of the check.", []string{"check"}, nil)
		c.lastSuccess = prometheus.NewDesc("health_check_last_success_timestamp_seconds", "Unix time of the last pass of the check; 0 until it passed once.", []string{"check"}, nil)
		c.age = prometheus.NewDesc("health_snapshot_age_seconds", "Age of the cached health snapshot; staleness flips liveness.", nil, nil)
		c.cycles = prometheus.NewDesc("health_checker_cycles_total", "Completed health checker cycles.", nil, nil)
		c.draining = prometheus.NewDesc("health_draining", "1 once the shutdown drain latch is set (readiness 503).", nil, nil)
	}
	return []*prometheus.Desc{c.buildInfo, c.up, c.duration, c.lastSuccess, c.age, c.cycles, c.draining}
}

func (c *opsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.descs() {
		ch <- d
	}
}

func (c *opsCollector) Collect(ch chan<- prometheus.Metric) {
	c.descs()
	snap := c.st.snapshot()
	ch <- prometheus.MustNewConstMetric(c.buildInfo, prometheus.GaugeValue, 1,
		envStr("APP_VERSION", "dev"), envStr("APP_REVISION", "unknown"))
	for name, result := range snap.Results {
		up := 0.0
		if result.OK {
			up = 1
		}
		last := 0.0
		if !result.LastSuccess.IsZero() {
			last = float64(result.LastSuccess.UnixNano()) / 1e9
		}
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, up, name)
		ch <- prometheus.MustNewConstMetric(c.duration, prometheus.GaugeValue, result.Duration.Seconds(), name)
		ch <- prometheus.MustNewConstMetric(c.lastSuccess, prometheus.GaugeValue, last, name)
	}
	age := 0.0
	if !snap.TakenAt.IsZero() {
		age = time.Since(snap.TakenAt).Seconds()
	}
	ch <- prometheus.MustNewConstMetric(c.age, prometheus.GaugeValue, age)
	ch <- prometheus.MustNewConstMetric(c.cycles, prometheus.CounterValue, float64(c.st.cycles.Load()))
	draining := 0.0
	if c.st.draining.Load() {
		draining = 1
	}
	ch <- prometheus.MustNewConstMetric(c.draining, prometheus.GaugeValue, draining)
}

// ---------------------------------------------------------------------------
// request context: id and upstream timing shared by the middleware, the
// backend client and the access log

type requestInfo struct {
	id       string
	mu       sync.Mutex
	upstream time.Duration
}

type requestInfoKey struct{}

func infoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return info
}

// requestID keeps a well-formed incoming X-Request-Id and generates a UUIDv4
// otherwise; the id is echoed, forwarded to the backend and logged.
func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); requestIDPattern.MatchString(id) {
		return id
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func normalizeMethod(method string) string {
	if knownMethods[method] {
		return method
	}
	return "OTHER"
}

func outcomeOf(status int) string {
	switch {
	case status >= 100 && status < 200:
		return "INFORMATIONAL"
	case status >= 200 && status < 300:
		return "SUCCESS"
	case status >= 300 && status < 400:
		return "REDIRECTION"
	case status >= 400 && status < 500:
		return "CLIENT_ERROR"
	case status >= 500 && status < 600:
		return "SERVER_ERROR"
	}
	return "UNKNOWN"
}

// routeClass maps a request path to its route template. Named routes first,
// then everything the exported bundle resolves (the static resolution of
// handleStatic) as /static/**, everything else UNKNOWN - a scan cannot create
// series.
func routeClass(cfg *config, requestPath string) string {
	clean := path.Clean("/" + requestPath)
	for _, route := range namedRoutes {
		if clean == route {
			return route
		}
	}
	if rest, ok := strings.CutPrefix(clean, "/api/users/"); ok {
		id, moduleID, found := strings.Cut(rest, "/modules/")
		if found && uuidPattern.MatchString(id) && uuidPattern.MatchString(moduleID) {
			return routeModuleSubscription
		}
	}
	if strings.HasPrefix(clean, "/dashboard/") {
		return "/dashboard"
	}
	if strings.HasPrefix(clean, "/_next/") {
		return routeStatic
	}
	if _, ok := resolveStatic(cfg.staticDir, clean); ok {
		return routeStatic
	}
	return routeUnknown
}

// resolveStatic maps a clean URL path to a file of the exported bundle:
// the path itself, its directory index, or the path with .html appended.
func resolveStatic(staticDir, clean string) (string, bool) {
	full := filepath.Join(staticDir, filepath.FromSlash(clean))
	info, err := os.Stat(full)
	if err == nil && info.IsDir() {
		full = filepath.Join(full, "index.html")
		info, err = os.Stat(full)
	}
	if err != nil && path.Ext(clean) == "" {
		full = filepath.Join(staticDir, filepath.FromSlash(clean)+".html")
		info, err = os.Stat(full)
	}
	if err != nil || info.IsDir() {
		return "", false
	}
	return full, true
}

// backendRoute maps a backend request path to the backend's route template.
func backendRoute(requestPath string) string {
	clean := path.Clean("/" + requestPath)
	for _, route := range backendRoutes {
		if clean == route {
			return route
		}
	}
	if rest, ok := strings.CutPrefix(clean, "/users/"); ok {
		id, moduleID, nested := strings.Cut(rest, "/modules/")
		switch {
		case !nested && uuidPattern.MatchString(rest):
			return "/users/{id}"
		case nested && uuidPattern.MatchString(id) && uuidPattern.MatchString(moduleID):
			return "/users/{id}/modules/{moduleId}"
		}
	}
	return routeUnknown
}

// ---------------------------------------------------------------------------
// main-listener instrumentation

type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// instrument wraps the main-listener handler: route class, request id,
// in-flight gauge, timing histogram, byte counters and the access log line.
func instrument(cfg *config, log *logger, m *metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		uri := routeClass(cfg, r.URL.Path)
		method := normalizeMethod(r.Method)
		info := &requestInfo{id: requestID(r)}
		r.Header.Set("X-Request-Id", info.id)
		w.Header().Set("X-Request-Id", info.id)

		active := m.serverActive.WithLabelValues(method, uri)
		active.Inc()
		defer active.Dec()

		body := &countingBody{ReadCloser: r.Body}
		r.Body = body
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		exception := "none"
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					exception = "panic"
					if !recorder.wrote {
						recorder.WriteHeader(http.StatusInternalServerError)
					}
					log.log("error", "server", fmt.Sprintf("handler panic: %v", recovered), fields{"http.request.id": info.id, "url.path": uri})
				}
			}()
			next.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info)))
		}()

		elapsed := time.Since(start)
		status := recorder.status
		m.serverRequests.WithLabelValues(method, uri, strconv.Itoa(status), outcomeOf(status), exception).Observe(elapsed.Seconds())
		m.serverReqBytes.WithLabelValues(uri).Add(float64(body.n))
		m.serverRespBytes.WithLabelValues(uri).Add(float64(recorder.bytes))

		if cfg.accessLog {
			entry := fields{
				"http.request.id":           info.id,
				"http.request.method":       r.Method,
				"url.path":                  uri,
				"http.response.status_code": status,
				"event.duration":            elapsed.Nanoseconds(),
				"http.request.bytes":        body.n,
				"http.response.bytes":       recorder.bytes,
			}
			info.mu.Lock()
			if info.upstream > 0 {
				entry["upstream.duration"] = info.upstream.Nanoseconds()
			}
			info.mu.Unlock()
			log.log("info", "access", fmt.Sprintf("%s %s %d", r.Method, uri, status), entry)
		}
	})
}

// ---------------------------------------------------------------------------
// backend-hop instrumentation (peer="backend")

type instrumentedTransport struct {
	m    *metrics
	next http.RoundTripper
}

func (t *instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	uri := backendRoute(req.URL.Path)
	method := normalizeMethod(req.Method)
	info := infoFrom(req.Context())
	if info != nil && req.Header.Get("X-Request-Id") == "" {
		req.Header.Set("X-Request-Id", info.id)
	}
	active := t.m.clientActive.WithLabelValues(peerBackend, method, uri)
	active.Inc()
	defer active.Dec()

	var sent int64
	if req.Body != nil && req.Body != http.NoBody {
		counter := &countingBody{ReadCloser: req.Body}
		req.Body = counter
		defer func() { t.m.clientReqBytes.WithLabelValues(peerBackend, uri).Add(float64(counter.n)) }()
	} else if req.ContentLength > 0 {
		sent = req.ContentLength
	}
	if sent > 0 {
		t.m.clientReqBytes.WithLabelValues(peerBackend, uri).Add(float64(sent))
	}

	start := time.Now()
	response, err := t.next.RoundTrip(req)
	elapsed := time.Since(start)
	if info != nil {
		info.mu.Lock()
		info.upstream += elapsed
		info.mu.Unlock()
	}
	if err != nil {
		t.m.clientRequests.WithLabelValues(peerBackend, method, uri, "IO_ERROR", "UNKNOWN").Observe(elapsed.Seconds())
		return nil, err
	}
	t.m.clientRequests.WithLabelValues(peerBackend, method, uri, strconv.Itoa(response.StatusCode), outcomeOf(response.StatusCode)).Observe(elapsed.Seconds())
	counter := t.m.clientRespBytes.WithLabelValues(peerBackend, uri)
	response.Body = &countingResponseBody{ReadCloser: response.Body, counter: counter}
	return response, nil
}

// countingResponseBody adds the bytes read from a peer response to the
// counter once, when the body is closed.
type countingResponseBody struct {
	io.ReadCloser
	counter prometheus.Counter
	n       int64
	closed  bool
}

func (b *countingResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

func (b *countingResponseBody) Close() error {
	if !b.closed {
		b.closed = true
		b.counter.Add(float64(b.n))
	}
	return b.ReadCloser.Close()
}
