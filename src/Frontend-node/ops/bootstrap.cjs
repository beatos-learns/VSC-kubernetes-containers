'use strict'

// PID 1 bootstrap for the auth-portal image (Node.js variant), in-process
// checker topology: it loads the Next.js standalone server into this same
// Node process, serves the probe endpoints and OpenMetrics on ADMIN_PORT,
// runs the cached health checker, instruments the main listener (routes,
// bytes, in-flight requests, connections) and the proxied backend calls,
// correlates requests through X-Request-Id, writes ECS JSON logs, and drives
// the SIGTERM/SIGINT drain sequence with real connection draining.

const http = require('node:http')
const fs = require('node:fs')
const path = require('node:path')
const util = require('node:util')
const crypto = require('node:crypto')
const { AsyncLocalStorage } = require('node:async_hooks')
const { monitorEventLoopDelay } = require('node:perf_hooks')

const SERVICE_NAME = 'auth-portal'
const ECS_VERSION = '8.11'
const APP_DIR = path.join(__dirname, '..')
const LOG_LEVELS = { trace: 0, debug: 1, info: 2, warn: 3, error: 4 }

// ---------------------------------------------------------------------------
// configuration (validated once; any violation aborts startup)
// ---------------------------------------------------------------------------

function fatal(message) {
  console.error('fatal configuration error: ' + message)
  process.exit(1)
}

function envStr(name, fallback) {
  const value = process.env[name]
  return value === undefined || value.trim() === '' ? fallback : value.trim()
}

function envInt(name, fallback, min, max) {
  const raw = envStr(name, String(fallback))
  const value = Number.parseInt(raw, 10)
  if (Number.isNaN(value)) fatal(`${name}=${raw} is not an integer`)
  if (value < min || value > max) fatal(`${name}=${value} is outside [${min}, ${max}]`)
  return value
}

function envFloat(name, fallback, min, max) {
  const raw = envStr(name, String(fallback))
  const value = Number.parseFloat(raw)
  if (Number.isNaN(value)) fatal(`${name}=${raw} is not a number`)
  if (value < min || value > max) fatal(`${name}=${value} is outside [${min}, ${max}]`)
  return value
}

function envEnum(name, fallback, allowed) {
  const value = envStr(name, fallback).toLowerCase()
  if (!allowed.includes(value)) fatal(`${name}=${value} is invalid; allowed: ${allowed.join(', ')}`)
  return value
}

function envBool(name, fallback) {
  const raw = envStr(name, String(fallback)).toLowerCase()
  if (['true', '1', 'yes'].includes(raw)) return true
  if (['false', '0', 'no'].includes(raw)) return false
  fatal(`${name}=${raw} is not a boolean`)
  return fallback
}

const APP_VERSION = envStr('APP_VERSION', 'dev')
const APP_REVISION = envStr('APP_REVISION', 'unknown')

const cfg = {
  port: envInt('PORT', 3000, 1, 65535),
  adminPort: envInt('ADMIN_PORT', 9090, 1, 65535),
  bindAddr: envStr('BIND_ADDR', '0.0.0.0'),
  logLevel: envEnum('LOG_LEVEL', 'info', ['trace', 'debug', 'info', 'warn', 'error']),
  logFormat: envEnum('LOG_FORMAT', 'json', ['json', 'text']),
  accessLog: envBool('ACCESS_LOG', true),
  checkIntervalSec: envInt('HEALTH_CHECK_INTERVAL', 5, 1, 3600),
  checkTimeoutSec: envInt('HEALTH_CHECK_TIMEOUT', 2, 1, 3600),
  staleFactor: envFloat('HEALTH_STALE_FACTOR', 3, 1, 100),
  drainDelaySec: envInt('SHUTDOWN_DRAIN_DELAY', 3, 0, 600),
  drainBudgetSec: envInt('SHUTDOWN_TIMEOUT', 10, 1, 3600),
  apiUrl: envStr('API_URL', ''),
}
if (cfg.checkTimeoutSec >= cfg.checkIntervalSec) {
  fatal(`HEALTH_CHECK_TIMEOUT (${cfg.checkTimeoutSec}s) must be smaller than HEALTH_CHECK_INTERVAL (${cfg.checkIntervalSec}s)`)
}
if (cfg.port === cfg.adminPort) fatal('PORT and ADMIN_PORT must differ')
if (cfg.apiUrl === '') fatal('API_URL is required (base URL of the user-mgmt-service backend)')
let apiUrlParsed
try {
  apiUrlParsed = new URL(cfg.apiUrl)
} catch {
  fatal(`API_URL=${cfg.apiUrl} is not a valid URL`)
}
if (!['http:', 'https:'].includes(apiUrlParsed.protocol)) fatal(`API_URL=${cfg.apiUrl} must be an http(s) URL`)
cfg.apiUrl = cfg.apiUrl.replace(/\/+$/, '')
const apiBasePath = new URL(cfg.apiUrl).pathname.replace(/\/+$/, '')
process.env.API_URL = cfg.apiUrl

