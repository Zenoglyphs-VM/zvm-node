// Observe Network events directly; interception callbacks also describe
// operations that have no corresponding Network request or completion.
import assert from 'node:assert/strict'
import {performance} from 'node:perf_hooks'
import {createPageClientObserver} from './market-page-client-support.mjs'
import {readPageRequestIdentity, pageRequestIdMatches} from './market-request-identity-support.mjs'

const observed = new WeakMap()
const maximumTrackedRequests = 256
const maximumRecentSignals = 8
const diagnosticSignalTypes = new Set(['Network.requestWillBeSentExtraInfo', 'Network.responseReceivedExtraInfo',
  'Network.responseReceived', 'Network.loadingFinished', 'Network.loadingFailed', 'Network.requestServedFromCache'])

function rememberSignal(entry, type, event) {
  const status = event.response?.status ?? event.statusCode
  const signal = {event: type.slice('Network.'.length), timestampPresent: Number.isFinite(event.timestamp),
    timestampBeforeRequest: Number.isFinite(event.timestamp) ? event.timestamp < entry.timestamp : null,
    responseURLMatchesRequest: event.response ? event.response.url === entry.url : null,
    status: Number.isInteger(status) && status >= 100 && status <= 599 ? status : null}
  if (entry.recentSignals.length === maximumRecentSignals) entry.recentSignals.shift()
  entry.recentSignals.push(signal)
}


// Page objects give a second, passive view of the intercepted HTTP lifecycle.
// A URL/method match is diagnostic correlation, never a protocol identity or
// proof that a request missing from this Network session has completed.
function observePageLifecycle(page) {
  const active = new Map(), recent = new Map()
  const pageClient = createPageClientObserver(active, recent)
  let started = 0, finished = 0, failed = 0, unmatchedSignals = 0, readErrors = 0, truncated = false
  const read = request => {
    const url = request.url(), method = request.method()
    if (typeof url !== 'string' || url.length > 8192 || typeof method !== 'string' || method.length > 32) throw new Error('invalid passive request fields')
    return {url, method}
  }
  const capture = (entry, request, event, response) => {
    if (entry.signals.length === maximumRecentSignals) entry.signals.shift()
    entry.signals.push(event)
    try {
      entry.interceptResolutionHandled = typeof request.isInterceptResolutionHandled === 'function' ? request.isInterceptResolutionHandled() === true : null
      const status = response?.status()
      if (Number.isInteger(status) && status >= 100 && status <= 599) entry.responseStatus = status
      if (event === 'requestfailed') {
        const error = request.failure()?.errorText
        entry.failureClass = ['net::ERR_ABORTED', 'net::ERR_FAILED', 'net::ERR_BLOCKED_BY_CLIENT'].includes(error) ? error : 'other'
      }
    } catch {readErrors++}
  }
  const onRequest = request => {
    if (active.has(request) || recent.has(request)) return
    if (active.size === maximumTrackedRequests) {truncated = true; return}
    try {
      const entry = {...read(request), requestIdentity: readPageRequestIdentity(request, page), signals: [], responseStatus: null, interceptResolutionHandled: null,
        terminal: null, failureClass: null}
      capture(entry, request, 'request')
      active.set(request, entry); started++
      pageClient.observe(request, entry)
    } catch {readErrors++}
  }
  const onResponse = response => {
    try {
      const request = response.request(), entry = active.get(request) ?? recent.get(request)
      if (entry) capture(entry, request, 'response', response)
      else unmatchedSignals++
    } catch {readErrors++}
  }
  const terminal = (request, event) => {
    const entry = active.get(request)
    if (!entry) {unmatchedSignals++; return}
    capture(entry, request, event)
    entry.terminal = event
    active.delete(request)
    if (recent.size === maximumRecentSignals) recent.delete(recent.keys().next().value)
    recent.set(request, entry)
    if (event === 'requestfinished') finished++; else failed++
  }
  const listeners = {request: onRequest, response: onResponse,
    requestfinished: request => terminal(request, 'requestfinished'),
    requestfailed: request => terminal(request, 'requestfailed')}
  for (const [name, listener] of Object.entries(listeners)) page.on(name, listener)
  return {
    close() {for (const [name, listener] of Object.entries(listeners)) page.off(name, listener); pageClient.close()},
    summary() {return {started, finished, failed, active: active.size, recent: recent.size, unmatchedSignals,
      readErrors, truncated, maximumActiveRecords: maximumTrackedRequests, maximumRecentRecords: maximumRecentSignals,
      passiveOnly: true, completionGateChanged: false}},
    matchesClient(id, url, method) {return pageClient.matches(id, url, method)},
    matchesRequestId(id, url, method) {return pageRequestIdMatches(active, recent, id, url, method)},
    matches(url, method) {
      const matching = records => [...records.values()].filter(entry => entry.url === url && entry.method === method)
      const live = matching(active), ended = matching(recent)
      return {sameURLAndMethodOnly: true, protocolIdentityEstablished: false, activeMatches: live.length,
        recentMatches: ended.length, observations: [...live, ...ended].slice(0, maximumRecentSignals).map(entry => ({
          signals: [...entry.signals], responseStatus: entry.responseStatus,
          interceptResolutionHandled: entry.interceptResolutionHandled, terminal: entry.terminal, failureClass: entry.failureClass}))}
    },
  }
}

