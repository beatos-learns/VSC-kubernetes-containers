'use strict'

// PID 1 bootstrap for the auth-portal image, in-process checker topology: it
// loads the Next.js standalone server into this same Node process, serves the
// probe endpoints and metrics on ADMIN_PORT, runs the cached health checker,
// and drives the SIGTERM/SIGINT drain sequence with real connection draining.

const http = require('node:http')
const path = require('node:path')

const LOG_LEVELS = { trace: 0, debug: 1, info: 2, warn: 3, error: 4 }

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

const cfg = {
  port: envInt('PORT', 3000, 1, 65535),
  adminPort: envInt('ADMIN_PORT', 9090, 1, 65535),
  bindAddr: envStr('BIND_ADDR', '0.0.0.0'),
  logLevel: envEnum('LOG_LEVEL', 'info', ['trace', 'debug', 'info', 'warn', 'error']),
  logFormat: envEnum('LOG_FORMAT', 'json', ['json', 'text']),
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
try {
  new URL(cfg.apiUrl)
} catch {
  fatal(`API_URL=${cfg.apiUrl} is not a valid URL`)
}
cfg.apiUrl = cfg.apiUrl.replace(/\/+$/, '')
process.env.API_URL = cfg.apiUrl

function log(level, message) {
  if (LOG_LEVELS[level] < LOG_LEVELS[cfg.logLevel]) return
  if (cfg.logFormat === 'json') {
    process.stderr.write(JSON.stringify({
      time: new Date().toISOString(), level, logger: 'bootstrap', msg: message,
    }) + '\n')
  } else {
    process.stderr.write(`${new Date().toISOString()} ${level.toUpperCase().padEnd(5)} bootstrap: ${message}\n`)
  }
}

const state = {
  snapshot: { results: {}, takenAt: 0 },
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
// sequence can stop intake and close real connections.
const capturedServers = []
const createServerOriginal = http.createServer
http.createServer = function patchedCreateServer(...args) {
  const server = createServerOriginal.apply(http, args)
  capturedServers.push(server)
  server.once('listening', () => {
    log('info', `auth-portal listening: version=${envStr('APP_VERSION', 'dev')} ` +
      `revision=${envStr('APP_REVISION', 'unknown')} config[PORT=${cfg.port} ADMIN_PORT=${cfg.adminPort} ` +
      `BIND_ADDR=${cfg.bindAddr} LOG_LEVEL=${cfg.logLevel} LOG_FORMAT=${cfg.logFormat} ` +
      `HEALTH_CHECK_INTERVAL=${cfg.checkIntervalSec} HEALTH_CHECK_TIMEOUT=${cfg.checkTimeoutSec} ` +
      `HEALTH_STALE_FACTOR=${cfg.staleFactor} SHUTDOWN_DRAIN_DELAY=${cfg.drainDelaySec} ` +
      `SHUTDOWN_TIMEOUT=${cfg.drainBudgetSec} API_URL=${cfg.apiUrl}]`)
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
    await fetch(target, {
      method: 'GET',
      signal: AbortSignal.timeout(cfg.checkTimeoutSec * 1000),
    })
  },
}

let previousResults = {}
async function runCycle() {
  const results = {}
  await Promise.all(Object.entries(checks).map(async ([name, run]) => {
    try {
      await run()
      results[name] = { ok: true, detail: 'ok' }
    } catch (error) {
      results[name] = { ok: false, detail: String(error && error.message ? error.message : error) }
    }
  }))
  state.snapshot = { results, takenAt: Date.now() }
  if (!state.started && allOk(results)) {
    state.started = true
    log('info', 'startup complete: first fully successful health cycle')
  }
  for (const name of Object.keys(results)) {
    const before = previousResults[name]
    if (before && before.ok !== results[name].ok) {
      log('info', `health check '${name}' transitioned ${before.ok} -> ${results[name].ok} (${results[name].detail})`)
    }
  }
  previousResults = results
}

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
    case '/metrics': {
      let body = '# TYPE build_info gauge\n'
      body += `build_info{version="${envStr('APP_VERSION', 'dev')}",revision="${envStr('APP_REVISION', 'unknown')}"} 1\n`
      body += '# TYPE health_check_up gauge\n'
      for (const [name, result] of Object.entries(state.snapshot.results)) {
        body += `health_check_up{check="${name}"} ${result.ok ? 1 : 0}\n`
      }
      body += '# EOF\n'
      response.writeHead(200, { 'Content-Type': 'application/openmetrics-text; version=1.0.0; charset=utf-8' })
      return response.end(body)
    }
    default:
      response.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' })
      return response.end('not found')
  }
})
adminServer.listen(cfg.adminPort, cfg.bindAddr)

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

process.env.NEXT_MANUAL_SIG_HANDLE = 'true'
process.env.NODE_ENV = 'production'
process.env.PORT = String(cfg.port)
process.env.HOSTNAME = cfg.bindAddr
process.chdir(path.join(__dirname, '..'))

try {
  require(path.join(__dirname, '..', 'server.js'))
} catch (error) {
  log('error', `failed to start Next.js standalone server: ${error && error.stack ? error.stack : error}`)
  process.exit(1)
}