function configSummary() {
  return `PORT=${cfg.port} ADMIN_PORT=${cfg.adminPort} BIND_ADDR=${cfg.bindAddr} LOG_LEVEL=${cfg.logLevel} ` +
    `LOG_FORMAT=${cfg.logFormat} ACCESS_LOG=${cfg.accessLog} HEALTH_CHECK_INTERVAL=${cfg.checkIntervalSec} ` +
    `HEALTH_CHECK_TIMEOUT=${cfg.checkTimeoutSec} HEALTH_STALE_FACTOR=${cfg.staleFactor} ` +
    `SHUTDOWN_DRAIN_DELAY=${cfg.drainDelaySec} SHUTDOWN_TIMEOUT=${cfg.drainBudgetSec} API_URL=${cfg.apiUrl}`
}

// ---------------------------------------------------------------------------
// logging: ECS JSON on stdout (LOG_FORMAT=text keeps the one-line human form)
// ---------------------------------------------------------------------------

function emitLog(logger, level, message, fields) {
  if (LOG_LEVELS[level] < LOG_LEVELS[cfg.logLevel]) return
  if (cfg.logFormat === 'json') {
    const entry = { '@timestamp': new Date().toISOString(), 'log.level': level, 'log.logger': logger, message }
    if (fields) Object.assign(entry, fields)
    entry['ecs.version'] = ECS_VERSION
    entry['service.name'] = SERVICE_NAME
    entry['service.version'] = APP_VERSION
    process.stdout.write(JSON.stringify(entry) + '\n')
  } else {
    let suffix = ''
    if (fields) {
      suffix = ' ' + Object.entries(fields).map(([key, value]) => `${key}=${JSON.stringify(value)}`).join(' ')
    }
    process.stdout.write(`${new Date().toISOString()} ${level.toUpperCase().padEnd(5)} ${logger}: ${message}${suffix}\n`)
  }
}

function log(level, message, fields) {
  emitLog('bootstrap', level, message, fields)
}

