// Standard-library controls. No SDK, browser, socket, body substitution or RPC.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {performance} from 'node:perf_hooks'
import {readFileSync} from 'node:fs'
import {createHash} from 'node:crypto'
import {createQuoteRequestPurposeObserver, quotePurposeLimits} from './market-quote-request-purpose.mjs'
import {createOwnedRPCCorrelation, ownedRPCHeader} from './market-rpc-correlation-support.mjs'
import {createNavigationRequestTracker} from './market-navigation-support.mjs'

const origin = 'http://127.0.0.1:1', controls = []
const firstPhase = 'ACCOUNT_CHANGE_REFUSAL', secondPhase = 'FORM_EDIT_SNAPSHOT'
const nonce = index => index.toString(16).padStart(32, '0')
async function check(name, operation) {assert.ok(!controls.includes(name)); await operation(); controls.push(name)}
function request(operation = () => undefined) {
  return {calls: [], url: () => origin + '/rpc', method: () => 'POST', headers: () => ({Accept: 'application/json'}),
    continue(...args) {this.calls.push(args); return operation(...args)}}
}
function fixture(options = {}) {
  const observer = createQuoteRequestPurposeObserver()
  let next = 0
  const helper = createOwnedRPCCorrelation(origin, {nonceFactory: () => nonce(++next), requestPurpose: observer.describe, ...options})
  const bind = (req, index, id = 'request-' + index, session = {}) => {
    helper.continue(req)
    const entry = {url: origin + '/rpc', method: 'POST'}
    helper.bind(id, entry, session)
    helper.observeSignal('Network.responseReceivedExtraInfo', {requestId: id, headers: {[ownedRPCHeader]: nonce(index)}}, undefined, session)
    return entry
  }
  return {observer, helper, bind}
}
function report(helper, entry) {return helper.requestPurposes(helper.select([entry]))}
const purpose = value => value.targets[0].purpose

