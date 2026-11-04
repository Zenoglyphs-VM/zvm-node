// Pure fake client controls; no SDK, browser, protocol command or network.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {performance} from 'node:perf_hooks'
import {createPageClientObserver, pageClientObservationLimits} from './market-page-client-support.mjs'
import {createNavigationRequestTracker} from './market-navigation-support.mjs'

const names = []
const control = async (name, run) => {assert.ok(!names.includes(name)); await run(); names.push(name)}
const fixture = () => {
  const active = new Map(), recent = new Map(), client = new EventEmitter()
  const observer = createPageClientObserver(active, recent)
  const add = (id = 'fixture-opaque', selected = client) => {
    const entry = {requestIdentity: {status: 'AVAILABLE', requestId: id}, url: 'http://127.0.0.1:1/rpc?secret=private-query', method: 'POST'}
    const request = {client: selected}; active.set(request, entry); observer.observe(request, entry)
    return {entry, request}
  }
  const matches = (id = 'fixture-opaque') => observer.matches(id, 'http://127.0.0.1:1/rpc?secret=private-query', 'POST')
  return {active, recent, client, observer, add, matches}
}

await control('same-client-network-response', () => {
  const f = fixture(); f.add()
  try {
    f.client.emit('Network.responseReceived', {requestId: 'fixture-opaque', timestamp: 1, response: {status: 200}})
    const value = f.matches()
    assert.equal(value.observations[0].status, 'AVAILABLE')
    assert.deepEqual(value.observations[0].signals, [{event: 'Network.responseReceived', timestampPresent: true, responseStatus: 200, responseStage: null}])
    assert.equal(value.crossSessionProtocolIdentityEstablished, false)
    assert.equal(value.serverRequestCorrelationEstablished, false)
  } finally {f.observer.close()}
})
await control('different-network-id-is-unmatched', () => {
  const f = fixture(); f.add()
  try {
    f.client.emit('Network.loadingFinished', {requestId: 'other', timestamp: 2})
    assert.deepEqual(f.matches().observations[0].signals, [])
    assert.equal(f.observer.summary().unmatchedSignals, 1)
  } finally {f.observer.close()}
})
await control('fetch-uses-network-id-not-fetch-id', () => {
  const f = fixture(); f.add()
  try {
    f.client.emit('Fetch.requestPaused', {requestId: 'fixture-opaque', networkId: 'different'})
    assert.deepEqual(f.matches().observations[0].signals, [])
    f.client.emit('Fetch.requestPaused', {requestId: 'different', networkId: 'fixture-opaque', responseStatusCode: 200})
    assert.equal(f.matches().observations[0].signals[0].responseStage, true)
  } finally {f.observer.close()}
})
await control('fetch-request-and-response-stages-remain-distinct', () => {
  const f = fixture(); f.add()
  try {
    f.client.emit('Fetch.requestPaused', {networkId: 'fixture-opaque'})
    f.client.emit('Fetch.requestPaused', {networkId: 'fixture-opaque', responseErrorReason: 'private-error-detail'})
    assert.deepEqual(f.matches().observations[0].signals.map(value => value.responseStage), [false, true])
    assert.ok(!JSON.stringify(f.matches()).includes('private-error-detail'))
  } finally {f.observer.close()}
})
await control('status-and-timestamp-allowlist', () => {
  const f = fixture(); f.add()
  try {
    for (const statusCode of [0, 99, 600, '200', Infinity, null])
      f.client.emit('Network.responseReceivedExtraInfo', {requestId: 'fixture-opaque', statusCode, timestamp: NaN})
    assert.ok(f.matches().observations[0].signals.every(value => value.responseStatus === null && !value.timestampPresent))
  } finally {f.observer.close()}
})
await control('unavailable-page-identity-does-not-read-client', () => {
  const f = fixture(), entry = {requestIdentity: {status: 'UNAVAILABLE'}}
  try {
    f.observer.observe({get client() {assert.fail('client accessed without Page identity')}}, entry)
    assert.equal(f.observer.summary().clients, 0)
  } finally {f.observer.close()}
})
await control('throwing-client-is-unavailable', () => {
  const f = fixture(), row = f.add('different')
  try {
    const entry = {...row.entry, requestIdentity: {status: 'AVAILABLE', requestId: 'fixture-opaque'}}
    f.active.set({}, entry)
    f.observer.observe({get client() {throw new Error('private-client-error')}}, entry)
    assert.equal(f.matches().observations[0].reason, 'READ_UNAVAILABLE')
    assert.ok(!JSON.stringify(f.matches()).includes('private-client-error'))
  } finally {f.observer.close()}
})
await control('unsupported-or-detached-client', () => {
  for (const client of [null, {}, {on() {}}, {...new EventEmitter(), on() {}, off() {}, detached: true}]) {
    const f = fixture(); f.add('fixture-opaque', client)
    try {assert.equal(f.matches().observations[0].reason, 'CLIENT_UNAVAILABLE')} finally {f.observer.close()}
  }
})
await control('throwing-subscription-unwinds-only-owned-listeners', () => {
  const f = fixture(), client = new EventEmitter(), original = client.on
  let calls = 0
  client.on = function(type, listener) {original.call(this, type, listener); if (++calls === 3) throw new Error('private-subscribe-error'); return this}
  const existing = () => {}; original.call(client, 'Network.loadingFinished', existing)
  f.add('fixture-opaque', client)
  try {
    assert.equal(f.matches().observations[0].reason, 'SUBSCRIPTION_UNAVAILABLE')
    assert.deepEqual(client.eventNames(), ['Network.loadingFinished'])
    assert.deepEqual(client.listeners('Network.loadingFinished'), [existing])
    assert.equal(f.observer.summary().clients, 0)
  } finally {f.observer.close()}
})
await control('bounded-client-count', () => {
  const f = fixture(), clients = Array.from({length: 5}, () => new EventEmitter())
  try {
    clients.forEach((client, i) => f.add('client-' + i, client))
    assert.equal(f.observer.summary().clients, 4)
    assert.equal(f.observer.summary().refusedClients, 1)
    assert.equal(f.matches('client-4').observations[0].reason, 'CLIENT_LIMIT')
    assert.equal(clients[4].eventNames().length, 0)
  } finally {f.observer.close()}
  assert.ok(clients.every(client => client.eventNames().length === 0))
})
await control('bounded-signal-ring-and-independent-snapshot', () => {
  const f = fixture(); f.add()
  try {
    for (let i = 0; i < 10; i++) f.client.emit('Network.responseReceivedExtraInfo', {requestId: 'fixture-opaque', statusCode: 200 + i})
    const value = f.matches().observations[0]
    assert.equal(value.signals.length, 8); assert.equal(value.signals[0].responseStatus, 202); assert.equal(value.signalsTruncated, true)
    value.signals[0].responseStatus = 599
    assert.equal(f.matches().observations[0].signals[0].responseStatus, 202)
  } finally {f.observer.close()}
})
await control('same-id-on-different-client-remains-separate', () => {
  const f = fixture(), other = new EventEmitter(); f.add(); f.add('fixture-opaque', other)
  try {
    f.client.emit('Network.loadingFinished', {requestId: 'fixture-opaque'})
    const value = f.matches()
    assert.equal(value.status, 'AMBIGUOUS')
    assert.deepEqual(value.observations.map(row => row.signals.length), [1, 0])
  } finally {f.observer.close()}
})
await control('url-method-id-and-record-bounds-fail-closed', () => {
  const f = fixture(); f.add()
  try {
    assert.equal(f.observer.matches('', '', '').status, 'UNAVAILABLE')
    assert.equal(f.observer.matches('fixture-opaque', 'different', 'POST').status, 'NO_MATCH')
    assert.equal(f.observer.matches('fixture-opaque', 'http://127.0.0.1:1/rpc?secret=private-query', 'GET').status, 'NO_MATCH')
    for (let i = 0; i < 256; i++) f.active.set({}, {})
    assert.equal(f.matches().status, 'UNAVAILABLE')
  } finally {f.observer.close()}
})
await control('no-sensitive-reads-or-protocol-commands', () => {
  const f = fixture(), row = f.add()
  try {
    for (const key of ['send', 'detach', 'id']) Object.defineProperty(f.client, key, {get() {assert.fail('client command or identity accessed')}})
    for (const key of ['headers', 'postData', 'fetchPostData', '_requestId', '_interceptionId'])
      Object.defineProperty(row.request, key, {get() {assert.fail('private request data accessed')}})
    const event = {requestId: 'fixture-opaque', response: {status: 200}}
    for (const key of ['headers', 'body', 'request']) Object.defineProperty(event, key, {get() {assert.fail('private event data accessed')}})
    f.client.emit('Network.responseReceived', event)
    const text = JSON.stringify(f.matches())
    for (const secret of ['fixture-opaque', 'private-query', '/rpc']) assert.ok(!text.includes(secret))
  } finally {f.observer.close()}
})
await control('recent-record-remains-visible-until-evicted', () => {
  const f = fixture(), row = f.add()
  try {
    f.active.delete(row.request); f.recent.set(row.request, row.entry)
    f.client.emit('Network.loadingFinished', {requestId: 'fixture-opaque'})
    assert.equal(f.matches().observations[0].signals.length, 1)
    f.recent.clear()
    assert.equal(f.matches().status, 'NO_MATCH')
  } finally {f.observer.close()}
})
await control('close-is-idempotent-and-preserves-other-listeners', () => {
  const f = fixture(), existing = () => {}; f.client.on('Network.loadingFinished', existing); f.add()
  f.observer.close(); f.observer.close()
  assert.deepEqual(f.client.listeners('Network.loadingFinished'), [existing])
  f.client.emit('Network.loadingFinished', {requestId: 'fixture-opaque'})
  assert.equal(f.matches().observations[0].signals.length, 0)
  assert.equal(f.matches().closed, true)
})
await control('late-or-throwing-events-are-not-completion-proof', () => {
  const f = fixture(); f.add()
  try {
    f.client.emit('Network.loadingFinished', {get requestId() {throw new Error('private-event-error')}})
    assert.equal(f.observer.summary().readErrors, 1)
    assert.equal(f.matches().observations[0].signals.length, 0)
  } finally {f.observer.close()}
})
await control('raw-client-terminal-never-completes-dedicated-network-gate', async () => {
  const page = new EventEmitter(), session = new EventEmitter(), client = new EventEmitter()
  session.detached = false
  const tracker = createNavigationRequestTracker(session, page)
  const id = 'fixture-opaque', url = 'http://127.0.0.1:1/rpc?secret=private-query'
  const request = {id, client, frame: () => ({page: () => page}), url: () => url,
    method: () => 'POST', isInterceptResolutionHandled: () => true}
  try {
    session.emit('*', 'Network.requestWillBeSent', {requestId: id, timestamp: 1, type: 'Fetch', request: {url, method: 'POST'}})
    page.emit('request', request)
    client.emit('Network.responseReceived', {requestId: id, timestamp: 2, response: {status: 200}})
    client.emit('Network.loadingFinished', {requestId: id, timestamp: 3})
    const diagnostic = tracker.diagnostic()
    assert.deepEqual(diagnostic.requests[0].pageClientMatches.observations[0].signals.map(value => value.event),
      ['Network.responseReceived', 'Network.loadingFinished'])
    assert.equal(diagnostic.activeRequests, 1)
    await assert.rejects(tracker.wait(performance.now() + 12), /original navigation deadline elapsed/)
    assert.equal(tracker.diagnostic().activeRequests, 1)
    assert.ok(!JSON.stringify(diagnostic).includes(id) && !JSON.stringify(diagnostic).includes('private-query'))
  } finally {tracker.close()}
  assert.equal(client.eventNames().length, 0)
  for (const name of ['request', 'response', 'requestfinished', 'requestfailed']) assert.equal(page.listenerCount(name), 0)
})

assert.equal(names.length, 18)
console.log('NAVIGATION-PAGE-CLIENT-CONTROLS ' + JSON.stringify({controls: names.length, pass: true,
  reviewedPuppeteerSourceVersion: '25.11.0', SDKOrBrowserExecutions: 0, networkRequests: 0, protocolCommands: 0,
  originalRequestAndWriterPreserved: true, rawIdentifiersLogged: false, completionGateChanged: false,
  crossSessionProtocolIdentityEstablished: false, serverRequestCorrelationEstablished: false,
  observationWindow: 'AFTER_PAGE_REQUEST_CALLBACK', limits: pageClientObservationLimits}))