// Next.js and the application code log through console.*; re-emitting those
// lines through the same logger keeps stdout one JSON shape (logger "next").
function captureConsole() {
  const levels = { log: 'info', info: 'info', debug: 'debug', warn: 'warn', error: 'error', trace: 'trace' }
  for (const [method, level] of Object.entries(levels)) {
    console[method] = (...args) => {
      const message = util.format(...args).replace(/\x1b\[[0-9;]*m/g, '').replace(/\s+$/, '')
      if (message !== '') emitLog('next', level, message)
    }
  }
}

// ---------------------------------------------------------------------------
// metrics: hand-written OpenMetrics exposition (no dependency)
// ---------------------------------------------------------------------------

const BUCKETS = [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]
const BUCKET_LE = BUCKETS.map((bound) => (Number.isInteger(bound) ? bound.toFixed(1) : String(bound)))

function fmtNumber(value) {
  if (Number.isNaN(value)) return 'NaN'
  if (value === Infinity) return '+Inf'
  if (value === -Infinity) return '-Inf'
  return String(value)
}

function escapeLabelValue(value) {
  return String(value).replace(/\\/g, '\\\\').replace(/\n/g, '\\n').replace(/"/g, '\\"')
}

function labelSet(names, values) {
  return names.map((name, index) => `${name}="${escapeLabelValue(values[index])}"`).join(',')
}

function sortedEntries(map) {
  return [...map.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0))
}

function header(name, type, help) {
  return `# HELP ${name} ${help}\n# TYPE ${name} ${type}\n`
}

function sample(name, labels, value) {
  return `${name}${labels ? `{${labels}}` : ''} ${fmtNumber(value)}\n`
}

class Counter {
  constructor(name, help, labelNames) {
    this.name = name
    this.help = help
    this.labelNames = labelNames
    this.series = new Map()
  }

  inc(values, by = 1) {
    const key = labelSet(this.labelNames, values)
    this.series.set(key, (this.series.get(key) || 0) + by)
  }

  render() {
    if (this.series.size === 0) return ''
    let out = header(this.name, 'counter', this.help)
    for (const [labels, value] of sortedEntries(this.series)) out += sample(this.name + '_total', labels, value)
    return out
  }
}

class Gauge {
  constructor(name, help, labelNames) {
    this.name = name
    this.help = help
    this.labelNames = labelNames
    this.series = new Map()
  }

  set(values, value) {
    this.series.set(labelSet(this.labelNames, values), value)
  }

  add(values, by) {
    const key = labelSet(this.labelNames, values)
    this.series.set(key, (this.series.get(key) || 0) + by)
  }

  inc(values) {
    this.add(values, 1)
  }

  dec(values) {
    this.add(values, -1)
  }

  render() {
    if (this.series.size === 0) return ''
    let out = header(this.name, 'gauge', this.help)
    for (const [labels, value] of sortedEntries(this.series)) out += sample(this.name, labels, value)
    return out
  }
}

class Histogram {
  constructor(name, help, labelNames) {
    this.name = name
    this.help = help
    this.labelNames = labelNames
    this.series = new Map()
  }

  observe(values, value) {
    const key = labelSet(this.labelNames, values)
    let series = this.series.get(key)
    if (!series) {
      series = { counts: new Array(BUCKETS.length).fill(0), sum: 0, count: 0 }
      this.series.set(key, series)
    }
    for (let index = 0; index < BUCKETS.length; index++) {
      if (value <= BUCKETS[index]) series.counts[index]++
    }
    series.sum += value
    series.count++
  }

  render() {
    if (this.series.size === 0) return ''
    let out = header(this.name, 'histogram', this.help)
    for (const [labels, series] of sortedEntries(this.series)) {
      const prefix = labels ? labels + ',' : ''
      let cumulative = 0
      for (let index = 0; index < BUCKETS.length; index++) {
        cumulative += series.counts[index]
        out += sample(this.name + '_bucket', `${prefix}le="${BUCKET_LE[index]}"`, cumulative)
      }
      out += sample(this.name + '_bucket', `${prefix}le="+Inf"`, series.count)
      out += sample(this.name + '_sum', labels, series.sum)
      out += sample(this.name + '_count', labels, series.count)
    }
    return out
  }
}

const metrics = {
  serverRequests: new Histogram('http_server_requests_seconds',
    'Duration of requests served on the main listener, by route template.',
    ['method', 'uri', 'status', 'outcome', 'exception']),
  serverActive: new Gauge('http_server_requests_active',
    'Requests currently in flight on the main listener.', ['method', 'uri']),
  serverRequestBytes: new Counter('http_server_request_bytes',
    'Request body bytes received on the main listener (Content-Length).', ['uri']),
  serverResponseBytes: new Counter('http_server_response_bytes',
    'Response body bytes written on the main listener.', ['uri']),
  clientRequests: new Histogram('http_client_requests_seconds',
    'Duration of calls to another component, until the response headers arrive.',
    ['peer', 'method', 'uri', 'status', 'outcome']),
  clientActive: new Gauge('http_client_requests_active',
    'Calls to another component currently in flight.', ['peer', 'method', 'uri']),
  clientRequestBytes: new Counter('http_client_request_bytes',
    'Request body bytes sent to another component.', ['peer', 'uri']),
  clientResponseBytes: new Counter('http_client_response_bytes',
    'Response body bytes announced by another component (Content-Length).', ['peer', 'uri']),
  connections: new Gauge('http_server_connections_active',
    'Open TCP connections per listener.', ['listener']),
}
metrics.connections.set(['main'], 0)
metrics.connections.set(['admin'], 0)

const eventLoopDelay = monitorEventLoopDelay({ resolution: 20 })
eventLoopDelay.enable()

function measureEventLoopLag() {
  return new Promise((resolve) => {
    const started = process.hrtime.bigint()
    setImmediate(() => resolve(Number(process.hrtime.bigint() - started) / 1e9))
  })
}

function readProcStatus() {
  try {
    const status = fs.readFileSync('/proc/self/status', 'utf8')
    const values = {}
    for (const key of ['VmRSS', 'VmSize', 'VmData']) {
      const match = new RegExp(`^${key}:\\s+(\\d+) kB`, 'm').exec(status)
      if (match) values[key] = Number(match[1]) * 1024
    }
    return values
  } catch {
    return null
  }
}

function readOpenFds() {
  try {
    return fs.readdirSync('/proc/self/fd').length
  } catch {
    return null
  }
}

function readMaxFds() {
  try {
    const match = /^Max open files\s+(\d+|unlimited)/m.exec(fs.readFileSync('/proc/self/limits', 'utf8'))
    return match && match[1] !== 'unlimited' ? Number(match[1]) : null
  } catch {
    return null
  }
}

function gaugeText(name, help, samples) {
  let out = header(name, 'gauge', help)
  for (const [labels, value] of samples) out += sample(name, labels, value)
  return out
}

function counterText(name, help, value) {
  return header(name, 'counter', help) + sample(name + '_total', '', value)
}

function seconds(nanoseconds) {
  return Number.isFinite(nanoseconds) ? nanoseconds / 1e9 : 0
}

async function renderMetrics() {
  const lag = await measureEventLoopLag()
  const families = []
  const add = (text) => { if (text !== '') families.push(text) }

  add(gaugeText('build_info', 'The running artifact (always 1).',
    [[labelSet(['version', 'revision'], [APP_VERSION, APP_REVISION]), 1]]))

  const snapshot = state.snapshot
  const checks = Object.keys(snapshot.results).sort()
  const up = []
  const duration = []
  const lastSuccess = []
  for (const name of checks) {
    const result = snapshot.results[name]
    const labels = labelSet(['check'], [name])
    up.push([labels, result.ok ? 1 : 0])
    duration.push([labels, result.durationSeconds])
    lastSuccess.push([labels, result.lastSuccessSeconds])
  }
  if (checks.length > 0) {
    add(gaugeText('health_check_up', '1 when the check passed in the last checker cycle.', up))
    add(gaugeText('health_check_duration_seconds', 'Duration of the last run of the check.', duration))
    add(gaugeText('health_check_last_success_timestamp_seconds',
      'Unix time of the last successful run of the check (0 until it first passed).', lastSuccess))
  }
  add(gaugeText('health_snapshot_age_seconds', 'Age of the cached health snapshot.',
    [['', snapshot.takenAt === 0 ? 0 : (Date.now() - snapshot.takenAt) / 1000]]))
  add(counterText('health_checker_cycles', 'Completed health checker cycles.', state.cycles))
  add(gaugeText('health_draining', '1 once the drain latch is set (readiness reports 503).',
    [['', state.draining ? 1 : 0]]))

  for (const family of Object.values(metrics)) add(family.render())

  const cpu = process.cpuUsage()
  add(counterText('process_cpu_user_seconds', 'Total user CPU time spent in seconds.', cpu.user / 1e6))
  add(counterText('process_cpu_system_seconds', 'Total system CPU time spent in seconds.', cpu.system / 1e6))
  add(counterText('process_cpu_seconds', 'Total user and system CPU time spent in seconds.', (cpu.user + cpu.system) / 1e6))
  add(gaugeText('process_start_time_seconds', 'Start time of the process since unix epoch in seconds.',
    [['', Math.round(Date.now() / 1000 - process.uptime())]]))
  const memory = process.memoryUsage()
  const procStatus = readProcStatus()
  add(gaugeText('process_resident_memory_bytes', 'Resident memory size in bytes.',
    [['', procStatus && procStatus.VmRSS !== undefined ? procStatus.VmRSS : memory.rss]]))
  if (procStatus && procStatus.VmSize !== undefined) {
    add(gaugeText('process_virtual_memory_bytes', 'Virtual memory size in bytes.', [['', procStatus.VmSize]]))
  }
  if (procStatus && procStatus.VmData !== undefined) {
    add(gaugeText('process_heap_bytes', 'Process heap size in bytes.', [['', procStatus.VmData]]))
  }
  const openFds = readOpenFds()
  if (openFds !== null) add(gaugeText('process_open_fds', 'Number of open file descriptors.', [['', openFds]]))
  const maxFds = readMaxFds()
  if (maxFds !== null) add(gaugeText('process_max_fds', 'Maximum number of open file descriptors.', [['', maxFds]]))

  add(gaugeText('nodejs_eventloop_lag_seconds', 'Lag of the event loop in seconds, measured at scrape time.', [['', lag]]))
  add(gaugeText('nodejs_eventloop_lag_min_seconds', 'Minimum event loop delay since the last scrape.', [['', seconds(eventLoopDelay.min)]]))
  add(gaugeText('nodejs_eventloop_lag_max_seconds', 'Maximum event loop delay since the last scrape.', [['', seconds(eventLoopDelay.max)]]))
  add(gaugeText('nodejs_eventloop_lag_mean_seconds', 'Mean event loop delay since the last scrape.', [['', seconds(eventLoopDelay.mean)]]))
  add(gaugeText('nodejs_eventloop_lag_stddev_seconds', 'Standard deviation of the event loop delay since the last scrape.', [['', seconds(eventLoopDelay.stddev)]]))
  add(gaugeText('nodejs_eventloop_lag_p50_seconds', 'Median event loop delay since the last scrape.', [['', seconds(eventLoopDelay.percentile(50))]]))
  add(gaugeText('nodejs_eventloop_lag_p90_seconds', '90th percentile of the event loop delay since the last scrape.', [['', seconds(eventLoopDelay.percentile(90))]]))
  add(gaugeText('nodejs_eventloop_lag_p99_seconds', '99th percentile of the event loop delay since the last scrape.', [['', seconds(eventLoopDelay.percentile(99))]]))
  eventLoopDelay.reset()
  if (typeof process._getActiveHandles === 'function') {
    add(gaugeText('nodejs_active_handles', 'Number of active libuv handles.', [['', process._getActiveHandles().length]]))
  }
  if (typeof process._getActiveRequests === 'function') {
    add(gaugeText('nodejs_active_requests', 'Number of active libuv requests.', [['', process._getActiveRequests().length]]))
  }
  if (typeof process.getActiveResourcesInfo === 'function') {
    const byType = new Map()
    for (const type of process.getActiveResourcesInfo()) byType.set(type, (byType.get(type) || 0) + 1)
    add(gaugeText('nodejs_active_resources', 'Number of active resources keeping the event loop alive, by type.',
      sortedEntries(byType).map(([type, count]) => [labelSet(['type'], [type]), count])))
  }
  add(gaugeText('nodejs_heap_size_total_bytes', 'Process heap size from Node.js in bytes.', [['', memory.heapTotal]]))
  add(gaugeText('nodejs_heap_size_used_bytes', 'Process heap size used from Node.js in bytes.', [['', memory.heapUsed]]))
  add(gaugeText('nodejs_external_memory_bytes', 'Node.js external memory size in bytes.', [['', memory.external]]))
  const [major, minor, patch] = process.versions.node.split('.')
  add(gaugeText('nodejs_version_info', 'Node.js version info.',
    [[labelSet(['version', 'major', 'minor', 'patch'], [process.version, major, minor, patch]), 1]]))

  families.sort((a, b) => {
    const nameA = a.slice(7, a.indexOf(' ', 7))
    const nameB = b.slice(7, b.indexOf(' ', 7))
    return nameA < nameB ? -1 : nameA > nameB ? 1 : 0
  })
  return families.join('') + '# EOF\n'
}

// ---------------------------------------------------------------------------
// request instrumentation of the main listener (route classes, bytes, in-flight,
// request id, access log) and of the proxied backend calls (fetch)
// ---------------------------------------------------------------------------

const REQUEST_ID_RE = /^[A-Za-z0-9._-]{1,128}$/
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const requestContext = new AsyncLocalStorage()

const publicFiles = new Set()
function indexPublicFiles(dir, prefix) {
  let entries
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true })
  } catch {
    return
  }
  for (const entry of entries) {
    const rel = prefix + '/' + entry.name
    if (entry.isDirectory()) indexPublicFiles(path.join(dir, entry.name), rel)
    else publicFiles.add(rel)
  }
}
indexPublicFiles(path.join(APP_DIR, 'public'), '')