export function createNavigationRequestTracker(session, page) {
  const pageLifecycle = page ? observePageLifecycle(page) : null
  const active = new Map(), subscribers = new Set()
  let changed = performance.now(), closed = false, broken = '', peak = 0
  let started = 0, completed = 0, documents = 0, cacheSignals = 0, unmatchedCompletionSignals = 0
  const notify = () => {for (const listener of [...subscribers]) listener()}
  const identity = event => {
    if (!event || typeof event.requestId !== 'string' || !event.requestId) {
      broken = 'invalid navigation protocol request identity'; notify(); return null
    }
    return event.requestId
  }
  const finish = event => {
    const id = identity(event)
    if (id === null) return
    const entry = active.get(id)
    if (!entry || !Number.isFinite(event.timestamp) || event.timestamp < entry.timestamp ||
        (event.response && event.response.url !== entry.url)) {unmatchedCompletionSignals++; return}
    active.delete(id); completed++; changed = performance.now(); notify()
  }
  const listener = (type, event) => {
    if (session.detached) {close(); return}
    // Passive evidence only. Extra-info/cache signals never complete a request.
    const tracked = typeof event?.requestId === 'string' ? active.get(event.requestId) : undefined
    if (tracked && diagnosticSignalTypes.has(type)) rememberSignal(tracked, type, event)
    if (type === 'Network.requestWillBeSent') {
      const id = identity(event)
      if (id === null) return
      if (!event.request || typeof event.request.url !== 'string' || typeof event.request.method !== 'string' ||
          !Number.isFinite(event.timestamp)) {broken = 'invalid navigation protocol request'; notify(); return}
      const previous = active.get(id)
      if (previous) {
        if (event.redirectResponse) {active.delete(id); completed++}
        else {
          if (previous.url !== event.request.url || previous.method !== event.request.method) broken = 'navigation protocol identity reused while active'
          notify(); return
        }
      }
      if (active.size === maximumTrackedRequests) broken = 'navigation request tracking limit exceeded'
      else {
        active.set(id, {url: event.request.url, method: event.request.method, timestamp: event.timestamp, at: performance.now(), recentSignals: []})
        started++; if (event.type === 'Document') documents++
        peak = Math.max(peak, active.size)
      }
      changed = performance.now(); notify()
    } else if (type === 'Network.responseReceived' || type === 'Network.loadingFinished' || type === 'Network.loadingFailed') finish(event)
    else if (type === 'Network.requestServedFromCache') {
      // Cache provenance alone is not a response or a terminal loading event.
      if (identity(event) !== null) cacheSignals++
    }
  }
  const close = () => {closed = true; session.off('*', listener); pageLifecycle?.close(); notify()}
  session.on('*', listener)
  const diagnostic = () => ({activeRequests: active.size, maximumTrackedRequests, peak, started, completed,
    documentRequestsStarted: documents, cacheSignals, unmatchedCompletionSignals,
    pageLifecycle: pageLifecycle?.summary() ?? null,
    requests: [...active.entries()].slice(0, 8).map(([id, {url, method, at, recentSignals}]) => {
      let path = '[unparseable]'
      try {path = new URL(url).pathname.slice(0, 120)} catch {}
      return {method: method.slice(0, 12), path, ageMilliseconds: Math.round(performance.now() - at), recentSignals: recentSignals.map(signal => ({...signal})),
        pageLifecycleMatches: pageLifecycle?.matches(url, method) ?? null,
        pageRequestIdMatches: pageLifecycle?.matchesRequestId(id, url, method) ?? null,
        pageClientMatches: pageLifecycle?.matchesClient(id, url, method) ?? null}
    })})
  return {
    close, diagnostic,
    wait(deadline, quietMilliseconds = 100) {
      assert.ok(Number.isFinite(deadline) && quietMilliseconds > 0)
      const began = performance.now()
      return new Promise((resolve, reject) => {
        let timer, settled = false
        const end = error => {
          if (settled) return
          settled = true; clearTimeout(timer); subscribers.delete(check)
          if (error) reject(error); else resolve()
        }
        const check = () => {
          clearTimeout(timer)
          const now = performance.now(), remaining = deadline - now
          if (closed || session.detached) return end(new Error('navigation page closed before network quiet'))
          if (broken) return end(new Error(broken))
          if (remaining <= 0) {
            const value = diagnostic()
            console.log('NAVIGATION-NETWORK-DEADLINE '+JSON.stringify(value))
            return end(new Error('original navigation deadline elapsed; network=' + JSON.stringify(value)))
          }
          const quietRemaining = quietMilliseconds - (now - Math.max(changed, began))
          if (active.size === 0 && quietRemaining <= 0) return end()
          timer = setTimeout(check, Math.max(1, Math.min(remaining, active.size ? remaining : quietRemaining)))
        }
        subscribers.add(check); check()
      })
    },
  }
}

