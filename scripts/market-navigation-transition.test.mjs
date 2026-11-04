// Bounded standard-library protocol controls; no browser, SDK, HTTP or wallet.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {performance} from 'node:perf_hooks'
import {pathToFileURL} from 'node:url'
import {resolve} from 'node:path'
const subject = process.argv[2] ? pathToFileURL(resolve(process.argv[2])) : new URL('./market-navigation-support.mjs', import.meta.url)
const {observeNavigationRequests, boundedNavigation} = await import(subject.href)
const origin = 'http://127.0.0.1:1'
let sequence = 1, controls = 0
const request = (id, path = '/api', type = 'Fetch', method = 'GET') => ({requestId: id, timestamp: sequence++, type,
  request: {url: origin+path, method}})
const terminal = (id, timestamp = 1e9) => ({requestId: id, timestamp})
class Session extends EventEmitter {
  detached = false
  signal(type, event) {this.emit('*', type, event)}
  async send(method, params, options) {assert.equal(method, 'Network.enable'); assert.equal(options.timeout, 5000)}
  async detach() {this.detached = true}
}
class Page extends EventEmitter {
  session = new Session()
  calls = 0
  async createCDPSession() {return this.session}
  async goto(href, options) {
    this.calls++; this.options = options
    const active = this.tracker.diagnostic().activeRequests
    if (this.regression) console.log('NAVIGATION-TRANSITION-REGRESSION '+JSON.stringify({activeBeforeNavigation: active, expectedActive: 0}))
    assert.equal(active, 0, 'known earlier requests must finish before page.goto')
    await this.beforeDocument?.()
    const id = 'document-'+this.calls
    this.session.signal('Network.requestWillBeSent', request(id, new URL(href).pathname, 'Document'))
    await this.loading?.(id)
    this.session.signal('Network.loadingFinished', terminal(id))
    return {href}
  }
}
async function fixture() {
  const page = new Page(), timers = new Set()
  page.tracker = await observeNavigationRequests(page)
  return {page, session: page.session, tracker: page.tracker, navigation: boundedNavigation(origin),
    after(milliseconds, work) {
      const timer = setTimeout(() => {timers.delete(timer); work()}, milliseconds)
      timers.add(timer)
    },
    close() {for (const timer of timers) clearTimeout(timer); timers.clear(); page.emit('close')},
  }
}
{
  const f = await fixture(); f.page.regression = true
  f.session.signal('Network.requestWillBeSent', request('earlier'))
  let ended = false
  f.after(20, () => {ended = true; f.session.signal('Network.loadingFinished', terminal('earlier'))})
  f.page.beforeDocument = () => assert.equal(ended, true)
  try {
    await f.navigation.goto(f.page, origin+'/document')
    assert.equal(f.page.calls, 1); assert.equal(f.navigation.summary().networkRequestsCompleted, 2)
  } finally {f.close()}
  controls++
}
{
  const f = await fixture()
  for (const [id, type] of [['first', 'Fetch'], ['second', 'XHR']]) f.session.signal('Network.requestWillBeSent', request(id, '/'+id, type))
  f.after(10, () => f.session.signal('Network.loadingFinished', terminal('first')))
  f.after(40, () => {assert.equal(f.page.calls, 0); f.session.signal('Network.loadingFinished', terminal('second'))})
  try {await f.navigation.goto(f.page, origin+'/document'); assert.equal(f.navigation.summary().networkRequestsCompleted, 3)} finally {f.close()}
  controls++
}
{
  const f = await fixture(), event = request('body')
  f.session.signal('Network.requestWillBeSent', event)
  f.session.signal('Network.responseReceived', {...terminal('body'), response: {url: event.request.url, status: 200}})
  f.session.signal('Network.responseReceivedExtraInfo', {requestId: 'body', statusCode: 200})
  f.session.signal('Network.requestServedFromCache', {requestId: 'body'})
  assert.equal(f.tracker.diagnostic().activeRequests, 1)
  f.after(30, () => {assert.equal(f.page.calls, 0); assert.equal(f.tracker.diagnostic().activeRequests, 1)})
  f.after(60, () => f.session.signal('Network.loadingFinished', terminal('body')))
  try {await f.navigation.goto(f.page, origin+'/document'); assert.equal(f.tracker.diagnostic().cacheSignals, 1)} finally {f.close()}
  controls++
}
{
  const f = await fixture()
  f.session.signal('Network.requestWillBeSent', request('failed'))
  f.after(20, () => f.session.signal('Network.loadingFailed', terminal('failed')))
  try {await f.navigation.goto(f.page, origin+'/document'); assert.equal(f.tracker.diagnostic().completed, 2)} finally {f.close()}
  controls++
}
{
  const f = await fixture(), first = request('redirect', '/before'), next = request('redirect', '/after')
  f.session.signal('Network.requestWillBeSent', first)
  f.session.signal('Network.requestWillBeSent', {...next, redirectResponse: {url: first.request.url}})
  f.after(10, () => {
    f.session.signal('Network.loadingFinished', terminal('redirect', first.timestamp))
    assert.equal(f.tracker.diagnostic().activeRequests, 1); assert.equal(f.page.calls, 0)
  })
  f.after(40, () => f.session.signal('Network.loadingFinished', terminal('redirect')))
  try {
    await f.navigation.goto(f.page, origin+'/document')
    assert.equal(f.tracker.diagnostic().completed, 3); assert.equal(f.tracker.diagnostic().unmatchedCompletionSignals, 1)
  } finally {f.close()}
  controls++
}
{
  const f = await fixture()
  f.session.signal('Network.requestWillBeSent', request('closed'))
  f.after(10, () => f.close())
  try {
    await assert.rejects(f.navigation.goto(f.page, origin+'/document'), /page closed before network quiet/)
    assert.equal(f.page.calls, 0); assert.equal(f.navigation.summary().failed, 1)
  } finally {f.close()}
  controls++
}
{
  const f = await fixture()
  f.session.signal('Network.requestWillBeSent', request('conflict', '/first'))
  f.session.signal('Network.requestWillBeSent', request('conflict', '/second'))
  try {
    await assert.rejects(f.navigation.goto(f.page, origin+'/document'), /identity reused while active/)
    assert.equal(f.page.calls, 0)
  } finally {f.close()}
  controls++
}
{
  // The source reads the same standard-library clock object. Only this pure
  // control advances it, avoiding a twenty-second real wait or a runtime option.
  const descriptor = Object.getOwnPropertyDescriptor(performance, 'now')
  let now = 1000, f
  Object.defineProperty(performance, 'now', {configurable: true, value: () => now})
  try {
    f = await fixture()
    f.session.signal('Network.requestWillBeSent', request('budget'))
    f.after(1, () => {
      now = 8000; f.session.signal('Network.loadingFinished', terminal('budget'))
      f.after(1, () => {now = 8100})
    })
    f.page.loading = async () => {
      assert.equal(f.page.options.timeout, 12900, 'pre-transition time must consume the original deadline')
      assert.equal(f.page.options.waitUntil, 'domcontentloaded')
      now = 21001
    }
    await assert.rejects(f.navigation.goto(f.page, origin+'/document'), /original navigation deadline elapsed/)
    assert.equal(f.page.calls, 1); assert.equal(f.navigation.summary().completed, 0); assert.equal(f.navigation.summary().failed, 1)
  } finally {
    f?.close()
    if (descriptor) Object.defineProperty(performance, 'now', descriptor); else delete performance.now
  }
  controls++
}
{
  const a = await fixture(), b = await fixture()
  a.session.signal('Network.requestWillBeSent', request('other-page'))
  try {
    await b.navigation.goto(b.page, origin+'/document')
    assert.equal(a.tracker.diagnostic().activeRequests, 1); assert.equal(b.navigation.summary().completed, 1)
  } finally {a.close(); b.close()}
  controls++
}
assert.equal(controls, 9)
console.log('NAVIGATION-TRANSITION-CONTROLS '+JSON.stringify({controls, pass: true, SDKOrBrowserExecutions: 0, networkRequests: 0,
  knownEarlierRequestsMustTerminateBeforeNavigation: true, preAndPostQuietRequired: true,
  overallNavigationDeadlineMilliseconds: 20000, quietMilliseconds: 100, maximumInflightRequests: 0,
  requestTypesFiltered: 0, originalTerminalIdentityAndTimestampGuardsPreserved: true,
  preTransitionTimeConsumesOriginalDeadline: true, newRuntimeTimeoutOrClockOption: false,
  fakeClockUsedOnlyInSharedDeadlineControl: true, historicalTimeoutRootCauseEstablished: false}))