// The route classes are a fixed enumeration shared with the Go variant, so a
// scan cannot create series and dashboards need no variant-specific query.
function classifyRoute(rawUrl) {
  if (typeof rawUrl !== 'string' || rawUrl.startsWith('//')) return 'UNKNOWN'
  let pathname
  try {
    pathname = new URL(rawUrl, 'http://localhost').pathname
  } catch {
    return 'UNKNOWN'
  }
  pathname = path.posix.normalize(pathname)
  if (pathname.length > 1 && pathname.endsWith('/')) pathname = pathname.slice(0, -1)
  switch (pathname) {
    case '/':
    case '/login':
    case '/signup':
    case '/api/login':
    case '/api/logout':
    case '/api/me':
    case '/api/signup':
    case '/api/modules':
      return pathname
    default:
  }
  const subscription = /^\/api\/users\/([^/]+)\/modules\/([^/]+)$/.exec(pathname)
  if (subscription && UUID_RE.test(subscription[1]) && UUID_RE.test(subscription[2])) {
    return '/api/users/{id}/modules/{moduleId}'
  }
  if (pathname === '/dashboard' || pathname.startsWith('/dashboard/')) return '/dashboard'
  if (pathname.startsWith('/_next/') || pathname === '/favicon.ico' || publicFiles.has(pathname)) return '/static/**'
  return 'UNKNOWN'
}

