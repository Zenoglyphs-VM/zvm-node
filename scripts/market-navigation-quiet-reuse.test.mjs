// Standard-library fake-clock controls; no browser, SDK, HTTP or wallet.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {performance} from 'node:perf_hooks'
import {pathToFileURL} from 'node:url'
import {resolve} from 'node:path'
const subject = process.argv[2] ? pathToFileURL(resolve(process.argv[2])) : new URL('./market-navigation-support.mjs', import.meta.url)
const {observeNavigationRequests, boundedNavigation} = await import(subject.href)
const origin = 'http://127.0.0.1:1'
let sequence = 1, controls = 0
const request = (id, path = '/api') => ({requestId: id, timestamp: sequence++, type: 'Fetch', request: {url: origin+path, method: 'GET'}})
const terminal = (id, timestamp = 1e9) => ({requestId: id, timestamp})
const delay = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds))
async function clock(work) {
  const descriptor = Object.getOwnPropertyDescriptor(performance, 'now')
  let now = 1000
  Object.defineProperty(performance, 'now', {configurable: true, value: () => now})
  try {await work(value => {assert.ok(value >= now); now = value})}
  finally {if (descriptor) Object.defineProperty(performance, 'now', descriptor); else delete performance.now}
}
class Session extends EventEmitter {
  detached = false
  signal(type, value) {this.emit('*', type, value)}
  async send(method, params, options) {assert.equal(method, 'Network.enable'); assert.equal(options.timeout, 5000)}
  async detach() {this.detached = true}
}
class Page extends EventEmitter {
  session = new Session()
  calls = 0
  documentStarted = false
  async createCDPSession() {return this.session}
  async goto(href, options) {
    this.calls++; this.options = options
    assert.equal(this.tracker.diagnostic().activeRequests, 0, 'known earlier loads must finish before navigation')
    this.documentStarted = true
    const id = 'document-'+this.calls
    this.session.signal('Network.requestWillBeSent', {...request(id, new URL(href).pathname), type: 'Document'})
    await this.loading?.()
    this.session.signal('Network.loadingFinished', terminal(id))
    return {href}
  }
}
async function fixture() {
  const page = new Page()
  page.tracker = await observeNavigationRequests(page)
  return {page, session: page.session, tracker: page.tracker, navigation: boundedNavigation(origin), close() {page.emit('close')}}
}
await clock(async set => {
  const f = await fixture(), originalTimeout = globalThis.setTimeout
  let settled = false, pending, preQuietTimers = 0
  set(1200)
  globalThis.setTimeout = (callback, milliseconds, ...args) => {
    if (f.page.calls === 0 && milliseconds === 100) preQuietTimers++
    return originalTimeout(callback, milliseconds, ...args)
  }
  try {
    pending = f.navigation.goto(f.page, origin+'/document')
    pending.then(() => {settled = true}, () => {})
    await delay(5)
    console.log('NAVIGATION-QUIET-REUSE-REGRESSION '+JSON.stringify({quietMillisecondsAlreadyObserved: 200,
      documentStartedBeforeClockAdvance: f.page.documentStarted, preQuietTimers, expectedPreQuietTimers: 0}))
    assert.equal(f.page.documentStarted, true, 'an already observed complete quiet interval must not restart before navigation')
    assert.equal(preQuietTimers, 0)
    assert.equal(settled, false, 'post-navigation quiet must still be fresh')
    set(1300); await pending
    assert.equal(f.navigation.summary().completed, 1)
    assert.equal(f.page.options.waitUntil, 'domcontentloaded'); assert.equal(f.page.options.timeout, 20000)
  } finally {globalThis.setTimeout = originalTimeout; f.close()}
}); controls++
await clock(async set => {
  const f = await fixture(); set(1200)
  let settled = false
  const pending = f.tracker.wait(2000).then(() => {settled = true})
  try {
    await delay(5); assert.equal(settled, false, 'the original wait API must start a fresh quiet interval')
    set(1300); await pending
  } finally {f.close()}
}); controls++
await clock(async set => {
  const f = await fixture(), originalTimeout = globalThis.setTimeout, scheduled = []
  set(1050)
  globalThis.setTimeout = (callback, milliseconds, ...args) => {scheduled.push(milliseconds); return originalTimeout(callback, milliseconds, ...args)}
  let pending, settled = false
  try {pending = f.tracker.beforeNavigation(1500).then(() => {settled = true})}
  finally {globalThis.setTimeout = originalTimeout}
  try {
    assert.deepEqual(scheduled, [50], 'partial observed quiet must wait exactly its remaining interval')
    await delay(5); assert.equal(settled, false)
    set(1100); await pending
  } finally {f.close()}
}); controls++
await clock(async set => {
  const f = await fixture(); set(1200)
  f.session.signal('Network.requestWillBeSent', request('new-activity'))
  let settled = false
  const pending = f.tracker.beforeNavigation(2200).then(() => {settled = true})
  try {
    await delay(5); assert.equal(settled, false)
    set(1300); f.session.signal('Network.loadingFinished', terminal('new-activity'))
    await delay(5); assert.equal(settled, false, 'new activity must invalidate earlier quiet')
    set(1400); await pending; assert.equal(f.tracker.diagnostic().completed, 1)
  } finally {f.close()}
}); controls++
await clock(async set => {
  const f = await fixture(); set(1200)
  const event = request('body')
  f.session.signal('Network.requestWillBeSent', event)
  f.session.signal('Network.responseReceived', {...terminal('body'), response: {url: event.request.url, status: 200}})
  f.session.signal('Network.responseReceivedExtraInfo', {requestId: 'body', statusCode: 200})
  f.session.signal('Network.requestServedFromCache', {requestId: 'body'})
  f.session.signal('Network.loadingFinished', terminal('body', 0))
  let settled = false
  const pending = f.tracker.beforeNavigation(2200).then(() => {settled = true})
  try {
    await delay(5); assert.equal(settled, false); assert.equal(f.tracker.diagnostic().activeRequests, 1)
    set(1300); f.session.signal('Network.loadingFinished', terminal('body'))
    await delay(5); assert.equal(settled, false)
    set(1400); await pending
    assert.equal(f.tracker.diagnostic().completed, 1); assert.equal(f.tracker.diagnostic().cacheSignals, 1)
    assert.equal(f.tracker.diagnostic().unmatchedCompletionSignals, 1)
  } finally {f.close()}
}); controls++
for (const mode of ['closed', 'detached', 'broken', 'expired']) {
  await clock(async set => {
    const f = await fixture(); set(1200)
    if (mode === 'closed') f.close()
    if (mode === 'detached') f.session.detached = true
    if (mode === 'broken') f.session.signal('Network.requestWillBeSent', {...request('invalid'), requestId: ''})
    const reason = mode === 'broken' ? /invalid navigation protocol request identity/ : mode === 'expired' ? /original navigation deadline elapsed/ : /page closed before network quiet/
    try {
      await assert.rejects(f.tracker.beforeNavigation(mode === 'expired' ? 1200 : 2200), reason)
      assert.equal(f.page.calls, 0)
    } finally {f.close()}
  }); controls++
}
await clock(async set => {
  const a = await fixture(), b = await fixture(); set(1200)
  a.session.signal('Network.requestWillBeSent', request('other-page'))
  const pending = b.navigation.goto(b.page, origin+'/document'); pending.catch(() => {})
  try {
    await delay(5); assert.equal(b.page.documentStarted, true); assert.equal(a.tracker.diagnostic().activeRequests, 1)
    set(1300); await pending; assert.equal(b.navigation.summary().completed, 1)
  } finally {a.close(); b.close()}
}); controls++
await clock(async set => {
  const f = await fixture(); set(1200)
  f.page.loading = async () => {assert.equal(f.page.options.timeout, 20000); set(21201)}
  try {
    await assert.rejects(f.navigation.goto(f.page, origin+'/document'), /original navigation deadline elapsed/)
    assert.equal(f.page.calls, 1); assert.equal(f.navigation.summary().completed, 0); assert.equal(f.navigation.summary().failed, 1)
  } finally {f.close()}
}); controls++
assert.equal(controls, 11)
console.log('NAVIGATION-QUIET-REUSE-CONTROLS '+JSON.stringify({controls, pass: true, SDKOrBrowserExecutions: 0, networkRequests: 0,
  alreadyObservedPreQuietCanBeReused: true, completeQuietPreTimerAllocations: 0, partialQuietWaitsOnlyRemainder: true,
  originalWaitAndPostNavigationRemainFresh: true, newActivityInvalidatesPriorQuiet: true,
  closedDetachedBrokenAndExpiredStillRefused: true, originalTerminalIdentityTimestampAndPageIsolationPreserved: true,
  overallNavigationDeadlineMilliseconds: 20000, quietMilliseconds: 100, maximumInflightRequests: 0, requestTypesFiltered: 0,
  fakeClockAndTimerObservationOnlyInPureControls: true, newRuntimeClockOrTimeoutOption: false,
  actualBrowserElapsedSpeedupOrHistoricalTimeoutCauseEstablished: false}))
