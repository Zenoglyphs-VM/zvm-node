// Bounded standard-library controls; no SDK, browser, request body or network.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {performance} from 'node:perf_hooks'
import {spawnSync} from 'node:child_process'
import {createRequestActionObserver, observeOwnedRequestAction, requestActionObservationLimits} from './market-request-action-support.mjs'
import {createNavigationRequestTracker} from './market-navigation-support.mjs'

const names = []
const control = async (name, run) => {assert.ok(!names.includes(name)); await run(); names.push(name)}
const deferred = () => {let resolve, reject; const promise = new Promise((yes, no) => {resolve = yes; reject = no}); return {promise, resolve, reject}}

await control('same-sync-return-and-one-invocation', () => {
  const observer = createRequestActionObserver(), request = {}; let calls = 0
  assert.equal(observer.observe(request, 'CONTINUE', () => {calls++; return 7}), 7)
  assert.equal(calls, 1); assert.equal(observer.describe(request).actions[0].state, 'SYNC_RETURN')
})
await control('same-synchronous-error', () => {
  const observer = createRequestActionObserver(), request = {}, error = new Error('fixture-error')
  assert.throws(() => observer.observe(request, 'ABORT', () => {throw error}), value => value === error)
  assert.equal(observer.describe(request).actions[0].state, 'SYNC_THROW')
})
await control('pending-native-promise', async () => {
  const observer = createRequestActionObserver(), request = {}, pending = deferred()
  const result = observer.observe(request, 'CONTINUE', () => pending.promise)
  assert.equal(observer.describe(request).actions[0].state, 'PENDING'); assert.equal(observer.summary().pendingActions, 1)
  pending.resolve(); await result; assert.equal(observer.summary().pendingActions, 0)
})
await control('fulfillment-value-and-declared-promise-identity-change', async () => {
  const observer = createRequestActionObserver(), request = {}, value = {}, original = Promise.resolve(value)
  const result = observer.observe(request, 'CONTINUE', () => original)
  assert.notEqual(result, original); assert.equal(await result, value)
  const action = observer.describe(request).actions[0]
  assert.equal(action.state, 'FULFILLED'); assert.equal(action.returnedPromiseIdentityChanged, true)
})
await control('same-rejection-reason', async () => {
  const observer = createRequestActionObserver(), request = {}, error = new Error('fixture-private-error')
  await assert.rejects(observer.observe(request, 'ABORT', () => Promise.reject(error)), value => value === error)
  assert.equal(observer.describe(request).actions[0].state, 'REJECTED')
  assert.ok(!JSON.stringify(observer.describe(request)).includes(error.message))
})
await control('thenable-getter-never-read-or-assimilated', () => {
  const observer = createRequestActionObserver(), request = {}, value = {get then() {throw new Error('then read')}}
  assert.equal(observer.observe(request, 'CONTINUE', () => value), value)
})
await control('promise-subclass-returned-unchanged', async () => {
  class Subclass extends Promise {}
  const observer = createRequestActionObserver(), request = {}, value = Subclass.resolve(3)
  const result = observer.observe(request, 'CONTINUE', () => value)
  assert.equal(result, value); assert.equal(await result, 3)
  assert.equal(observer.describe(request).actions[0].state, 'UNOBSERVED_PROMISE')
})
await control('throwing-promise-species-does-not-replace-return', () => {
  const observer = createRequestActionObserver(), request = {}, value = Promise.resolve(3)
  Object.defineProperty(value, 'constructor', {get() {throw new Error('fixture-species')}})
  assert.equal(observer.observe(request, 'CONTINUE', () => value), value)
  assert.equal(observer.describe(request).actions[0].state, 'UNOBSERVED_PROMISE')
  assert.equal(observer.summary().pendingActions, 0)
})
await control('request-fields-never-read', async () => {
  const observer = createRequestActionObserver(), request = {}
  for (const key of ['id', 'client', 'url', 'method', 'headers', 'postData', 'fetchPostData', 'frame', 'failure', '_requestId'])
    Object.defineProperty(request, key, {get() {throw new Error('fixture-private-field')}})
  await observer.observe(request, 'CONTINUE', () => Promise.resolve())
  assert.equal(observer.describe(request).actions[0].state, 'FULFILLED')
  assert.ok(!JSON.stringify(observer.describe(request)).includes('fixture-private-field'))
})
await control('non-promise-object-return-identity', () => {
  const observer = createRequestActionObserver(), value = {}
  assert.equal(observer.observe({}, 'CONTINUE', () => value), value)
})
await control('per-request-overflow-still-invokes-original', () => {
  const observer = createRequestActionObserver(), request = {}; let calls = 0
  for (let i = 0; i < 4; i++) assert.equal(observer.observe(request, 'CONTINUE', () => ++calls), i + 1)
  assert.equal(calls, 4); assert.equal(observer.describe(request).actions.length, 3)
  assert.equal(observer.describe(request).truncated, true)
})
await control('pending-overflow-never-blocks-original', async () => {
  const observer = createRequestActionObserver(), pending = deferred(), results = []; let calls = 0
  for (let i = 0; i < 257; i++) results.push(observer.observe({}, 'CONTINUE', () => {calls++; return pending.promise}))
  assert.equal(calls, 257); assert.equal(observer.summary().pendingActions, 256)
  assert.equal(observer.summary().refusedActions, 1); assert.equal(results[256], pending.promise)
  pending.resolve(); await Promise.all(results); assert.equal(observer.summary().pendingActions, 0)
})
await control('settlement-releases-pending-capacity', async () => {
  const observer = createRequestActionObserver()
  for (let i = 0; i < 260; i++) await observer.observe({}, 'ABORT', () => Promise.resolve())
  assert.equal(observer.summary().pendingActions, 0); assert.equal(observer.summary().refusedActions, 0)
})
await control('independent-scalar-snapshots', async () => {
  const observer = createRequestActionObserver(), request = {}
  await observer.observe(request, 'CONTINUE', () => Promise.resolve())
  const first = observer.describe(request); first.actions[0].state = 'changed'; first.actions.push({})
  assert.equal(observer.describe(request).actions.length, 1); assert.equal(observer.describe(request).actions[0].state, 'FULFILLED')
  assert.equal(observer.describe(request).protocolAcknowledgementEstablished, false)
  assert.equal(observer.describe(request).networkCompletionEstablished, false)
})
await control('invalid-request-is-diagnostic-bypass', () => {
  const observer = createRequestActionObserver()
  assert.equal(observer.observe(null, 'CONTINUE', () => 4), 4)
  assert.equal(observer.describe(null).status, 'NOT_OBSERVED')
})
await control('unknown-action-is-diagnostic-bypass', () => {
  const observer = createRequestActionObserver(), request = {}
  assert.equal(observer.observe(request, 'OTHER', () => 5), 5)
  assert.equal(observer.describe(request).status, 'NOT_OBSERVED')
})
await control('unknown-request-snapshot-is-private', () => {
  const observer = createRequestActionObserver(), request = new Proxy({}, {get() {throw new Error('private getter')}})
  assert.equal(observer.describe(request).status, 'NOT_OBSERVED')
})
await control('unhandled-rejection-is-not-consumed', () => {
  const source = 'import {observeOwnedRequestAction as observe} from '+JSON.stringify(new URL('./market-request-action-support.mjs', import.meta.url).href)+
    '; void observe({}, "CONTINUE", () => Promise.reject(new Error("fixture-action-rejection")))'
  const result = spawnSync(process.execPath, ['--unhandled-rejections=strict', '--input-type=module', '-e', source],
    {encoding: 'utf8', timeout: 5000, maxBuffer: 16384})
  assert.equal(result.error, undefined); assert.equal(result.status, 1)
  assert.match(result.stderr, /Error: fixture-action-rejection/)
})

