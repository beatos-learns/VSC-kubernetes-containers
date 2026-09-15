package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/common/expfmt"
)

const openMetricsContentType = "application/openmetrics-text; version=1.0.0; charset=utf-8"

// pgActivityStates and pgLockModes are the fixed label enumerations of
// pg_stat_activity_backends and pg_locks: every value is always emitted
// (0 when absent), values PostgreSQL does not list here are dropped.
var pgActivityStates = []string{
	"active", "idle", "idle in transaction", "idle in transaction (aborted)",
	"fastpath function call", "disabled",
}

var pgLockModes = []string{
	"AccessShareLock", "RowShareLock", "RowExclusiveLock", "ShareUpdateExclusiveLock",
	"ShareLock", "ShareRowExclusiveLock", "ExclusiveLock", "AccessExclusiveLock",
}

type pgDatabaseStats struct {
	Name         string
	NumBackends  float64
	XactCommit   float64
	XactRollback float64
	BlksRead     float64
	BlksHit      float64
	TupFetched   float64
	TupInserted  float64
	TupUpdated   float64
	TupDeleted   float64
	Deadlocks    float64
	SizeBytes    float64
}

// pgStats is one immutable sampling result, swapped atomically like the
// health snapshot. The pg_stat_* families are served only while OK.
type pgStats struct {
	OK             bool
	Detail         string
	TakenAt        time.Time
	LastSuccess    time.Time
	Duration       time.Duration
	Databases      []pgDatabaseStats
	Activity       map[string]float64
	Locks          map[string]float64
	MaxConnections float64
}

const (
	pgDatabaseStatsSQL = `SELECT d.datname, s.numbackends, s.xact_commit, s.xact_rollback,
  s.blks_read, s.blks_hit, s.tup_fetched, s.tup_inserted, s.tup_updated, s.tup_deleted,
  s.deadlocks, pg_database_size(d.oid)
FROM pg_stat_database s JOIN pg_database d ON d.oid = s.datid
WHERE NOT d.datistemplate ORDER BY d.datname`
	pgActivitySQL       = `SELECT state, count(*) FROM pg_stat_activity WHERE state IS NOT NULL GROUP BY state`
	pgMaxConnectionsSQL = `SELECT current_setting('max_connections')`
	pgLocksSQL          = `SELECT mode, count(*) FROM pg_locks GROUP BY mode`
)

// samplePgStats authenticates on the handshake connection and reads the
// statistics; the deadline set at dial time bounds the whole exchange.
func samplePgStats(conn *pgConn, password string, previous pgStats) pgStats {
	started := time.Now()
	result := pgStats{TakenAt: started, LastSuccess: previous.LastSuccess}
	fail := func(stage string, err error) pgStats {
		result.OK = false
		result.Detail = stage + ": " + err.Error()
		result.Duration = time.Since(started)
		return result
	}
	if err := conn.authenticate(password); err != nil {
		conn.close()
		return fail("authentication", err)
	}
	defer conn.terminate()

	rows, err := conn.query(pgDatabaseStatsSQL)
	if err != nil {
		return fail("pg_stat_database", err)
	}
	for _, row := range rows {
		if len(row) != 12 {
			return fail("pg_stat_database", fmt.Errorf("unexpected column count %d", len(row)))
		}
		values := make([]float64, 11)
		for i := range values {
			if values[i], err = strconv.ParseFloat(row[i+1], 64); err != nil {
				return fail("pg_stat_database", fmt.Errorf("column %d: %w", i+1, err))
			}
		}
		result.Databases = append(result.Databases, pgDatabaseStats{
			Name: row[0], NumBackends: values[0], XactCommit: values[1], XactRollback: values[2],
			BlksRead: values[3], BlksHit: values[4], TupFetched: values[5], TupInserted: values[6],
			TupUpdated: values[7], TupDeleted: values[8], Deadlocks: values[9], SizeBytes: values[10],
		})
	}

	if result.Activity, err = countedRows(conn, pgActivitySQL); err != nil {
		return fail("pg_stat_activity", err)
	}
	if result.Locks, err = countedRows(conn, pgLocksSQL); err != nil {
		return fail("pg_locks", err)
	}
	rows, err = conn.query(pgMaxConnectionsSQL)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		if err == nil {
			err = fmt.Errorf("unexpected result shape")
		}
		return fail("max_connections", err)
	}
	if result.MaxConnections, err = strconv.ParseFloat(rows[0][0], 64); err != nil {
		return fail("max_connections", err)
	}

	result.OK = true
	result.Detail = "ok"
	result.Duration = time.Since(started)
	result.LastSuccess = time.Now()
	return result
}

