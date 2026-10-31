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
  assert.ok(!message.includes('fixture-secret') && !message.includes('?private='))
  const secret = 'fixture-header-and-payload-secret'
  session.signal('Network.responseReceivedExtraInfo', {requestId: 'one', statusCode: 200,
    headers: {Authorization: secret}, rawHeaders: secret, blockedCookies: [{cookieLine: secret}]})
  session.signal('Network.loadingFinished', terminal('one', 0))
  session.signal('Network.responseReceived', {...terminal('one'), response: {url: 'http://127.0.0.1:1/other?private='+secret, status: 200}})
  assert.equal(tracker.diagnostic().activeRequests, 1)
  let signals = tracker.diagnostic().requests[0].recentSignals
  assert.deepEqual(signals.map(signal => signal.event), ['responseReceivedExtraInfo', 'loadingFinished', 'responseReceived'])
  assert.equal(signals[0].timestampPresent, false); assert.equal(signals[0].status, 200)
  assert.equal(signals[1].timestampBeforeRequest, true); assert.equal(signals[2].responseURLMatchesRequest, false)
  for (let i=0; i<20; i++) session.signal('Network.requestWillBeSentExtraInfo', {requestId: 'one', headers: {Authorization: secret}})
  signals = tracker.diagnostic().requests[0].recentSignals
  assert.equal(signals.length, 8); assert.ok(signals.every(signal => signal.event === 'requestWillBeSentExtraInfo'))
  assert.ok(!JSON.stringify(tracker.diagnostic()).includes(secret) && !JSON.stringify(tracker.diagnostic()).includes('Authorization'))
  signals[0].event = secret
  assert.ok(!JSON.stringify(tracker.diagnostic()).includes(secret), 'diagnostic snapshots must not alias retained state')
  assert.equal(tracker.diagnostic().activeRequests, 1); controls++
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

