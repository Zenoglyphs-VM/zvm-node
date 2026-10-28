// Pure event controls use Node standard libraries and no browser or SDK.
import assert from 'node:assert/strict'
import {performance} from 'node:perf_hooks'
import {EventEmitter} from 'node:events'
import {createNavigationRequestTracker, observeNavigationRequests, boundedNavigation} from './market-navigation-support.mjs'

class Session extends EventEmitter {
  detached = false
  signal(type, value) {this.emit('*', type, value)}
  async send(method, params, options) {this.options = options; assert.equal(method, 'Network.enable'); await this.setup?.()}
  async detach() {this.detached = true}
}
class Page extends EventEmitter {
  session = new Session()
  async createCDPSession() {return this.session}
  async goto(href, options) {
    this.options = options
    const id = 'document-'+String(this.sequence = (this.sequence ?? 0)+1)
    this.session.signal('Network.requestWillBeSent', request(id, new URL(href).pathname, 'Document'))
    await this.loading?.()
    this.session.signal('Network.loadingFinished', terminal(id))
    return {href}
  }
}
let timestamp = 1
const request = (requestId = 'one', path = '/rpc?private=fixture-secret', type = 'Fetch') => ({requestId, timestamp: timestamp++, type,
  request: {url: 'http://127.0.0.1:1'+path, method: 'POST'}})
const terminal = (requestId = 'one', time = 1e9) => ({requestId, timestamp: time})
const delay = ms => new Promise(resolve => setTimeout(resolve, ms))
const fixture = () => {const session = new Session(); return {session, tracker: createNavigationRequestTracker(session)}}
let controls = 0