function backendRoute(pathname) {
  let rel = pathname
  if (apiBasePath !== '' && rel.startsWith(apiBasePath)) rel = rel.slice(apiBasePath.length)
  if (rel.length > 1 && rel.endsWith('/')) rel = rel.slice(0, -1)
  switch (rel) {
    case '/users/register':
    case '/users/login':
    case '/users/me':
    case '/users':
    case '/modules':
      return rel
    default:
  }
  const match = /^\/users\/([^/]+)$/.exec(rel)
  if (match && UUID_RE.test(match[1])) return '/users/{id}'
  const subscription = /^\/users\/([^/]+)\/modules\/([^/]+)$/.exec(rel)
  if (subscription && UUID_RE.test(subscription[1]) && UUID_RE.test(subscription[2])) {
    return '/users/{id}/modules/{moduleId}'
  }
  return 'UNKNOWN'
}

function outcomeOf(status) {
  if (status >= 100 && status < 200) return 'INFORMATIONAL'
  if (status >= 200 && status < 300) return 'SUCCESS'
  if (status >= 300 && status < 400) return 'REDIRECTION'
  if (status >= 400 && status < 500) return 'CLIENT_ERROR'
  if (status >= 500 && status < 600) return 'SERVER_ERROR'
  return 'UNKNOWN'
}

