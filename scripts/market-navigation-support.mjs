// One original navigation deadline covers document loading and network quiet.
// API contract: Puppeteer 25.11.0 Page.waitForNetworkIdle, with zero inflight.
import assert from 'node:assert/strict'
import {performance} from 'node:perf_hooks'

export function boundedNavigation(origin) {
  const selected = new URL(origin)
  assert.equal(selected.protocol, 'http:'); assert.equal(selected.hostname, '127.0.0.1')
  const timeoutMilliseconds = 20000, quietMilliseconds = 100
  let attempts = 0, completed = 0, failed = 0, totalMilliseconds = 0, maximumMilliseconds = 0
  return {
    async goto(page, href) {
      assert.equal(new URL(href).origin, selected.origin)
      const started = performance.now(), deadline = started + timeoutMilliseconds
      attempts++
      try {
        const response = await page.goto(String(href), {waitUntil: 'domcontentloaded', timeout: timeoutMilliseconds})
        const remaining = Math.floor(deadline - performance.now())
        assert.ok(remaining > 0, 'original navigation deadline elapsed before network quiet')
        await page.waitForNetworkIdle({idleTime: quietMilliseconds, concurrency: 0, timeout: remaining})
        assert.ok(performance.now() <= deadline, 'original overall navigation deadline exceeded')
        completed++
        return response
      } catch (error) {
        failed++
        throw error
      } finally {
        const elapsed = performance.now() - started
        totalMilliseconds += elapsed; maximumMilliseconds = Math.max(maximumMilliseconds, elapsed)
      }
    },
    summary() {
      return {attempts, completed, failed, totalMilliseconds: Math.round(totalMilliseconds * 100) / 100,
        maximumMilliseconds: Math.round(maximumMilliseconds * 100) / 100, timeoutMilliseconds,
        quietMilliseconds, maximumInflightRequests: 0, lifecycle: 'domcontentloaded',
        previousQuietMilliseconds: 500, oneOverallOriginalDeadline: true,
        DOMErrorSignatureReceiptAndBalanceControlsRemoved: 0, productAPIOrRPCRepliesReplaced: 0}
    },
  }
}
