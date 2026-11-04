// Standard-library fake protocol controls; no browser, SDK, HTTP or wallet.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {performance} from 'node:perf_hooks'
import {pathToFileURL} from 'node:url'
import {resolve} from 'node:path'
const subject = process.argv[2] ? pathToFileURL(resolve(process.argv[2])) : new URL('./market-navigation-support.mjs', import.meta.url)
const {createNavigationRequestTracker, boundedNavigation} = await import(subject.href)
class Session extends EventEmitter {
  detached = false
  signal(type, value) {this.emit('*', type, value)}
  async send(method) {assert.equal(method, 'Network.enable')}
  async detach() {this.detached = true}
}
let sequence = 1
const origin = 'http://127.0.0.1:1'
const request = (id = 'one', path = '/api') => ({requestId: id, timestamp: sequence++, type: 'Fetch', request: {url: origin+path, method: 'GET'}})
const terminal = (id = 'one', timestamp = 1e9) => ({requestId: id, timestamp})
const fixture = () => {const session = new Session(); return {session, tracker: createNavigationRequestTracker(session)}}
const delay = ms => new Promise(resolve => setTimeout(resolve, ms))
let controls = 0
{
  const {session, tracker} = fixture()
  session.signal('Network.requestWillBeSent', request())
  session.signal('Network.responseReceived', {...terminal(), response: {url: origin+'/api', status: 200}})
  console.log('NAVIGATION-HEADER-TERMINAL-REGRESSION '+JSON.stringify({activeAfterHeaders: tracker.diagnostic().activeRequests, expectedActive: 1}))
  assert.equal(tracker.diagnostic().activeRequests, 1, 'response headers must not complete a tracked load')
  await assert.rejects(tracker.wait(performance.now()+20), /original navigation deadline elapsed/)
  session.signal('Network.loadingFinished', terminal()); await tracker.wait(performance.now()+600)
  assert.equal(tracker.diagnostic().completed, 1); tracker.close(); controls++
}
{
  const {session, tracker} = fixture()
  session.signal('Network.requestWillBeSent', request())
  session.signal('Network.requestServedFromCache', {requestId: 'one'})
  session.signal('Network.responseReceivedExtraInfo', {requestId: 'one', statusCode: 200})
  session.signal('Network.responseReceived', {...terminal(), response: {url: origin+'/api', status: 200}})
  assert.equal(tracker.diagnostic().activeRequests, 1); assert.equal(tracker.diagnostic().completed, 0)
  session.signal('Network.loadingFinished', terminal()); await tracker.wait(performance.now()+600)
  assert.equal(tracker.diagnostic().cacheSignals, 1); tracker.close(); controls++
}
{
  const {session, tracker} = fixture()
  for (const id of ['one', 'two']) {
    session.signal('Network.requestWillBeSent', request(id))
    session.signal('Network.responseReceived', terminal(id))
  }
  session.signal('Network.loadingFinished', terminal('one'))
  assert.equal(tracker.diagnostic().activeRequests, 1)
  session.signal('Network.loadingFailed', terminal('two')); await tracker.wait(performance.now()+600)
  assert.equal(tracker.diagnostic().completed, 2); tracker.close(); controls++
}
{
  const {session, tracker} = fixture(), event = request()
  session.signal('Network.requestWillBeSent', event)
  session.signal('Network.loadingFinished', terminal('one', event.timestamp-1))
  assert.equal(tracker.diagnostic().activeRequests, 1)
  session.signal('Network.loadingFinished', terminal()); session.signal('Network.loadingFinished', terminal())
  assert.equal(tracker.diagnostic().completed, 1); assert.equal(tracker.diagnostic().unmatchedCompletionSignals, 2)
  await tracker.wait(performance.now()+600); tracker.close(); controls++
}
{
  const {session, tracker} = fixture(), first = request('redirect', '/first'), second = request('redirect', '/second')
  session.signal('Network.requestWillBeSent', first)
  session.signal('Network.requestWillBeSent', {...second, redirectResponse: {url: first.request.url}})
  session.signal('Network.loadingFinished', terminal('redirect', first.timestamp))
  session.signal('Network.responseReceived', {...terminal('redirect'), response: {url: second.request.url, status: 200}})
  assert.equal(tracker.diagnostic().activeRequests, 1)
  session.signal('Network.loadingFinished', terminal('redirect')); await tracker.wait(performance.now()+600)
  assert.equal(tracker.diagnostic().completed, 2); tracker.close(); controls++
}
{
  const {session, tracker} = fixture(); session.signal('Network.requestWillBeSent', request())
  session.signal('Network.responseReceived', terminal())
  const waiting = tracker.wait(performance.now()+600); tracker.close()
  await assert.rejects(waiting, /page closed/); controls++
}
{
  const {session, tracker} = fixture(); session.signal('Network.requestWillBeSent', request('one', '/first'))
  session.signal('Network.requestWillBeSent', request('one', '/second'))
  session.signal('Network.responseReceived', terminal()); session.signal('Network.loadingFinished', terminal())
  await assert.rejects(tracker.wait(performance.now()+600), /identity reused while active/); tracker.close(); controls++
}
{
  class Page extends EventEmitter {
    session = new Session()
    async createCDPSession() {return this.session}
    async goto(href, options) {
      this.options = options
      const event = request('document', '/document'); event.type = 'Document'
      this.session.signal('Network.requestWillBeSent', event)
      this.session.signal('Network.responseReceived', {...terminal('document'), response: {url: event.request.url, status: 200}})
      this.body = setTimeout(() => {this.bodyEnded = performance.now(); this.session.signal('Network.loadingFinished', terminal('document'))}, 200)
      return {href}
    }
  }
  const page = new Page(), navigation = boundedNavigation(origin), began = performance.now()
  let settled = false
  const result = navigation.goto(page, origin+'/document').then(value => {settled = true; return value})
  try {
    await delay(150); assert.equal(settled, false, 'navigation must not return during an unfinished response body')
    assert.deepEqual(await result, {href: origin+'/document'})
    assert.ok(page.bodyEnded >= began && performance.now()-page.bodyEnded >= 90)
    const summary = navigation.summary()
    assert.equal(summary.completed, 1); assert.equal(summary.failed, 0)
    assert.equal(summary.networkRequestsStarted, 1); assert.equal(summary.networkRequestsCompleted, 1)
    assert.equal(summary.timeoutMilliseconds, 20000); assert.equal(summary.quietMilliseconds, 100)
    assert.equal(summary.maximumInflightRequests, 0); assert.equal(summary.networkRequestTypesFiltered, 0)
    assert.equal(page.options.waitUntil, 'domcontentloaded')
  } finally {clearTimeout(page.body); page.emit('close')}
  controls++
}
assert.equal(controls, 8)
console.log('NAVIGATION-TERMINAL-LOAD-CONTROLS '+JSON.stringify({controls, pass: true, SDKOrBrowserExecutions: 0,
  networkRequests: 0, responseHeadersCompleteRequest: false, cacheOrExtraInfoCompletesRequest: false,
  matchingLoadingTerminalRequired: true, overallNavigationDeadlineMilliseconds: 20000, quietMilliseconds: 100,
  maximumInflightRequests: 0, requestTypesFiltered: 0, protocolIdentityAndTimestampGuardsPreserved: true}))
