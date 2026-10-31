// This read-only fixture observation never completes a navigation or an RPC.
// No request body, URL, capability, response text or error message is reported.
export const RPC_PROGRESS_BODY_LIMIT = 32768
export const RPC_PROGRESS_TIMEOUT = 750
const scope = 'owned-loopback-POST-rpc-handler-lifecycle'
const keys = ['version', 'scope', 'maximumTrackedActive', 'maximumRecent', 'started', 'finished',
  'normalReturns', 'abnormalExits', 'active', 'overflowStarted', 'untrackedActive', 'completeTracking',
  'activeRecords', 'recent', 'HTTPStatusBodyOrDeliveryObserved', 'protocolRequestIdentityEstablished',
  'responseWriterOrRequestReplaced']
const activeKeys = ['sequence', 'ageMilliseconds', 'contextCanceled']
const recentKeys = ['sequence', 'durationMilliseconds', 'returnedNormally', 'contextCanceled']
const exact = (value, expected) => value && typeof value === 'object' && !Array.isArray(value) &&
  Object.keys(value).length === expected.length && expected.every(key => Object.hasOwn(value, key))
const number = value => Number.isSafeInteger(value) && value >= 0
const unavailable = reason => ({status: 'UNAVAILABLE', reason, qualifiesNavigation: false})

export function validateOwnedRPCSnapshot(value) {
  const refuse = () => {throw new Error('invalid owned RPC snapshot')}
  if (!exact(value, keys) || value.version !== 1 || value.scope !== scope ||
    value.maximumTrackedActive !== 32 || value.maximumRecent !== 8) refuse()
  for (const key of ['started', 'finished', 'normalReturns', 'abnormalExits', 'active', 'overflowStarted', 'untrackedActive']) {
    if (!number(value[key])) refuse()
  }
  if (value.finished > value.started || value.normalReturns + value.abnormalExits !== value.finished ||
    value.active !== value.started - value.finished || value.untrackedActive > value.active ||
    value.untrackedActive > value.overflowStarted || value.overflowStarted > value.started ||
    value.completeTracking !== (value.overflowStarted === 0) ||
    value.HTTPStatusBodyOrDeliveryObserved !== false || value.protocolRequestIdentityEstablished !== false ||
    value.responseWriterOrRequestReplaced !== false) refuse()
  if (!Array.isArray(value.activeRecords) || value.activeRecords.length > 32 ||
    value.activeRecords.length + value.untrackedActive !== value.active ||
    !Array.isArray(value.recent) || value.recent.length !== Math.min(value.finished, 8)) refuse()
  const seen = new Set()
  let previous = 0
  for (const row of value.activeRecords) {
    if (!exact(row, activeKeys) || !number(row.sequence) || row.sequence <= previous || row.sequence > value.started ||
      !number(row.ageMilliseconds) || typeof row.contextCanceled !== 'boolean') refuse()
    seen.add(row.sequence); previous = row.sequence
  }
  for (const row of value.recent) {
    if (!exact(row, recentKeys) || !number(row.sequence) || row.sequence < 1 || row.sequence > value.started ||
      seen.has(row.sequence) || !number(row.durationMilliseconds) || typeof row.contextCanceled !== 'boolean' ||
      typeof row.returnedNormally !== 'boolean') refuse()
    seen.add(row.sequence)
  }
  return value
}

async function boundedBody(response) {
  const reader = response.body?.getReader()
  if (!reader) return unavailable('SCHEMA')
  const chunks = []
  let size = 0, reads = 0
  try {
    for (;;) {
      const {done, value} = await reader.read()
      if (done) break
      reads++
      if (!(value instanceof Uint8Array) || reads > 128 || size + value.byteLength > RPC_PROGRESS_BODY_LIMIT) {
        void reader.cancel().catch(() => {})
        return unavailable('BODY_LIMIT')
      }
      size += value.byteLength; chunks.push(value)
    }
  } finally { reader.releaseLock() }
  const raw = new Uint8Array(size)
  let offset = 0
  for (const chunk of chunks) { raw.set(chunk, offset); offset += chunk.byteLength }
  const value = JSON.parse(new TextDecoder('utf-8', {fatal: true}).decode(raw))
  return {status: 'AVAILABLE', snapshot: validateOwnedRPCSnapshot(value), qualifiesNavigation: false}
}

export async function readOwnedRPCProgress(base, capability, {fetchImpl = globalThis.fetch, timeoutMilliseconds = RPC_PROGRESS_TIMEOUT} = {}) {
  let endpoint
  try {
    if (typeof base !== 'string' || base.length > 2048 || typeof capability !== 'string' ||
      !/^[a-f0-9]{64}$/.test(capability) || typeof fetchImpl !== 'function' ||
      !Number.isSafeInteger(timeoutMilliseconds) || timeoutMilliseconds < 1 || timeoutMilliseconds > RPC_PROGRESS_TIMEOUT) return unavailable('CONFIGURATION')
    const origin = new URL(base)
    if (origin.protocol !== 'http:' || origin.hostname !== '127.0.0.1' || !origin.port ||
      origin.username || origin.password || origin.search || origin.hash || origin.pathname !== '/') return unavailable('CONFIGURATION')
    endpoint = new URL('/__fixture/rpc-progress', origin)
  } catch { return unavailable('CONFIGURATION') }
  const controller = new AbortController()
  let timer
  try {
    const deadline = new Promise(resolve => {timer = setTimeout(() => {
      controller.abort(); resolve(unavailable('DEADLINE'))
    }, timeoutMilliseconds)})
    const observation = (async () => {
      try {
        const response = await fetchImpl(endpoint, {method: 'GET', redirect: 'error',
          headers: {'X-ZVM-Fixture': capability}, signal: controller.signal})
        if (response.status !== 200) return unavailable('HTTP_STATUS')
        if (!response.headers.get('content-type')?.toLowerCase().startsWith('application/json')) return unavailable('CONTENT_TYPE')
        return await boundedBody(response)
      } catch {
        return unavailable(controller.signal.aborted ? 'DEADLINE' : 'TRANSPORT_OR_SCHEMA')
      }
    })()
    return await Promise.race([observation, deadline])
  } finally { clearTimeout(timer) }
}

export async function reportOwnedRPCProgress(fixture, browser) {
  const connected = () => {try {return typeof browser?.isConnected === 'function' ? browser.isConnected() : null} catch {return null}}
  const before = connected()
  const observation = await readOwnedRPCProgress(fixture.base, fixture.token)
  console.log('OWNED-MARKET-RPC-PROGRESS ' + JSON.stringify({phase: 'BEFORE_OWNED_BROWSER_CLOSE',
    browserConnectedBefore: before, browserConnectedAfter: connected(), ...observation}))
}