function chunkLength(chunk, encoding) {
  if (chunk == null || typeof chunk === 'function') return 0
  if (typeof chunk === 'string') return Buffer.byteLength(chunk, typeof encoding === 'string' ? encoding : 'utf8')
  if (Buffer.isBuffer(chunk) || ArrayBuffer.isView(chunk)) return chunk.byteLength
  return 0
}

function bodyLength(body) {
  if (body == null) return 0
  if (typeof body === 'string') return Buffer.byteLength(body)
  if (Buffer.isBuffer(body) || ArrayBuffer.isView(body)) return body.byteLength
  if (body instanceof ArrayBuffer) return body.byteLength
  if (body instanceof URLSearchParams) return Buffer.byteLength(body.toString())
  return 0
}

function errorName(error) {
  return (error && error.constructor && error.constructor.name) || 'Error'
}

// beginRequest runs before any listener of the 'request' event: it fixes the
// request id, classifies the route, wraps the response to count bytes, and
// finalizes the series plus the access line when the response is done.
function beginRequest(req, res) {
  const startedAt = process.hrtime.bigint()
  const incoming = req.headers['x-request-id']
  const requestId = typeof incoming === 'string' && REQUEST_ID_RE.test(incoming) ? incoming : crypto.randomUUID()
  const context = { requestId, upstreamNanos: 0, upstreamCalls: 0, exception: 'none' }
  const method = String(req.method || 'GET').toUpperCase()
  const uri = classifyRoute(req.url)
  const requestBytes = Number.parseInt(req.headers['content-length'], 10) || 0
  try {
    res.setHeader('X-Request-Id', requestId)
  } catch {
    // headers already sent: nothing to echo
  }
  metrics.serverActive.inc([method, uri])
  metrics.serverRequestBytes.inc([uri], requestBytes)

  let responseBytes = 0
  const writeOriginal = res.write
  const endOriginal = res.end
  res.write = function write(chunk, encoding, ...rest) {
    responseBytes += chunkLength(chunk, encoding)
    return writeOriginal.call(this, chunk, encoding, ...rest)
  }
  res.end = function end(chunk, encoding, ...rest) {
    responseBytes += chunkLength(chunk, encoding)
    return endOriginal.call(this, chunk, encoding, ...rest)
  }

  let finished = false
  const finish = () => {
    if (finished) return
    finished = true
    const durationNanos = Number(process.hrtime.bigint() - startedAt)
    const status = res.headersSent ? String(res.statusCode) : '0'
    const outcome = res.headersSent ? outcomeOf(res.statusCode) : 'UNKNOWN'
    metrics.serverActive.dec([method, uri])
    metrics.serverRequests.observe([method, uri, status, outcome, context.exception], durationNanos / 1e9)
    metrics.serverResponseBytes.inc([uri], responseBytes)
    if (cfg.accessLog) {
      const fields = {
        'http.request.id': requestId,
        'http.request.method': method,
        'url.path': uri,
        'http.response.status_code': Number(status),
        'event.duration': durationNanos,
        'http.request.bytes': requestBytes,
        'http.response.bytes': responseBytes,
      }
      if (context.upstreamCalls > 0) fields['upstream.duration'] = context.upstreamNanos
      emitLog('access', 'info', `${method} ${uri} ${status}`, fields)
    }
  }
  res.once('finish', finish)
  res.once('close', finish)
  return context
}

// Every listener of the captured server's 'request' event (Next.js attaches
// its own) runs inside the request context, so the backend calls it makes can
// carry the request id and account their duration to this request.
function instrumentMainServer(server) {
  const emitOriginal = server.emit
  server.emit = function emit(event, ...args) {
    if (event !== 'request') return emitOriginal.call(this, event, ...args)
    const [req, res] = args
    const context = beginRequest(req, res)
    return requestContext.run(context, () => {
      try {
        return emitOriginal.call(this, event, ...args)
      } catch (error) {
        context.exception = errorName(error)
        throw error
      }
    })
  }
  server.on('connection', (socket) => {
    metrics.connections.inc(['main'])
    socket.once('close', () => metrics.connections.dec(['main']))
  })
}