// Independent page signals never satisfy the original protocol-session gate.
const passiveRequest = (url = 'http://127.0.0.1:1/api?private=fixture-secret') => ({
  url: () => url, method: () => 'POST', isInterceptResolutionHandled: () => true,
  failure: () => ({errorText: 'net::ERR_ABORTED'})})
{
  const page = new Page(), tracker = await observeNavigationRequests(page), req = passiveRequest()
  page.session.signal('Network.requestWillBeSent', request())
  page.emit('request', req)
  page.emit('response', {request: () => req, status: () => 200})
  page.emit('requestfinished', req)
  let value = tracker.diagnostic()
  // The path differs, so no correlation may be invented.
  assert.equal(value.requests[0].pageLifecycleMatches.recentMatches, 0)
  assert.equal(value.pageLifecycle.finished, 1)
  await assert.rejects(tracker.wait(performance.now()+15), /original navigation deadline elapsed/)
  const req2 = passiveRequest('http://127.0.0.1:1/rpc?private=fixture-secret')
  page.emit('request', req2); page.emit('response', {request: () => req2, status: () => 200}); page.emit('requestfinished', req2)
  value = tracker.diagnostic()
  const match = value.requests[0].pageLifecycleMatches
  assert.equal(match.recentMatches, 1); assert.equal(match.activeMatches, 0)
  assert.equal(match.protocolIdentityEstablished, false)
  assert.deepEqual(match.observations[0].signals, ['request', 'response', 'requestfinished'])
  assert.equal(match.observations[0].responseStatus, 200)
  assert.equal(value.activeRequests, 1)
  await assert.rejects(tracker.wait(performance.now()+15), /original navigation deadline elapsed/)
  assert.ok(!JSON.stringify(value).includes('fixture-secret'))
  page.emit('close'); controls++
}
{
  const page = new Page(), tracker = await observeNavigationRequests(page), req = passiveRequest()
  page.session.signal('Network.requestWillBeSent', request())
  page.emit('response', {request: () => req, status: () => 200})
  page.session.signal('Network.responseReceivedExtraInfo', {requestId: 'one', statusCode: 200})
  const value = tracker.diagnostic()
  assert.equal(value.pageLifecycle.unmatchedSignals, 1)
  assert.equal(value.pageLifecycle.started, 0)
  assert.equal(value.requests[0].pageLifecycleMatches.observations.length, 0)
  assert.equal(value.activeRequests, 1)
  await assert.rejects(tracker.wait(performance.now()+15), /original navigation deadline elapsed/)
  page.emit('close'); controls++
}
{
  const page = new Page(), tracker = await observeNavigationRequests(page)
  const first = passiveRequest('http://127.0.0.1:1/rpc?private=fixture-secret'), second = passiveRequest(first.url())
  page.session.signal('Network.requestWillBeSent', request())
  page.emit('request', first); page.emit('request', second); page.emit('requestfailed', first)
  const value = tracker.diagnostic(), match = value.requests[0].pageLifecycleMatches
  assert.equal(match.activeMatches, 1); assert.equal(match.recentMatches, 1)
  assert.equal(match.protocolIdentityEstablished, false)
  assert.equal(value.pageLifecycle.failed, 1)
  assert.ok(match.observations.some(entry => entry.terminal === 'requestfailed' && entry.failureClass === 'net::ERR_ABORTED'))
  assert.equal(value.activeRequests, 1)
  page.emit('close'); controls++
}
{
  const page = new Page(), tracker = await observeNavigationRequests(page)
  const requests = Array.from({length: 257}, (_, i) => passiveRequest('http://127.0.0.1:1/api/'+i))
  for (const req of requests) page.emit('request', req)
  assert.equal(tracker.diagnostic().pageLifecycle.active, 256)
  assert.equal(tracker.diagnostic().pageLifecycle.truncated, true)
  for (const req of requests) page.emit('requestfinished', req)
  const value = tracker.diagnostic()
  assert.equal(value.pageLifecycle.active, 0); assert.equal(value.pageLifecycle.recent, 8)
  assert.equal(value.pageLifecycle.unmatchedSignals, 1)
  assert.equal(value.pageLifecycle.completionGateChanged, false)
  await tracker.wait(performance.now()+500)
  page.emit('close'); controls++
}
{
  const page = new Page(), tracker = await observeNavigationRequests(page)
  page.emit('request', {url: () => {throw new Error('fixture-header-and-payload-secret')}})
  assert.equal(tracker.diagnostic().pageLifecycle.readErrors, 1)
  const req = passiveRequest('http://127.0.0.1:1/rpc?private=fixture-secret')
  page.session.signal('Network.requestWillBeSent', request())
  page.emit('request', req); page.emit('requestfinished', req)
  const snapshot = tracker.diagnostic()
  snapshot.requests[0].pageLifecycleMatches.observations[0].signals[0] = 'mutated'
  assert.equal(tracker.diagnostic().requests[0].pageLifecycleMatches.observations[0].signals[0], 'request')
  assert.ok(!JSON.stringify(tracker.diagnostic()).includes('fixture-header-and-payload-secret'))
  page.emit('close')
  for (const name of ['request', 'response', 'requestfinished', 'requestfailed']) assert.equal(page.listenerCount(name), 0)
  assert.equal(page.session.listenerCount('*'), 0); controls++
}

assert.equal(controls, 21)
console.log('NAVIGATION-IDENTITY-CONTROLS '+JSON.stringify({controls, originalProtocolControls: 16, passivePageObserverControls: 5, pass: true, SDKOrBrowserExecutions: 0,
  overallNavigationDeadlineMilliseconds: 20000, quietMilliseconds: 100, maximumInflightRequests: 0,
  requestTracking: 'cdp-network-request-identities', originalBudgetPreserved: true,
  protocolSessionsIsolated: true, interceptionOnlySignalsCountedAsNetworkRequests: false,
  cacheProvenanceAloneCompletesRequest: false, diagnosticQueryValuesOmitted: true}))

import './market-request-identity-support.test.mjs'

import './market-page-client-support.test.mjs'
