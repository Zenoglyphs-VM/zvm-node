// Standard-library fake controls; no SDK, browser, body or network access.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {performance} from 'node:perf_hooks'
import {readPageRequestIdentity, pageRequestIdMatches, maximumOpaqueRequestIdLength,
  reviewedPuppeteerSourceVersion} from './market-request-identity-support.mjs'
import {createNavigationRequestTracker} from './market-navigation-support.mjs'

const names = []
const control = async (name, run) => {assert.ok(!names.includes(name)); await run(); names.push(name)}
const page = {}
const request = (id = 'fixture-opaque-id', owner = page) => ({id, frame: () => ({page: () => owner})})
const entry = (id = 'fixture-opaque-id', overrides = {}) => ({url: 'http://127.0.0.1:1/rpc?private=fixture-query', method: 'POST',
  requestIdentity: readPageRequestIdentity(request(id), page), signals: ['request'], responseStatus: null,
  interceptResolutionHandled: true, terminal: null, failureClass: null, ...overrides})
const matched = (active, recent = new Map(), id = 'fixture-opaque-id') =>
  pageRequestIdMatches(active, recent, id, 'http://127.0.0.1:1/rpc?private=fixture-query', 'POST')

await control('same-page-opaque-id', () => {
  assert.deepEqual(readPageRequestIdentity(request(), page), {status: 'AVAILABLE', reason: null, requestId: 'fixture-opaque-id'})
})
await control('missing-id', () => {
  assert.equal(readPageRequestIdentity({frame: request().frame}, page).status, 'UNAVAILABLE')
})
await control('non-string-id', () => {
  assert.equal(readPageRequestIdentity(request(7), page).status, 'UNAVAILABLE')
})
await control('bounded-id', () => {
  assert.equal(readPageRequestIdentity(request('a'.repeat(maximumOpaqueRequestIdLength)), page).status, 'AVAILABLE')
  assert.equal(readPageRequestIdentity(request('a'.repeat(maximumOpaqueRequestIdLength + 1)), page).status, 'UNAVAILABLE')
})
await control('control-character-id', () => {
  assert.equal(readPageRequestIdentity(request('private\nidentifier'), page).status, 'UNAVAILABLE')
})
await control('throwing-id-getter', () => {
  assert.deepEqual(readPageRequestIdentity({get id() {throw new Error('fixture-private-getter')}}, page),
    {status: 'UNAVAILABLE', reason: 'READ_UNAVAILABLE', requestId: null})
})
await control('missing-frame', () => {
  assert.equal(readPageRequestIdentity({id: 'fixture-opaque-id'}, page).reason, 'FRAME_UNAVAILABLE')
  assert.equal(readPageRequestIdentity({id: 'fixture-opaque-id', frame: () => null}, page).reason, 'FRAME_UNAVAILABLE')
})
await control('different-page', () => {
  assert.equal(readPageRequestIdentity(request('fixture-opaque-id', {}), page).reason, 'PAGE_MISMATCH')
})
await control('throwing-frame-page', () => {
  assert.equal(readPageRequestIdentity({id: 'fixture-opaque-id', frame: () => ({page() {throw new Error('fixture-private-page')}})}, page).reason, 'READ_UNAVAILABLE')
})
await control('no-private-client-body-or-header-reads', () => {
  const value = request()
  for (const key of ['client', 'headers', 'postData', 'fetchPostData', 'content', '_requestId', '_interceptionId'])
    Object.defineProperty(value, key, {get() {throw new Error('private request data accessed')}})
  assert.equal(readPageRequestIdentity(value, page).status, 'AVAILABLE')
})
await control('active-and-recent-scoped-observations', () => {
  const a = new Map([[{}, entry()]]), b = new Map([[{}, entry('ended', {terminal: 'requestfinished'})]])
  const value = matched(a, b)
  assert.equal(value.status, 'MATCHED'); assert.equal(value.activeMatches, 1); assert.equal(value.recentMatches, 0)
  assert.equal(matched(a, b, 'ended').recentMatches, 1)
  assert.equal(value.exactProtocolSessionIdentityEstablished, false)
})
await control('url-method-and-id-mismatch', () => {
  for (const value of [entry('other-id'), entry(undefined, {url: 'http://127.0.0.1:1/other'}), entry(undefined, {method: 'GET'})])
    assert.equal(matched(new Map([[{}, value]])).status, 'NO_MATCH')
  assert.equal(matched(new Map(), new Map(), '').status, 'UNAVAILABLE')
})
await control('ambiguous-and-bounded-output', () => {
  const values = new Map(Array.from({length: 9}, () => [{}, entry()]))
  const value = matched(values)
  assert.equal(value.status, 'AMBIGUOUS'); assert.equal(value.activeMatches, 9)
  assert.equal(value.observations.length, 8); assert.equal(value.observationsTruncated, true)
})
await control('redaction-and-independent-snapshots', () => {
  const values = new Map([[{}, entry('fixture-opaque-id', {signals: ['request', 'fixture-header-secret'], headers: 'fixture-header-secret', body: 'fixture-body-secret'})]])
  const snapshot = matched(values), text = JSON.stringify(snapshot)
  for (const secret of ['fixture-opaque-id', 'fixture-query', 'fixture-header-secret', 'fixture-body-secret', '/rpc', 'headers', 'body'])
    assert.ok(!text.includes(secret))
  snapshot.observations[0].signals[0] = 'mutated'
  assert.deepEqual(matched(values).observations[0].signals, ['request'])
  assert.equal(matched(new Map([[{}, {requestIdentity: {status: 'UNAVAILABLE'}}]])).status, 'UNAVAILABLE')
})
await control('record-overflow-fails-closed', () => {
  assert.equal(matched(new Map(Array.from({length: 257}, () => [{}, entry()]))).status, 'UNAVAILABLE')
  assert.equal(matched(new Map(), new Map(Array.from({length: 9}, () => [{}, entry()]))).status, 'UNAVAILABLE')
})
await control('page-completion-never-satisfies-network-gate', async () => {
  const p = new EventEmitter(), session = new EventEmitter()
  session.detached = false
  const tracker = createNavigationRequestTracker(session, p)
  const id = 'fixture-opaque-id', url = 'http://127.0.0.1:1/rpc?private=fixture-query'
  const r = {...request(id, p), url: () => url, method: () => 'POST', isInterceptResolutionHandled: () => true}
  try {
    session.emit('*', 'Network.requestWillBeSent', {requestId: id, timestamp: 1, type: 'Fetch', request: {url, method: 'POST'}})
    p.emit('request', r); p.emit('response', {request: () => r, status: () => 200}); p.emit('requestfinished', r)
    const value = tracker.diagnostic()
    assert.equal(value.requests[0].pageRequestIdMatches.status, 'MATCHED')
    assert.equal(value.requests[0].pageRequestIdMatches.recentMatches, 1)
    assert.equal(value.requests[0].pageRequestIdMatches.exactProtocolSessionIdentityEstablished, false)
    assert.equal(value.requests[0].pageLifecycleMatches.sameURLAndMethodOnly, true)
    assert.equal(value.requests[0].pageLifecycleMatches.protocolIdentityEstablished, false)
    assert.equal(value.activeRequests, 1)
    await assert.rejects(tracker.wait(performance.now() + 12), /original navigation deadline elapsed/)
    assert.equal(tracker.diagnostic().activeRequests, 1)
    assert.ok(!JSON.stringify(value).includes(id) && !JSON.stringify(value).includes('fixture-query'))
  } finally {
    tracker.close()
  }
  for (const event of ['request', 'response', 'requestfinished', 'requestfailed']) assert.equal(p.listenerCount(event), 0)
  assert.equal(session.listenerCount('*'), 0)
})

assert.equal(names.length, 16)
console.log('NAVIGATION-REQUEST-ID-CONTROLS ' + JSON.stringify({controls: names.length, pass: true,
  reviewedPuppeteerSourceVersion, SDKOrBrowserExecutions: 0, networkRequests: 0,
  rawOpaqueIdentifiersLogged: false, exactProtocolSessionIdentityEstablished: false,
  completionGateChanged: false, unavailableCompatibilityFailsClosed: true,
  maximumOpaqueRequestIdLength, maximumActiveRecords: 256, maximumRecentRecords: 8}))