// The health checker keeps the un-instrumented fetch: its probe of the
// backend is not a proxied user request and must not appear as one.
const fetchOriginal = globalThis.fetch

function instrumentedFetch(input, init) {
  let url
  if (typeof input === 'string') url = input
  else if (input instanceof URL) url = input.href
  else if (input && typeof input.url === 'string') url = input.url
  else url = String(input)
  if (!(url === cfg.apiUrl || url.startsWith(cfg.apiUrl + '/') || url.startsWith(cfg.apiUrl + '?'))) {
    return fetchOriginal(input, init)
  }
  const method = String((init && init.method) || (input && input.method) || 'GET').toUpperCase()
  let uri = 'UNKNOWN'
  try {
    uri = backendRoute(new URL(url).pathname)
  } catch {
    // unparsable: stays UNKNOWN
  }
  const context = requestContext.getStore()
  const headers = new Headers((init && init.headers) || (input instanceof Request ? input.headers : undefined))
  if (context && !headers.has('x-request-id')) headers.set('x-request-id', context.requestId)
  const nextInit = Object.assign({}, init, { headers })
  const requestBytes = bodyLength(init ? init.body : undefined)
  const labels = ['backend', method, uri]
  metrics.clientActive.inc(labels)
  metrics.clientRequestBytes.inc(['backend', uri], requestBytes)
  const startedAt = process.hrtime.bigint()
  const finish = (status, outcome, responseBytes) => {
    const durationNanos = Number(process.hrtime.bigint() - startedAt)
    metrics.clientActive.dec(labels)
    metrics.clientRequests.observe(['backend', method, uri, status, outcome], durationNanos / 1e9)
    metrics.clientResponseBytes.inc(['backend', uri], responseBytes)
    if (context) {
      context.upstreamNanos += durationNanos
      context.upstreamCalls++
    }
  }
  return fetchOriginal(input, nextInit).then((response) => {
    finish(String(response.status), outcomeOf(response.status), Number(response.headers.get('content-length')) || 0)
    return response
  }, (error) => {
    finish('IO_ERROR', 'UNKNOWN', 0)
    throw error
  })
}
globalThis.fetch = instrumentedFetch

// ---------------------------------------------------------------------------
// health state and checker
// ---------------------------------------------------------------------------

const state = {
  snapshot: { results: {}, takenAt: 0 },
  cycles: 0,
  started: false,
  draining: false,
}

function allOk(results) {
  const names = Object.keys(results)
  return names.length > 0 && names.every((name) => results[name].ok)
}

function isFresh() {
  return state.snapshot.takenAt !== 0 &&
    Date.now() - state.snapshot.takenAt <= cfg.staleFactor * cfg.checkIntervalSec * 1000
}

function isReady() {
  return isFresh() && allOk(state.snapshot.results) && !state.draining
}

// Capture the listeners the Next standalone server creates so that the drain
// sequence can stop intake and close real connections, and instrument them.
const capturedServers = []
const createServerOriginal = http.createServer
http.createServer = function patchedCreateServer(...args) {
  const server = createServerOriginal.apply(http, args)
  capturedServers.push(server)
  instrumentMainServer(server)
  server.once('listening', () => {
    log('info', `auth-portal listening: version=${APP_VERSION} revision=${APP_REVISION} config[${configSummary()}]`)
  })
  return server
}

const checks = {
  'next-server': async () => {
    if (!capturedServers.some((server) => server.listening)) {
      throw new Error('no listening Next.js server')
    }
  },
  'backend-api': async () => {
    const target = cfg.apiUrl + '/users'
    await fetchOriginal(target, {
      method: 'GET',
      signal: AbortSignal.timeout(cfg.checkTimeoutSec * 1000),
    })
  },
}
// Dependency checks gate /readyz only; the self checks gate /startupz
// as well, so an unreachable backend never holds startup back.
const dependencyChecks = new Set(['backend-api'])

function selfChecksOk(results) {
  return Object.keys(checks).every((name) => dependencyChecks.has(name) || results[name].ok)
}