// countedRows runs a "SELECT label, count(*) ... GROUP BY label" query.
func countedRows(conn *pgConn, sql string) (map[string]float64, error) {
	rows, err := conn.query(sql)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]float64, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("unexpected column count %d", len(row))
		}
		value, err := strconv.ParseFloat(row[1], 64)
		if err != nil {
			return nil, err
		}
		counts[row[0]] = value
	}
	return counts, nil
}

// ---------------------------------------------------------------------------

// healthCollector renders the baseline families of the image contract from
// the atomic health snapshot and the supervisor state; nothing is computed
// at scrape time beyond reading them.
type healthCollector struct {
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

func newHealthCollector(st *state) *healthCollector {
	return &healthCollector{
		st:       st,
		version:  envStr("APP_VERSION", "dev"),
		revision: envStr("APP_REVISION", "unknown"),
		buildInfo: prometheus.NewDesc("build_info",
			"The running artifact: always 1, labelled with its version and revision.", []string{"version", "revision"}, nil),
		checkUp: prometheus.NewDesc("health_check_up",
			"1 when the check passed in the last checker cycle.", []string{"check"}, nil),
		checkDuration: prometheus.NewDesc("health_check_duration_seconds",
			"Duration of the last run of the check.", []string{"check"}, nil),
		checkLastSuccess: prometheus.NewDesc("health_check_last_success_timestamp_seconds",
			"Unix time of the last successful run of the check; 0 until it passed once.", []string{"check"}, nil),
		snapshotAge: prometheus.NewDesc("health_snapshot_age_seconds",
			"Age of the cached health snapshot the probe endpoints serve.", nil, nil),
		cycles: prometheus.NewDesc("health_checker_cycles_total",
			"Completed checker cycles since the process started.", nil, nil),
		draining: prometheus.NewDesc("health_draining",
			"1 once a shutdown signal latched draining (readiness reports 503).", nil, nil),
		childUp: prometheus.NewDesc("supervisor_child_up",
			"1 while the supervised postmaster process is running.", nil, nil),
		childStart: prometheus.NewDesc("supervisor_child_start_time_seconds",
			"Unix time the supervised postmaster was started; 0 before its start.", nil, nil),
	}
}

func (h *healthCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{h.buildInfo, h.checkUp, h.checkDuration, h.checkLastSuccess,
		h.snapshotAge, h.cycles, h.draining, h.childUp, h.childStart} {
		ch <- desc
	}
}

func (h *healthCollector) Collect(ch chan<- prometheus.Metric) {
	snap := h.st.snap.Load().(snapshot)
	ch <- prometheus.MustNewConstMetric(h.buildInfo, prometheus.GaugeValue, 1, h.version, h.revision)
	for name, result := range snap.Results {
		ch <- prometheus.MustNewConstMetric(h.checkUp, prometheus.GaugeValue, boolValue(result.OK), name)
		ch <- prometheus.MustNewConstMetric(h.checkDuration, prometheus.GaugeValue, result.Duration.Seconds(), name)
		ch <- prometheus.MustNewConstMetric(h.checkLastSuccess, prometheus.GaugeValue, unixSeconds(result.LastSuccess), name)
	}
	age := 0.0
	if !snap.TakenAt.IsZero() {
		age = time.Since(snap.TakenAt).Seconds()
	}
	ch <- prometheus.MustNewConstMetric(h.snapshotAge, prometheus.GaugeValue, age)
	ch <- prometheus.MustNewConstMetric(h.cycles, prometheus.CounterValue, float64(h.st.cycles.Load()))
	ch <- prometheus.MustNewConstMetric(h.draining, prometheus.GaugeValue, boolValue(h.st.draining.Load()))
	ch <- prometheus.MustNewConstMetric(h.childUp, prometheus.GaugeValue, boolValue(h.st.childUp.Load()))
	ch <- prometheus.MustNewConstMetric(h.childStart, prometheus.GaugeValue, float64(h.st.childStart.Load()))
}

// pgCollector renders the database families from the last sampling result.
type pgCollector struct {
	st *state

	up             *prometheus.Desc
	statsUp        *prometheus.Desc
	statsLast      *prometheus.Desc
	statsDuration  *prometheus.Desc
	numBackends    *prometheus.Desc
	xactCommit     *prometheus.Desc
	xactRollback   *prometheus.Desc
	blksHit        *prometheus.Desc
	blksRead       *prometheus.Desc
	tupFetched     *prometheus.Desc
	tupInserted    *prometheus.Desc
	tupUpdated     *prometheus.Desc
	tupDeleted     *prometheus.Desc
	deadlocks      *prometheus.Desc
	databaseSize   *prometheus.Desc
	activity       *prometheus.Desc
	maxConnections *prometheus.Desc
	locks          *prometheus.Desc
}

func newPgCollector(st *state) *pgCollector {
	perDatabase := []string{"datname"}
	return &pgCollector{
		st: st,
		up: prometheus.NewDesc("pg_up",
			"1 when the postmaster accepted the health-check handshake in the last checker cycle.", nil, nil),
		statsUp: prometheus.NewDesc("pg_stats_up",
			"1 when the last statistics sampling succeeded (needs the database password at runtime).", nil, nil),
		statsLast: prometheus.NewDesc("pg_stats_last_success_timestamp_seconds",
			"Unix time of the last successful statistics sampling; 0 until it succeeded once.", nil, nil),
		statsDuration: prometheus.NewDesc("pg_stats_scrape_duration_seconds",
			"Duration of the last statistics sampling (authentication and queries).", nil, nil),
		numBackends: prometheus.NewDesc("pg_stat_database_numbackends",
			"Backends currently connected to the database.", perDatabase, nil),
		xactCommit: prometheus.NewDesc("pg_stat_database_xact_commit_total",
			"Transactions committed in the database.", perDatabase, nil),
		xactRollback: prometheus.NewDesc("pg_stat_database_xact_rollback_total",
			"Transactions rolled back in the database.", perDatabase, nil),
		blksHit: prometheus.NewDesc("pg_stat_database_blks_hit_total",
			"Disk blocks found in the buffer cache.", perDatabase, nil),
		blksRead: prometheus.NewDesc("pg_stat_database_blks_read_total",
			"Disk blocks read from disk.", perDatabase, nil),
		tupFetched: prometheus.NewDesc("pg_stat_database_tup_fetched_total",
			"Live rows fetched by queries.", perDatabase, nil),
		tupInserted: prometheus.NewDesc("pg_stat_database_tup_inserted_total",
			"Rows inserted.", perDatabase, nil),
		tupUpdated: prometheus.NewDesc("pg_stat_database_tup_updated_total",
			"Rows updated.", perDatabase, nil),
		tupDeleted: prometheus.NewDesc("pg_stat_database_tup_deleted_total",
			"Rows deleted.", perDatabase, nil),
		deadlocks: prometheus.NewDesc("pg_stat_database_deadlocks_total",
			"Deadlocks detected in the database.", perDatabase, nil),
		databaseSize: prometheus.NewDesc("pg_database_size_bytes",
			"Disk space used by the database.", perDatabase, nil),
		activity: prometheus.NewDesc("pg_stat_activity_backends",
			"Server processes by state (pg_stat_activity, the sampler's own session included).", []string{"state"}, nil),
		maxConnections: prometheus.NewDesc("pg_settings_max_connections",
			"The max_connections setting.", nil, nil),
		locks: prometheus.NewDesc("pg_locks",
			"Locks held or awaited by mode (pg_locks).", []string{"mode"}, nil),
	}
}

func (p *pgCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{p.up, p.statsUp, p.statsLast, p.statsDuration, p.numBackends,
		p.xactCommit, p.xactRollback, p.blksHit, p.blksRead, p.tupFetched, p.tupInserted, p.tupUpdated,
		p.tupDeleted, p.deadlocks, p.databaseSize, p.activity, p.maxConnections, p.locks} {
		ch <- desc
	}
}

func (p *pgCollector) Collect(ch chan<- prometheus.Metric) {
	snap := p.st.snap.Load().(snapshot)
	stats := p.st.stats.Load().(pgStats)
	ch <- prometheus.MustNewConstMetric(p.up, prometheus.GaugeValue, boolValue(snap.Results["postgresql"].OK))
	ch <- prometheus.MustNewConstMetric(p.statsUp, prometheus.GaugeValue, boolValue(stats.OK))
	ch <- prometheus.MustNewConstMetric(p.statsLast, prometheus.GaugeValue, unixSeconds(stats.LastSuccess))
	ch <- prometheus.MustNewConstMetric(p.statsDuration, prometheus.GaugeValue, stats.Duration.Seconds())
	if !stats.OK {
		return
	}
	for _, db := range stats.Databases {
		ch <- prometheus.MustNewConstMetric(p.numBackends, prometheus.GaugeValue, db.NumBackends, db.Name)
		ch <- prometheus.MustNewConstMetric(p.xactCommit, prometheus.CounterValue, db.XactCommit, db.Name)
		ch <- prometheus.MustNewConstMetric(p.xactRollback, prometheus.CounterValue, db.XactRollback, db.Name)
		ch <- prometheus.MustNewConstMetric(p.blksHit, prometheus.CounterValue, db.BlksHit, db.Name)
		ch <- prometheus.MustNewConstMetric(p.blksRead, prometheus.CounterValue, db.BlksRead, db.Name)
		ch <- prometheus.MustNewConstMetric(p.tupFetched, prometheus.CounterValue, db.TupFetched, db.Name)
		ch <- prometheus.MustNewConstMetric(p.tupInserted, prometheus.CounterValue, db.TupInserted, db.Name)
		ch <- prometheus.MustNewConstMetric(p.tupUpdated, prometheus.CounterValue, db.TupUpdated, db.Name)
		ch <- prometheus.MustNewConstMetric(p.tupDeleted, prometheus.CounterValue, db.TupDeleted, db.Name)
		ch <- prometheus.MustNewConstMetric(p.deadlocks, prometheus.CounterValue, db.Deadlocks, db.Name)
		ch <- prometheus.MustNewConstMetric(p.databaseSize, prometheus.GaugeValue, db.SizeBytes, db.Name)
	}
	for _, state := range pgActivityStates {
		ch <- prometheus.MustNewConstMetric(p.activity, prometheus.GaugeValue, stats.Activity[state], state)
	}
	ch <- prometheus.MustNewConstMetric(p.maxConnections, prometheus.GaugeValue, stats.MaxConnections)
	for _, mode := range pgLockModes {
		ch <- prometheus.MustNewConstMetric(p.locks, prometheus.GaugeValue, stats.Locks[mode], mode)
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// ---------------------------------------------------------------------------

func newRegistry(st *state) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newHealthCollector(st),
		newPgCollector(st),
	)
	return reg
}

// metricsHandler always serves OpenMetrics text with the `# EOF` terminator,
// no content negotiation: the standard fixes the format, not the scraper.
func metricsHandler(reg *prometheus.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		families, err := reg.Gather()
		if err != nil {
			http.Error(w, "metrics collection failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var body bytes.Buffer
		encoder := expfmt.NewEncoder(&body, expfmt.NewFormat(expfmt.TypeOpenMetrics))
		for _, family := range families {
			if err := encoder.Encode(family); err != nil {
				http.Error(w, "metrics encoding failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if closer, ok := encoder.(expfmt.Closer); ok {
			if err := closer.Close(); err != nil {
				http.Error(w, "metrics encoding failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", openMetricsContentType)
		w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
		_, _ = w.Write(body.Bytes())
	}
}