await check('mark-uses-only-exact-object-identity-and-no-request-fields', () => {
  const observer = createQuoteRequestPurposeObserver()
  const a = new Proxy({}, {get() {throw new Error('private-field-read')}, ownKeys() {throw new Error('private-enumeration')}})
  assert.equal(observer.mark(a, firstPhase), true)
  assert.deepEqual(observer.describe(a), {status: 'MARKED', phase: firstPhase})
  assert.deepEqual(observer.describe({}), {status: 'NOT_MARKED', phase: null})
})
await check('two-original-fixture-purposes-remain-distinct', () => {
  const observer = createQuoteRequestPurposeObserver(), a = {}, b = {}
  assert.ok(observer.mark(a, firstPhase)); assert.ok(observer.mark(b, secondPhase))
  assert.equal(observer.describe(a).phase, firstPhase); assert.equal(observer.describe(b).phase, secondPhase)
  assert.equal(observer.summary().markedRequests, 2)
})
await check('invalid-inputs-and-private-labels-are-not-retained', () => {
  const observer = createQuoteRequestPurposeObserver()
  for (const value of [null, undefined, 1, 'private-request']) assert.equal(observer.mark(value, firstPhase), false)
  for (const label of ['private-label', null, {}, firstPhase + '-private']) assert.equal(observer.mark({}, label), false)
  assert.equal(observer.describe(null).status, 'UNAVAILABLE')
  assert.ok(!JSON.stringify(observer.summary()).includes('private'))
})
await check('idempotent-mark-and-conflicting-phase-refuse-attribution', () => {
  const observer = createQuoteRequestPurposeObserver(), req = {}
  assert.ok(observer.mark(req, firstPhase)); assert.ok(observer.mark(req, firstPhase))
  assert.equal(observer.mark(req, secondPhase), false)
  assert.deepEqual(observer.describe(req), {status: 'AMBIGUOUS', phase: null})
  assert.equal(observer.summary().markedRequests, 1); assert.equal(observer.summary().conflictingMarks, 1)
})
await check('weak-observation-allocation-is-bounded-to-two-marks', () => {
  const observer = createQuoteRequestPurposeObserver()
  for (let index = 0; index < quotePurposeLimits.markedRequests; index++) assert.ok(observer.mark({}, firstPhase))
  const third = {}; assert.equal(observer.mark(third, firstPhase), false)
  assert.equal(observer.describe(third).status, 'NOT_MARKED'); assert.equal(observer.summary().refusedMarks, 1)
})
await check('observer-copies-cannot-rewrite-the-private-purpose', () => {
  const observer = createQuoteRequestPurposeObserver(), req = {}; observer.mark(req, firstPhase)
  const value = observer.describe(req); value.phase = secondPhase; value.status = 'NOT_MARKED'
  assert.equal(observer.describe(req).phase, firstPhase)
})
await check('unique-owned-header-entry-binds-the-original-marked-object', () => {
  const {observer, helper, bind} = fixture(), req = request(); observer.mark(req, firstPhase)
  const value = report(helper, bind(req, 1))
  assert.equal(value.selectionScope, 'PENDING_NETWORK')
  assert.equal(value.targets[0].requestObjectTagStatus, 'MATCHED')
  assert.equal(value.targets[0].sameUniqueOwnedHeaderRecordAsSelectedTarget, true)
  assert.deepEqual(purpose(value), {status: 'MARKED', phase: firstPhase, reason: null})
  assert.ok(!JSON.stringify(value).includes(nonce(1)))
})
await check('equal-url-method-and-fields-cannot-borrow-another-request-purpose', () => {
  const {observer, helper, bind} = fixture(), marked = request(), other = request()
  observer.mark(marked, firstPhase); bind(marked, 1)
  assert.deepEqual(purpose(report(helper, bind(other, 2))), {status: 'NOT_MARKED', phase: null, reason: null})
})
await check('forged-and-other-helper-selections-are-unavailable', () => {
  const {helper, bind} = fixture(), entry = bind(request(), 1), selected = helper.select([entry])
  assert.equal(helper.requestPurposes({...selected}).reason, 'SELECTION')
  assert.equal(fixture().helper.requestPurposes(selected).reason, 'SELECTION')
  assert.deepEqual(helper.requestPurposes(null).targets, [])
})
await check('duplicate-selected-header-record-is-ambiguous', () => {
  const {observer, helper, bind} = fixture(), req = request(); observer.mark(req, firstPhase)
  const a = bind(req, 1), b = {url: origin + '/rpc', method: 'POST'}
  helper.observeSignal('Network.responseReceivedExtraInfo', {requestId: 'other', headers: {[ownedRPCHeader]: nonce(1)}}, b, {})
  const value = helper.requestPurposes(helper.select([a, b]))
  assert.ok(value.targets.every(row => row.requestObjectTagStatus === 'AMBIGUOUS' && !row.sameUniqueOwnedHeaderRecordAsSelectedTarget))
  assert.ok(value.targets.every(row => row.purpose.status === 'UNAVAILABLE'))
})
await check('missing-and-conflicting-header-echo-cannot-establish-purpose', () => {
  const {observer, helper, bind} = fixture(), a = request(), b = request(); observer.mark(a, firstPhase)
  const entry = bind(a, 1), other = bind(b, 2)
  const absent = report(helper, {url: origin + '/rpc', method: 'POST'})
  assert.equal(absent.targets[0].requestObjectTagStatus, 'UNAVAILABLE')
  helper.observeSignal('Network.responseReceivedExtraInfo', {requestId: 'conflict', headers: {[ownedRPCHeader]: nonce(2)}}, entry, {})
  assert.equal(report(helper, entry).targets[0].requestObjectTagStatus, 'UNAVAILABLE')
  assert.equal(purpose(report(helper, other)).status, 'NOT_MARKED')
})
await check('late-echo-keeps-the-original-session-and-request-object', () => {
  const {observer, helper} = fixture(), req = request(), session = {}; observer.mark(req, secondPhase); helper.continue(req)
  const entry = {url: origin + '/rpc', method: 'POST'}; helper.bind('late', entry, session)
  helper.observeSignal('Network.responseReceivedExtraInfo', {requestId: 'late', headers: {[ownedRPCHeader]: nonce(1)}}, undefined, session)
  assert.equal(purpose(report(helper, entry)).phase, secondPhase)
  const isolated = {url: origin + '/rpc', method: 'POST'}; helper.bind('late', isolated, {})
  assert.equal(report(helper, isolated).targets[0].requestObjectTagStatus, 'UNAVAILABLE')
})
await check('continue-arguments-and-exact-returned-promise-remain-unchanged', () => {
  const promise = Promise.resolve('private-result'), {observer, helper} = fixture(), req = request(() => promise)
  observer.mark(req, firstPhase); assert.equal(helper.continue(req), promise)
  assert.deepEqual(req.calls, [[{headers: {Accept: 'application/json', [ownedRPCHeader]: nonce(1)}}]])
})
await check('original-sync-throw-and-rejection-propagate-once', async () => {
  const error = new Error('private-error'), first = fixture(), a = request(() => {throw error})
  first.observer.mark(a, firstPhase); assert.throws(() => first.helper.continue(a), value => value === error); assert.equal(a.calls.length, 1)
  const second = fixture(), promise = Promise.reject(error), b = request(() => promise); second.observer.mark(b, firstPhase)
  assert.equal(second.helper.continue(b), promise); await assert.rejects(promise, value => value === error); assert.equal(b.calls.length, 1)
})
await check('untrusted-purpose-callback-refuses-diagnostics-without-changing-operation', () => {
  for (const requestPurpose of [() => {throw new Error('private-secret')}, () => ({status: 'MARKED', phase: 'private-secret'}),
    () => ({status: 'MARKED', phase: firstPhase, private: 'private-secret'}),
    () => ({get status() {throw new Error('private-secret')}, phase: firstPhase})]) {
    const {helper, bind} = fixture({requestPurpose}), req = request(), value = report(helper, bind(req, 1))
    assert.equal(purpose(value).status, 'UNAVAILABLE'); assert.equal(value.observerReadErrors, 1); assert.equal(req.calls.length, 1)
    assert.equal(req.calls[0][0].headers[ownedRPCHeader], nonce(1)); assert.ok(!JSON.stringify(value).includes('private-secret'))
  }
})
await check('purpose-is-captured-before-first-continue-and-never-reclassified-late', () => {
  const {observer, helper, bind} = fixture(), req = request(), entry = bind(req, 1)
  observer.mark(req, firstPhase)
  assert.equal(purpose(report(helper, entry)).status, 'NOT_MARKED')
  helper.continue(req); assert.equal(purpose(report(helper, entry)).status, 'NOT_MARKED'); assert.equal(req.calls.length, 2)
})
await check('public-selection-and-purpose-copies-cannot-change-private-bindings', () => {
  const {observer, helper, bind} = fixture(), req = request(); observer.mark(req, firstPhase)
  const entry = bind(req, 1), selected = helper.select([entry]); selected.targets[0].index = 99
  const first = helper.requestPurposes(selected); first.targets[0].purpose.phase = secondPhase
  const second = helper.requestPurposes(selected); assert.equal(second.targets[0].index, 0); assert.equal(purpose(second).phase, firstPhase)
})
await check('purpose-echo-and-server-identity-never-complete-a-network-request', async () => {
  class Session extends EventEmitter {detached = false; signal(type, event) {this.emit('*', type, event)}}
  const session = new Session(), {observer, helper} = fixture(), tracker = createNavigationRequestTracker(session, undefined, helper), req = request()
  observer.mark(req, firstPhase); helper.continue(req)
  session.signal('Network.requestWillBeSent', {requestId: 'pending', timestamp: 1, request: {url: origin + '/rpc', method: 'POST'}})
  session.signal('Network.responseReceivedExtraInfo', {requestId: 'pending', headers: {[ownedRPCHeader]: nonce(1)}})
  const value = helper.requestPurposes(helper.select(tracker.ownedRPCEntries()))
  assert.equal(purpose(value).status, 'MARKED')
  for (const key of ['completionGateChanged', 'responseDeliveryEstablished', 'protocolSessionIdentityEstablished', 'serverRequestCorrelationEstablished']) assert.equal(value[key], false)
  assert.equal(value.additionalHTTPRequests, 0); assert.equal(value.additionalProtocolCommands, 0)
  await assert.rejects(tracker.wait(performance.now() + 10), /original navigation deadline elapsed/)
  assert.equal(tracker.diagnostic().activeRequests, 1)
  session.signal('Network.loadingFinished', {requestId: 'pending', timestamp: 2}); await tracker.wait(performance.now() + 500, 1); tracker.close()
})

assert.equal(controls.length, 18)
const digest = path => createHash('sha256').update(readFileSync(new URL(path, import.meta.url))).digest('hex')
console.log('QUOTE-PURPOSE-CONTROLS ' + JSON.stringify({node: process.version, controls, controlCount: controls.length, pass: true,
  observerSourceSHA256: digest('./market-quote-request-purpose.mjs'), correlationSourceSHA256: digest('./market-rpc-correlation-support.mjs'),
  navigationSourceSHA256: digest('./market-navigation-support.mjs'), SDKBrowserSocketOrHTTPExecutions: 0,
  originalContinueArgumentsAndOutcomePreserved: true, originalNetworkCompletionGatePreserved: true, rawPrivateValuesReported: false}))

await import('./market-rpc-request-context.test.mjs')
