'use strict'

// Exec-style probe: `node /app/ops/probe.cjs --endpoint=livez` performs a local
// HTTP GET against ADMIN_PORT and exits 0/1, keeping the image probeable
// without curl or wget.

const http = require('node:http')

const ENDPOINTS = ['startupz', 'livez', 'readyz']

let endpoint = 'livez'
for (const arg of process.argv.slice(2)) {
  if (arg.startsWith('--endpoint=')) endpoint = arg.slice('--endpoint='.length)
}
if (!ENDPOINTS.includes(endpoint)) {
  console.error(`unknown endpoint '${endpoint}'; allowed: ${ENDPOINTS.join(', ')}`)
  process.exit(1)
}

const adminPort = process.env.ADMIN_PORT && process.env.ADMIN_PORT.trim() !== ''
  ? process.env.ADMIN_PORT.trim()
  : '9090'

const bindAddr = process.env.BIND_ADDR && process.env.BIND_ADDR.trim() !== ''
  ? process.env.BIND_ADDR.trim()
  : '0.0.0.0'
const probeHost = bindAddr === '0.0.0.0' ? '127.0.0.1' : bindAddr === '::' ? '::1' : bindAddr

const request = http.get({ host: probeHost, port: adminPort, path: '/' + endpoint, timeout: 3000 }, (response) => {
  response.resume()
  process.exit(response.statusCode === 200 ? 0 : 1)
})
request.on('timeout', () => {
  request.destroy()
  console.error('probe failed: timeout')
  process.exit(1)
})
request.on('error', (error) => {
  console.error('probe failed: ' + error.message)
  process.exit(1)
})