{
  const {session, tracker} = fixture(), first = request('one'), second = request('two')
  session.signal('Network.requestWillBeSent', first); session.signal('Network.requestWillBeSent', first)
  session.signal('Network.requestWillBeSent', second); session.signal('Network.responseReceived', terminal('one'))
  session.signal('Network.loadingFinished', terminal('one'))
  assert.equal(tracker.diagnostic().activeRequests, 1)
  await assert.rejects(tracker.wait(performance.now()+15), /original navigation deadline elapsed/)
  session.signal('Network.loadingFailed', terminal('two')); await tracker.wait(performance.now()+500)
  assert.equal(tracker.diagnostic().completed, 2); controls++
}
{
  const {session, tracker} = fixture(); session.signal('Network.requestWillBeSent', request())
  let message = ''
  try {await tracker.wait(performance.now()+15)} catch (error) {message = error.message}
  assert.match(message, /activeRequests":1/); assert.match(message, /"path":"\/rpc"/)
  assert.ok(!message.includes('fixture-secret') && !message.includes('?private=')); controls++
}
{
  const {session, tracker} = fixture(), began = performance.now(), waiting = tracker.wait(began+700)
  setTimeout(() => session.signal('Network.requestWillBeSent', request()), 40)
  setTimeout(() => session.signal('Network.loadingFinished', terminal()), 70)
  await waiting; assert.ok(performance.now()-began >= 165); controls++
}
{
  const {session, tracker} = fixture(); session.signal('Network.requestWillBeSent', request())
  session.signal('Network.loadingFinished', terminal())
  const began = performance.now(), noise = setInterval(() => session.signal('Network.loadingFinished', terminal()), 10)
  try {await tracker.wait(began+500)} finally {clearInterval(noise)}
  assert.ok(performance.now()-began >= 95 && tracker.diagnostic().unmatchedCompletionSignals > 0); controls++
}
{
  const a = fixture(), b = fixture(); a.session.signal('Network.requestWillBeSent', request())
  await b.tracker.wait(performance.now()+500); assert.equal(a.tracker.diagnostic().activeRequests, 1)
  const waiting = a.tracker.wait(performance.now()+500); a.tracker.close()
  await assert.rejects(waiting, /page closed/); controls++
}
{
  const {session, tracker} = fixture()
  for (let i=0; i<257; i++) session.signal('Network.requestWillBeSent', request(String(i)))
  assert.equal(tracker.diagnostic().activeRequests, 256)
  await assert.rejects(tracker.wait(performance.now()+500), /tracking limit exceeded/); controls++
}
{
  const page = new Page(), navigation = boundedNavigation('http://127.0.0.1:1')
  page.session.setup = () => delay(20); page.loading = () => delay(25)
  const began = performance.now(); await navigation.goto(page, 'http://127.0.0.1:1/account/test')
  assert.ok(performance.now()-began >= 140)
  assert.equal(page.options.waitUntil, 'domcontentloaded'); assert.ok(page.options.timeout <= 19990 && page.options.timeout > 19000)
  const value = navigation.summary(); assert.equal(value.completed, 1); assert.equal(value.failed, 0)
  assert.equal(value.quietMilliseconds, 100); assert.equal(value.maximumInflightRequests, 0)
  await assert.rejects(navigation.goto(page, 'https://example.invalid/'), /Expected values/); controls++
}
{
  const {tracker} = fixture(), began = performance.now(); await delay(25)
  await assert.rejects(tracker.wait(began+40), /original navigation deadline elapsed/)
  assert.ok(performance.now()-began < 400); controls++
}
{
  const {session, tracker} = fixture(), first = request('redirect', '/before'), next = request('redirect', '/after')
  session.signal('Network.requestWillBeSent', first)
  session.signal('Network.requestWillBeSent', {...next, redirectResponse: {url: first.request.url}})
  session.signal('Network.loadingFinished', terminal('redirect', first.timestamp))
  session.signal('Network.responseReceived', {...terminal('redirect'), response: {url: first.request.url}})
  assert.equal(tracker.diagnostic().activeRequests, 1)
  session.signal('Network.loadingFinished', terminal('redirect')); await tracker.wait(performance.now()+500)
  assert.equal(tracker.diagnostic().completed, 2); controls++
}
{
  const page = new Page(), navigation = boundedNavigation('http://127.0.0.1:1'); page.goto = async () => ({})
  await assert.rejects(navigation.goto(page, 'http://127.0.0.1:1/document'), /document request events were not observed/)
  assert.equal(navigation.summary().failed, 1); controls++
}
{
  const {session, tracker} = fixture(); session.signal('Network.requestWillBeSent', request('cached', '/asset.js', 'Script'))
  session.signal('Network.requestServedFromCache', {requestId: 'cached'}); assert.equal(tracker.diagnostic().activeRequests, 1)
  session.signal('Network.responseReceived', terminal('cached')); await tracker.wait(performance.now()+500)
  assert.equal(tracker.diagnostic().completed, 1); assert.equal(tracker.diagnostic().cacheSignals, 1); controls++
}
{
  const {session, tracker} = fixture(); session.signal('Network.requestWillBeSent', request())
  session.signal('Network.requestServedFromCache', {requestId: 'other'}); assert.equal(tracker.diagnostic().activeRequests, 1)
  await assert.rejects(tracker.wait(performance.now()+15), /original navigation deadline elapsed/); controls++
}
{
  const page = new Page(), tracker = await observeNavigationRequests(page)
  page.emit('request', {id: 'interception-only', url: () => 'http://127.0.0.1:1/rpc'})
  await tracker.wait(performance.now()+500); assert.equal(tracker.diagnostic().started, 0)
  page.emit('close'); assert.ok(page.session.detached); controls++
}
{
  const {session, tracker} = fixture()
  for (const type of ['Document', 'Script', 'Image', 'Fetch', 'XHR', 'Other']) session.signal('Network.requestWillBeSent', request(type, '/'+type, type))
  assert.equal(tracker.diagnostic().activeRequests, 6)
  for (const type of ['Document', 'Script', 'Image', 'Fetch', 'XHR', 'Other']) session.signal('Network.loadingFinished', terminal(type))
  await tracker.wait(performance.now()+500); assert.equal(tracker.diagnostic().completed, 6); controls++
}
{
  const {session, tracker} = fixture(); session.signal('Network.requestWillBeSent', request('conflict', '/first'))
  session.signal('Network.requestWillBeSent', request('conflict', '/different'))
  await assert.rejects(tracker.wait(performance.now()+500), /identity reused while active/); controls++
}
{
  const page = new Page(); page.session.setup = () => {throw new Error('fixture setup refusal')}
  await assert.rejects(observeNavigationRequests(page), /fixture setup refusal/)
  assert.ok(page.session.detached); assert.equal(page.session.listenerCount('*'), 0); controls++
}
assert.equal(controls, 16)
console.log('NAVIGATION-IDENTITY-CONTROLS '+JSON.stringify({controls, pass: true, SDKOrBrowserExecutions: 0,
  overallNavigationDeadlineMilliseconds: 20000, quietMilliseconds: 100, maximumInflightRequests: 0,
  requestTracking: 'cdp-network-request-identities', originalBudgetPreserved: true,
  protocolSessionsIsolated: true, interceptionOnlySignalsCountedAsNetworkRequests: false,
  cacheProvenanceAloneCompletesRequest: false, diagnosticQueryValuesOmitted: true}))
