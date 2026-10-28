// One original deadline covers loading and a zero-request quiet interval.
// The public wildcard event observes requests before interception callbacks run.
import assert from 'node:assert/strict'
import {performance} from 'node:perf_hooks'

const observed = new WeakMap()
const maximumTrackedRequests = 256

export function observeNavigationRequests(page) {
  if (observed.has(page)) return observed.get(page)
  const active = new Map(), subscribers = new Set()
  let changed = performance.now(), closed = false, broken = '', peak = 0
  let started = 0, completed = 0, unmatchedCompletionSignals = 0
  const notify = () => {for (const listener of [...subscribers]) listener()}
  const finish = request => {
    if (!active.delete(request)) {unmatchedCompletionSignals++; return}
    completed++; changed = performance.now(); notify()
  }
  const listener = (type, event) => {
    if (type === 'request') {
      if (active.has(event)) return
      if (!event || typeof event.url !== 'function') broken = 'invalid navigation request event'
      else if (active.size === maximumTrackedRequests) broken = 'navigation request tracking limit exceeded'
      else {active.set(event, performance.now()); started++; peak = Math.max(peak, active.size)}
      changed = performance.now(); notify()
    } else if (type === 'response') finish(event.request())
    else if (type === 'requestfinished' || type === 'requestfailed') finish(event)
    else if (type === 'close') {closed = true; page.off('*', listener); notify()}
  }
  page.on('*', listener)
  const diagnostic = () => ({activeRequests: active.size, maximumTrackedRequests, peak, started, completed,
    unmatchedCompletionSignals, requests: [...active].slice(0, 8).map(([request, at]) => {
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
          if (remaining <= 0) return end(new Error('original navigation deadline elapsed; network=' + JSON.stringify(diagnostic())))
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
  let attempts = 0, completed = 0, failed = 0, totalMilliseconds = 0, maximumMilliseconds = 0
  return {
    async goto(page, href) {
      assert.equal(new URL(href).origin, selected.origin)
      const began = performance.now(), deadline = began + timeoutMilliseconds
      const tracker = observeNavigationRequests(page)
      attempts++
      try {
        const response = await page.goto(String(href), {waitUntil: 'domcontentloaded', timeout: timeoutMilliseconds})
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
      return {attempts, completed, failed, totalMilliseconds: Math.round(totalMilliseconds * 100) / 100,
        maximumMilliseconds: Math.round(maximumMilliseconds * 100) / 100, timeoutMilliseconds,
        quietMilliseconds, maximumInflightRequests: 0, lifecycle: 'domcontentloaded',
        previousQuietMilliseconds: 500, oneOverallOriginalDeadline: true,
        requestTracking: 'public-request-identity-events', maximumTrackedRequests,
        networkRequestTypesFiltered: 0, DOMErrorSignatureReceiptAndBalanceControlsRemoved: 0,
        productAPIOrRPCRepliesReplaced: 0}
    },
  }
}