const fixture = (id = 'fixture-action-id', path = '/rpc') => {
  const page = new EventEmitter(), session = new EventEmitter(); session.detached = false
  const tracker = createNavigationRequestTracker(session, page), url = 'http://127.0.0.1:1'+path+'?private=fixture-query'
  const request = {id, url: () => url, method: () => 'POST', frame: () => ({page: () => page}), isInterceptResolutionHandled: () => true}
  session.emit('*', 'Network.requestWillBeSent', {requestId: id, timestamp: 1, type: 'Fetch', request: {url, method: 'POST'}})
  page.emit('request', request)
  return {page, session, tracker, request, url, id}
}
await control('fulfilled-method-does-not-complete-network-request', async () => {
  const value = fixture()
  try {
    await observeOwnedRequestAction(value.request, 'CONTINUE', () => Promise.resolve())
    const snapshot = value.tracker.diagnostic(), match = snapshot.requests[0].pageRequestActionMatches
    assert.equal(match.status, 'MATCHED'); assert.equal(match.observations[0].actions[0].state, 'FULFILLED')
    assert.equal(snapshot.activeRequests, 1); assert.equal(match.exactProtocolSessionIdentityEstablished, false)
    await assert.rejects(value.tracker.wait(performance.now() + 12), /original navigation deadline elapsed/)
    assert.equal(value.tracker.diagnostic().activeRequests, 1)
  } finally {value.tracker.close()}
})
await control('pending-method-diagnostic-has-no-private-values', async () => {
  const value = fixture(), pending = deferred()
  const result = observeOwnedRequestAction(value.request, 'ABORT', () => pending.promise)
  try {
    const snapshot = value.tracker.diagnostic(), match = snapshot.requests[0].pageRequestActionMatches
    assert.equal(match.observations[0].actions[0].state, 'PENDING')
    for (const secret of [value.id, 'fixture-query', value.url]) assert.ok(!JSON.stringify(snapshot).includes(secret))
    assert.equal(match.completionGateChanged, false)
  } finally {pending.resolve(); await result; value.tracker.close()}
})
await control('same-id-page-objects-remain-ambiguous', async () => {
  const value = fixture()
  try {
    const other = {...value.request}; value.page.emit('request', other)
    await observeOwnedRequestAction(value.request, 'CONTINUE', () => Promise.resolve())
    const match = value.tracker.diagnostic().requests[0].pageRequestActionMatches
    assert.equal(match.status, 'AMBIGUOUS'); assert.equal(match.activeMatches, 2)
    assert.equal(match.observations.length, 2); assert.equal(match.observations[1].status, 'NOT_OBSERVED')
    assert.equal(value.tracker.diagnostic().activeRequests, 1)
  } finally {value.tracker.close()}
})
await control('url-method-and-id-scope-and-page-cleanup', () => {
  const value = fixture()
  try {
    value.page.emit('requestfinished', value.request)
    const match = value.tracker.diagnostic().requests[0].pageRequestActionMatches
    assert.equal(match.recentMatches, 1); assert.equal(match.activeMatches, 0)
    value.session.emit('*', 'Network.requestWillBeSent', {requestId: 'other', timestamp: 2, type: 'Fetch', request: {url: value.url, method: 'GET'}})
    assert.equal(value.tracker.diagnostic().requests[1].pageRequestActionMatches.status, 'NO_MATCH')
  } finally {value.tracker.close()}
  for (const event of ['request', 'response', 'requestfinished', 'requestfailed']) assert.equal(value.page.listenerCount(event), 0)
  assert.equal(value.session.listenerCount('*'), 0)
})

assert.equal(names.length, 22)
console.log('NAVIGATION-REQUEST-ACTION-CONTROLS '+JSON.stringify({controls: names.length, pass: true,
  memoryAndOwnedNodeChildOnly: true, SDKOrBrowserExecutions: 0, networkRequests: 0,
  unhandledRejectionChildExit: 1, requestFieldsRead: false, protocolCommandsAdded: 0,
  originalCallCountArgumentsFulfillmentAndRejectionPreserved: true,
  nativeReturnedPromiseIdentityAndSettlementMicrotaskChanged: true,
  methodFulfillmentIsProtocolAcknowledgement: false, methodFulfillmentIsNetworkCompletion: false,
  completionGateChanged: false, maximumPendingActions: requestActionObservationLimits.pending,
  maximumActionsPerRequest: requestActionObservationLimits.perRequest}))
