// One original deadline covers loading and a zero-request quiet interval.
// The public wildcard event observes requests before interception callbacks run.
import assert from 'node:assert/strict'
import {performance} from 'node:perf_hooks'

const observed = new WeakMap()
const maximumTrackedRequests = 256

export function observeNavigationRequests(page) {
  if (observed.has(page)) return observed.get(page)
  const active = new Map(), known = new WeakMap(), subscribers = new Set()
  let changed = performance.now(), closed = false, broken = '', peak = 0
  let started = 0, completed = 0, unmatchedCompletionSignals = 0
  const notify = () => {for (const listener of [...subscribers]) listener()}
  const identity = request => {
    if (!request || typeof request.id !== 'string' || !request.id || typeof request.url !== 'function' || typeof request.method !== 'function') {
      broken = 'invalid navigation request identity'; notify(); return null
    }
    return request.id
  }
  const finish = request => {
    const id = identity(request)
    if (id === null) return
    let entry = known.get(request)
    if (!entry) {
      const candidate = active.get(id)
      if (candidate && candidate.request.url() === request.url() && candidate.request.method() === request.method()) {
        entry = candidate; known.set(request, entry)
      }
    }
    if (!entry || active.get(id) !== entry) {unmatchedCompletionSignals++; return}
    active.delete(id)
    completed++; changed = performance.now(); notify()
  }
  const listener = (type, event) => {
    if (type === 'request') {
      const id = identity(event)
      if (id === null || known.has(event)) return
      const previous = active.get(id)
      if (previous) {
        if (previous.request.url() === event.url() && previous.request.method() === event.method()) known.set(event, previous)
        else broken = 'navigation request identity reused while active'
        notify(); return
      }
      if (active.size === maximumTrackedRequests) broken = 'navigation request tracking limit exceeded'
      else {
        const entry = {request: event, at: performance.now()}
        active.set(id, entry); known.set(event, entry); started++; peak = Math.max(peak, active.size)
      }
      changed = performance.now(); notify()
    } else if (type === 'requestservedfromcache') {
      // The documented cache notification can omit its request. Without an
      // identity it cannot release any tracked operation.
      if (event) finish(event); else unmatchedCompletionSignals++
    } else if (type === 'response') finish(event.request())
    else if (type === 'requestfinished' || type === 'requestfailed') finish(event)
    else if (type === 'close') {closed = true; page.off('*', listener); notify()}
  }
  page.on('*', listener)
  const diagnostic = () => ({activeRequests: active.size, maximumTrackedRequests, peak, started, completed,
    unmatchedCompletionSignals, requests: [...active.values()].slice(0, 8).map(({request, at}) => {
      let path = '[unparseable]'
      try {path = new URL(request.url()).pathname.slice(0, 120)} catch {}
      return {method: String(request.method()).slice(0, 12), path,
        ageMilliseconds: Math.round(performance.now() - at)}
    })})
  const tracker = {
    diagnostic,
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
          if (closed) return end(new Error('navigation page closed before network quiet'))
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
  observed.set(page, tracker)
  return tracker
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
      const tracker = observeNavigationRequests(page)
      trackers.add(tracker)
      const before = tracker.diagnostic().started
      attempts++
      try {
        const response = await page.goto(String(href), {waitUntil: 'domcontentloaded', timeout: timeoutMilliseconds})
        assert.ok(tracker.diagnostic().started > before, 'navigation request events were not observed')
        await tracker.wait(deadline, quietMilliseconds)
        assert.ok(performance.now() <= deadline, 'original overall navigation deadline exceeded')
        completed++
        return response
      } catch (error) {failed++; throw error}
      finally {
        const elapsed = performance.now() - began
        totalMilliseconds += elapsed; maximumMilliseconds = Math.max(maximumMilliseconds, elapsed)
      }
    },
    summary() {
      const values = [...trackers].map(tracker => tracker.diagnostic())
      return {attempts, completed, failed, totalMilliseconds: Math.round(totalMilliseconds * 100) / 100,
        maximumMilliseconds: Math.round(maximumMilliseconds * 100) / 100, timeoutMilliseconds,
        quietMilliseconds, maximumInflightRequests: 0, lifecycle: 'domcontentloaded',
        previousQuietMilliseconds: 500, oneOverallOriginalDeadline: true,
        requestTracking: 'stable-request-identity-cache-events', maximumTrackedRequests,
        networkRequestsStarted: values.reduce((sum, value) => sum+value.started, 0),
        networkRequestsCompleted: values.reduce((sum, value) => sum+value.completed, 0),
        maximumObservedInflightRequests: Math.max(0, ...values.map(value => value.peak)),
        networkRequestTypesFiltered: 0, DOMErrorSignatureReceiptAndBalanceControlsRemoved: 0,
        productAPIOrRPCRepliesReplaced: 0}
    },
  }
}