export function observeNavigationRequests(page) {
  if (observed.has(page)) return observed.get(page)
  let session, tracker, closed = false
  const close = () => {
    closed = true; tracker?.close()
    if (session && !session.detached) void session.detach().catch(() => {})
  }
  page.once('close', close)
  const pending = (async () => {
    try {
      session = await page.createCDPSession()
      if (closed) throw new Error('navigation page closed during network setup')
      tracker = createNavigationRequestTracker(session, page)
      await session.send('Network.enable', {}, {timeout: 5000})
      if (closed || session.detached) throw new Error('navigation page closed during network setup')
      return tracker
    } catch (error) {page.off('close', close); close(); throw error}
  })()
  observed.set(page, pending)
  return pending
}

async function withinDeadline(promise, deadline) {
  let timer
  const remaining = deadline-performance.now()
  assert.ok(remaining > 0, 'original navigation deadline elapsed')
  try {
    return await Promise.race([promise, new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error('original navigation deadline elapsed during network setup')), remaining)
    })])
  } finally {clearTimeout(timer)}
}

export function boundedNavigation(origin) {
  const selected = new URL(origin)
  assert.equal(selected.protocol, 'http:'); assert.equal(selected.hostname, '127.0.0.1')
  const timeoutMilliseconds = 20000, quietMilliseconds = 100
  const trackers = new Set()
  let attempts = 0, completed = 0, failed = 0, totalMilliseconds = 0, maximumMilliseconds = 0
  return {
    async goto(page, href) {
      assert.equal(new URL(href).origin, selected.origin)
      const began = performance.now(), deadline = began + timeoutMilliseconds
      attempts++
      try {
        const tracker = await withinDeadline(observeNavigationRequests(page), deadline)
        trackers.add(tracker)
        const before = tracker.diagnostic().documentRequestsStarted
        const remaining = deadline-performance.now()
        assert.ok(remaining > 0, 'original navigation deadline elapsed')
        const response = await page.goto(String(href), {waitUntil: 'domcontentloaded', timeout: remaining})
        assert.ok(tracker.diagnostic().documentRequestsStarted > before, 'navigation document request events were not observed')
        await tracker.wait(deadline, quietMilliseconds)
        assert.ok(performance.now() <= deadline, 'original overall navigation deadline exceeded')
        completed++; return response
      } catch (error) {failed++; throw error}
      finally {
        const elapsed = performance.now()-began
        totalMilliseconds += elapsed; maximumMilliseconds = Math.max(maximumMilliseconds, elapsed)
      }
    },
    summary() {
      const values = [...trackers].map(tracker => tracker.diagnostic())
      return {attempts, completed, failed, totalMilliseconds: Math.round(totalMilliseconds*100)/100,
        maximumMilliseconds: Math.round(maximumMilliseconds*100)/100, timeoutMilliseconds,
        quietMilliseconds, maximumInflightRequests: 0, lifecycle: 'domcontentloaded',
        previousQuietMilliseconds: 500, oneOverallOriginalDeadline: true,
        requestTracking: 'cdp-network-request-identities', maximumTrackedRequests,
        networkProtocolSessions: values.length, networkProtocolCacheSignals: values.reduce((sum, value) => sum+value.cacheSignals, 0),
        networkRequestsStarted: values.reduce((sum, value) => sum+value.started, 0),
        networkRequestsCompleted: values.reduce((sum, value) => sum+value.completed, 0),
        maximumObservedInflightRequests: Math.max(0, ...values.map(value => value.peak)),
        networkRequestTypesFiltered: 0, DOMErrorSignatureReceiptAndBalanceControlsRemoved: 0,
        productAPIOrRPCRepliesReplaced: 0}
    },
  }
}