let previousResults = {}
async function runCycle() {
  const results = {}
  await Promise.all(Object.entries(checks).map(async ([name, run]) => {
    const startedAt = process.hrtime.bigint()
    const before = previousResults[name]
    try {
      await run()
      results[name] = {
        ok: true,
        detail: 'ok',
        durationSeconds: Number(process.hrtime.bigint() - startedAt) / 1e9,
        lastSuccessSeconds: Date.now() / 1000,
      }
    } catch (error) {
      results[name] = {
        ok: false,
        detail: String(error && error.message ? error.message : error),
        durationSeconds: Number(process.hrtime.bigint() - startedAt) / 1e9,
        lastSuccessSeconds: before ? before.lastSuccessSeconds : 0,
      }
    }
  }))
  state.snapshot = { results, takenAt: Date.now() }
  state.cycles++
  if (!state.started && selfChecksOk(results)) {
    state.started = true
    log('info', 'startup complete: self checks passed')
  }
  for (const name of Object.keys(results)) {
    const before = previousResults[name]
    if (before && before.ok !== results[name].ok) {
      log('info', `health check '${name}' transitioned ${before.ok} -> ${results[name].ok} (${results[name].detail})`,
        { 'health.check': name, 'health.up': results[name].ok })
    }
  }
  previousResults = results
}

// ---------------------------------------------------------------------------
// admin listener: probes and metrics (never logged per hit)
// ---------------------------------------------------------------------------

const adminServer = createServerOriginal((request, response) => {
  const url = new URL(request.url, 'http://localhost')
  const respond = (up) => {
    const status = up ? 200 : 503
    if (url.searchParams.get('verbose') === '1') {
      response.writeHead(status, { 'Content-Type': 'application/json' })
      response.end(JSON.stringify({
        status: up ? 'ok' : 'unavailable',
        draining: state.draining,
        snapshotAgeMillis: state.snapshot.takenAt === 0 ? -1 : Date.now() - state.snapshot.takenAt,
        checks: state.snapshot.results,
      }))
    } else {
      response.writeHead(status, { 'Content-Type': 'text/plain; charset=utf-8' })
      response.end(up ? 'ok' : 'unavailable')
    }
  }
  switch (url.pathname) {
    case '/startupz':
      return respond(state.started)
    case '/livez':
      return respond(isFresh())
    case '/readyz':
      return respond(isReady())
    case '/metrics':
      renderMetrics().then((body) => {
        response.writeHead(200, { 'Content-Type': 'application/openmetrics-text; version=1.0.0; charset=utf-8' })
        response.end(body)
      }, (error) => {
        log('error', `metrics rendering failed: ${error && error.stack ? error.stack : error}`)
        response.writeHead(500, { 'Content-Type': 'text/plain; charset=utf-8' })
        response.end('metrics unavailable')
      })
      return undefined
    default:
      response.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' })
      return response.end('not found')
  }
})
adminServer.on('connection', (socket) => {
  metrics.connections.inc(['admin'])
  socket.once('close', () => metrics.connections.dec(['admin']))
})
adminServer.listen(cfg.adminPort, cfg.bindAddr)

// ---------------------------------------------------------------------------
// drain sequence
// ---------------------------------------------------------------------------

const sleep = (millis) => new Promise((resolve) => setTimeout(resolve, millis))

let drainStarted = false
async function drain(signalName) {
  if (drainStarted) return
  drainStarted = true
  log('info', `received ${signalName}: draining (readiness now 503, drain delay ${cfg.drainDelaySec}s)`)
  state.draining = true
  await sleep(cfg.drainDelaySec * 1000)
  const begin = Date.now()
  const closePromises = capturedServers.map((server) => new Promise((resolve) => {
    server.close(() => resolve())
    server.closeIdleConnections()
  }))
  const outcome = await Promise.race([
    Promise.all(closePromises).then(() => 'drained'),
    sleep(cfg.drainBudgetSec * 1000).then(() => 'timeout'),
  ])
  let exitCode = 0
  if (outcome === 'timeout') {
    log('error', `drain budget of ${cfg.drainBudgetSec}s exhausted; closing remaining connections`)
    capturedServers.forEach((server) => server.closeAllConnections())
    exitCode = 1
  } else {
    log('info', `graceful shutdown complete in ${Date.now() - begin}ms`)
  }
  adminServer.close()
  process.exit(exitCode)
}

process.on('SIGTERM', () => { drain('SIGTERM') })
process.on('SIGINT', () => { drain('SIGINT') })

runCycle()
setInterval(runCycle, cfg.checkIntervalSec * 1000).unref()

// ---------------------------------------------------------------------------
// the Next.js standalone server, loaded into this process
// ---------------------------------------------------------------------------

process.env.NEXT_MANUAL_SIG_HANDLE = 'true'
process.env.NODE_ENV = 'production'
process.env.PORT = String(cfg.port)
process.env.HOSTNAME = cfg.bindAddr
process.chdir(APP_DIR)

log('info', `auth-portal starting: version=${APP_VERSION} revision=${APP_REVISION} loading the Next.js standalone server`)
captureConsole()

try {
  require(path.join(APP_DIR, 'server.js'))
} catch (error) {
  log('error', `failed to start Next.js standalone server: ${error && error.stack ? error.stack : error}`)
  process.exit(1)
}
