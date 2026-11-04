// Standard-library controls. No SDK, browser, socket or HTTP request is run.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {performance} from 'node:perf_hooks'
import {readFileSync} from 'node:fs'
import {createHash} from 'node:crypto'
import {createOwnedRPCRequestContext, captureRPCRequestContext as capture, observeRPCRequestContext as observe,
  bindRPCRequestContext as bind, describeRPCRequestContext as describe, summarizeRPCRequestContext as summary,
  rpcRequestContextLimits as limits} from './market-rpc-request-context.mjs'
import {createOwnedRequestDiagnostics} from './market-owned-request-diagnostics.mjs'
import {createOwnedRPCCorrelation, ownedRPCHeader} from './market-rpc-correlation-support.mjs'
import {createNavigationRequestTracker} from './market-navigation-support.mjs'

const origin = 'http://127.0.0.1:1', a = '0x' + '1'.repeat(40), b = '0x' + '2'.repeat(40), c = '0x' + '3'.repeat(40)
const controls = [], selectors = {APPROVAL_FOR_ALL: '0x11111111', FEE_BPS: '0x22222222'}, nonce = n => n.toString(16).padStart(32, '0')
async function check(name, operation) {assert.ok(!controls.includes(name)); await operation(); controls.push(name)}
const observer = extra => createOwnedRPCRequestContext(origin, {targets: {collection: a, exchange: b, token: c}, selectors: () => selectors, ...extra})
function request(body = {jsonrpc: '2.0', id: 'private-id', method: 'eth_call', params: [{to: b, data: '0x22222222'}]}, operation = () => undefined) {
  return {calls: [], url: () => origin + '/rpc', method: () => 'POST', headers: () => ({Accept: 'application/json'}),
    postData: () => typeof body === 'string' ? body : JSON.stringify(body), continue(...args) {this.calls.push(args); return operation(...args)}}
}
const doc = (id = 'doc', frameId = 'frame', loaderId = 'loader', timestamp = 1, url = origin + '/item/1') =>
  ({requestId: id, frameId, loaderId, timestamp, type: 'Document', request: {url, method: 'GET'}})
const rpc = (id = 'rpc', frameId = 'frame', loaderId = 'loader', timestamp = 2) =>
  ({requestId: id, frameId, loaderId, timestamp, type: 'Fetch', request: {url: origin + '/rpc', method: 'POST'}})
function contextEntry(value, session = {}, start = rpc()) {
  observe(value, 'Network.requestWillBeSent', doc(), session)
  observe(value, 'Network.requestWillBeSent', start, session)
  const entry = {}; bind(value, start.requestId, entry, session); return {entry, session}
}

await check('only-allowlisted-methods-roles-and-call-kinds-are-reported', () => {
  const value = observer(), ref = capture(value, request())
  const out = describe(value, ref, null)
  assert.deepEqual(out.request, {status: 'AVAILABLE', reason: null, batch: false, entries: 1,
    calls: [{method: 'ETH_CALL', callKind: 'FEE_BPS', targetRole: 'EXCHANGE'}]})
  for (const token of [a, b, c, 'private-id', '0x22222222']) assert.ok(!JSON.stringify(out).includes(token))
})
await check('unknown-methods-and-private-parameters-never-escape', () => {
  const value = observer(), ref = capture(value, request({method: 'private-method', params: ['private-secret'], id: 'private-id'}))
  const out = describe(value, ref, null)
  assert.deepEqual(out.request.calls, [{method: 'OTHER_RPC', callKind: 'NOT_CALL', targetRole: 'NOT_CALL'}])
  assert.ok(!JSON.stringify(out).includes('private'))
})
await check('collection-token-and-unknown-call-targets-are-separated', () => {
  const value = observer()
  for (const [to, targetRole] of [[a, 'COLLECTION'], [c, 'TOKEN'], ['0x' + '4'.repeat(40), 'OTHER_TARGET']]) {
    const out = describe(value, capture(value, request({method: 'eth_call', params: [{to, data: '0x11111111'}]})), null)
    assert.equal(out.request.calls[0].targetRole, targetRole); assert.equal(out.request.calls[0].callKind, 'APPROVAL_FOR_ALL')
  }
})
await check('batch-order-width-and-invalid-envelope-refusals', () => {
  const value = observer(), body = [{method: 'eth_chainId'}, {method: 'eth_getBalance', params: [a]}, {method: 'private-method'}]
  const out = describe(value, capture(value, request(body)), null).request
  assert.equal(out.batch, true); assert.equal(out.entries, 3); assert.deepEqual(out.calls.map(row => row.method), ['CHAIN_ID', 'BALANCE', 'OTHER_RPC'])
  for (const bad of [[], Array.from({length: 17}, () => ({method: 'eth_call'})), null, true, {method: 1}, [null]])
    assert.equal(describe(value, capture(value, request(bad)), null).request.status, 'UNAVAILABLE')
})
await check('input-byte-and-JSON-bounds-refuse-without-body-output', () => {
  const value = observer()
  for (const body of ['private-json', 'x'.repeat(limits.inputBytes + 1), JSON.stringify({method: 'eth_call', extra: '\u20ac'.repeat(6000)})]) {
    const out = describe(value, capture(value, request(body)), null)
    assert.equal(out.request.status, 'UNAVAILABLE'); assert.ok(!JSON.stringify(out).includes(body.slice(0, 40)))
  }
})
await check('unowned-requests-do-not-read-post-data', () => {
  const value = observer(), req = request(); req.url = () => 'http://127.0.0.1:2/rpc'; req.postData = () => {throw new Error('private-error')}
  assert.equal(describe(value, capture(value, req), null).request.reason, 'UNOWNED_REQUEST'); assert.equal(summary(value).readErrors, 0)
})
await check('unavailable-or-throwing-body-readers-do-not-leak-errors', () => {
  const value = observer(), absent = request(), throwing = request()
  absent.postData = () => undefined; throwing.postData = () => {throw new Error('private-secret')}
  assert.equal(describe(value, capture(value, absent), null).request.reason, 'BODY_UNAVAILABLE')
  const out = describe(value, capture(value, throwing), null)
  assert.equal(out.request.reason, 'READ_REFUSED'); assert.ok(!JSON.stringify(out).includes('private-secret'))
})
await check('same-session-document-request-is-joined-without-identifiers', () => {
  const value = observer(), ref = capture(value, request()), {entry} = contextEntry(value)
  const out = describe(value, ref, entry)
  assert.deepEqual(out.document, {status: 'AVAILABLE', reason: null, documentRequestOrdinalAtStart: 1,
    latestSameFrameDocumentRequestOrdinal: 1, loaderMatchesDocumentAtStart: true, loaderMatchesLatestDocument: true,
    timestampNotBeforeDocumentStart: true, sameDocumentRequestStillLatest: true})
  for (const token of ['"frame"', '"loader"', '"rpc"', 'private-id']) assert.ok(!JSON.stringify(out).includes(token))
})
await check('later-document-request-does-not-rewrite-the-start-snapshot', () => {
  const value = observer(), ref = capture(value, request()), {entry, session} = contextEntry(value)
  observe(value, 'Network.requestWillBeSent', doc('next-doc', 'frame', 'next-loader', 3), session)
  const out = describe(value, ref, entry).document
  assert.equal(out.documentRequestOrdinalAtStart, 1); assert.equal(out.latestSameFrameDocumentRequestOrdinal, 2)
  assert.equal(out.loaderMatchesLatestDocument, false); assert.equal(out.sameDocumentRequestStillLatest, false)
})
await check('another-frame-and-another-session-do-not-borrow-document-context', () => {
  const value = observer(), ref = capture(value, request()), {entry, session} = contextEntry(value)
  observe(value, 'Network.requestWillBeSent', doc('child', 'child-frame', 'child-loader', 3), session)
  assert.equal(describe(value, ref, entry).document.latestSameFrameDocumentRequestOrdinal, 1)
  const other = {}, unjoined = {}; observe(value, 'Network.requestWillBeSent', rpc(), other); bind(value, 'rpc', unjoined, other)
  assert.equal(describe(value, ref, unjoined).document.status, 'UNAVAILABLE')
})
await check('reused-primary-identity-and-missing-start-remain-unavailable', () => {
  const value = observer(), ref = capture(value, request()), {entry, session} = contextEntry(value)
  observe(value, 'Network.requestWillBeSent', rpc(), session)
  assert.equal(describe(value, ref, entry).document.reason, 'AMBIGUOUS_BINDING')
  const second = {}; bind(value, 'missing', second, session)
  assert.equal(describe(value, ref, second).document.reason, 'NO_BOUND_START')
})
await check('foreign-navigation-invalidates-prior-owned-document-context', () => {
  const value = observer(), ref = capture(value, request()), {entry, session} = contextEntry(value)
  observe(value, 'Network.requestWillBeSent', doc('foreign', 'frame', 'foreign-loader', 3, 'http://127.0.0.1:2/private'), session)
  assert.equal(describe(value, ref, entry).document.reason, 'DOCUMENT_UNAVAILABLE')
})
await check('record-scope-and-frame-bounds-are-visible-and-fail-closed', () => {
  const value = observer()
  for (let index = 0; index < limits.records; index++) assert.ok(capture(value, request()))
  assert.equal(capture(value, request()), null); assert.equal(summary(value).capturedRequests, limits.records)
  const scoped = observer()
  for (let index = 0; index <= limits.scopes; index++) observe(scoped, 'Network.requestWillBeSent', doc(), {})
  assert.equal(summary(scoped).scopes, limits.scopes); assert.equal(summary(scoped).truncated, true)
  const framed = observer(), session = {}
  for (let index = 0; index <= limits.frames; index++) observe(framed, 'Network.requestWillBeSent', doc('doc-' + index, 'frame-' + index), session)
  assert.equal(summary(framed).scopeUsage[0].frames, limits.frames); assert.equal(summary(framed).truncated, true)
})
await check('start-and-binding-caps-never-grow-on-reused-bound-identities', () => {
  const value = observer(), session = {}
  for (let index = 0; index <= limits.starts; index++) observe(value, 'Network.requestWillBeSent', rpc('start-' + index), session)
  assert.equal(summary(value).scopeUsage[0].starts, limits.starts)
  const bound = observer(), other = {}
  for (let index = 0; index <= limits.bindings; index++) bind(bound, 'bound-' + index, {}, other)
  assert.equal(summary(bound).scopeUsage[0].bindings, limits.bindings)
  for (let index = 0; index < limits.starts; index++) observe(bound, 'Network.requestWillBeSent', rpc('new-' + index), other)
  observe(bound, 'Network.requestWillBeSent', rpc('bound-0'), other)
  assert.equal(summary(bound).scopeUsage[0].starts, limits.starts)
})
await check('opaque-observers-and-capture-references-cannot-be-forged', () => {
  const value = observer(), ref = capture(value, request()), fake = new Proxy({}, {get() {throw new Error('private-getter')}})
  assert.deepEqual(Object.keys(value), []); assert.deepEqual(Object.keys(ref), [])
  assert.equal(capture(fake, request()), null); assert.equal(summary(fake).status, 'UNAVAILABLE')
  assert.equal(describe(value, {}, null).request.status, 'UNAVAILABLE')
})
await check('ambiguous-selector-and-target-labels-never-guess', () => {
  const value = observer({targets: {collection: a, exchange: a}, selectors: () => ({APPROVAL_FOR_ALL: '0x11111111', FEE_BPS: '0x11111111'})})
  const out = describe(value, capture(value, request({method: 'eth_call', params: [{to: a, data: '0x11111111'}]})), null).request.calls[0]
  assert.equal(out.callKind, 'AMBIGUOUS_CALL'); assert.equal(out.targetRole, 'AMBIGUOUS_TARGET')
  const refused = observer({selectors: () => ({'private-label': 'private-secret'})})
  const safe = describe(refused, capture(refused, request()), null)
  assert.equal(safe.request.calls[0].callKind, 'OTHER_CALL'); assert.ok(!JSON.stringify(safe).includes('private-secret'))
})
await check('returned-snapshots-and-summary-copies-are-independent', () => {
  const value = observer(), ref = capture(value, request()), {entry} = contextEntry(value)
  const first = describe(value, ref, entry); first.request.calls[0].method = 'private-label'; first.document.documentRequestOrdinalAtStart = 99
  const stats = summary(value); stats.scopeUsage[0].frames = 99
  assert.equal(describe(value, ref, entry).request.calls[0].method, 'ETH_CALL')
  assert.equal(describe(value, ref, entry).document.documentRequestOrdinalAtStart, 1); assert.equal(summary(value).scopeUsage[0].frames, 1)
})
await check('owned-header-selection-binds-context-without-changing-original-operation', () => {
  const value = observer(), promise = Promise.resolve('private-result'), helper = createOwnedRPCCorrelation(origin, {nonceFactory: () => nonce(1), requestContext: value})
  const req = request(undefined, () => promise); assert.equal(helper.continue(req), promise)
  assert.deepEqual(req.calls, [[{headers: {Accept: 'application/json', [ownedRPCHeader]: nonce(1)}}]])
  const session = {}, entry = {url: origin + '/rpc', method: 'POST'}
  helper.observeSignal('Network.requestWillBeSent', doc(), undefined, session)
  helper.observeSignal('Network.requestWillBeSent', rpc(), undefined, session); helper.bind('rpc', entry, session)
  helper.observeSignal('Network.responseReceivedExtraInfo', {requestId: 'rpc', headers: {[ownedRPCHeader]: nonce(1)}}, undefined, session)
  const selected = helper.select([entry]), out = helper.requestContexts(selected)
  assert.equal(out.targets[0].sameUniqueOwnedHeaderRecordAsSelectedTarget, true)
  assert.equal(out.targets[0].context.request.calls[0].callKind, 'FEE_BPS'); assert.equal(out.targets[0].context.document.status, 'AVAILABLE')
  assert.equal(helper.requestContexts({...selected}).status, 'UNAVAILABLE'); assert.ok(!JSON.stringify(out).includes(nonce(1)))
})
await check('composed-factory-uses-declared-codecs-lazily-and-keeps-purpose-observation', () => {
  let builds = 0
  const names = ['isApprovedForAll', 'allowance', 'balanceOf', 'ownerOf', 'royaltyInfo', 'feeBps', 'feeRecipient']
  const {rpcCorrelation: helper, quotePurpose} = createOwnedRequestDiagnostics(origin, {collection: a, exchange: b}, () => {
    builds++; return {parseAbi: value => value, encodeFunctionData: ({functionName}) => '0x' + (names.indexOf(functionName) + 1).toString(16).padStart(8, '0')}
  })
  assert.equal(builds, 0)
  const req = request({method: 'eth_call', params: [{to: b, data: '0x00000006'}]}); quotePurpose.mark(req, 'ACCOUNT_CHANGE_REFUSAL'); helper.continue(req)
  assert.equal(builds, 1)
  const entry = {url: origin + '/rpc', method: 'POST'}, actualNonce = req.calls[0][0].headers[ownedRPCHeader]
  helper.observeSignal('Network.responseReceivedExtraInfo', {requestId: 'same', headers: {[ownedRPCHeader]: actualNonce}}, entry)
  const selected = helper.select([entry])
  assert.equal(helper.requestPurposes(selected).targets[0].purpose.phase, 'ACCOUNT_CHANGE_REFUSAL')
  assert.equal(helper.requestContexts(selected).targets[0].context.request.calls[0].callKind, 'FEE_BPS')
})
await check('context-and-header-observations-never-complete-a-network-request', async () => {
  class Session extends EventEmitter {detached = false; signal(type, event) {this.emit('*', type, event)}}
  const session = new Session(), value = observer(), helper = createOwnedRPCCorrelation(origin, {nonceFactory: () => nonce(1), requestContext: value})
  const tracker = createNavigationRequestTracker(session, undefined, helper); helper.continue(request())
  session.signal('Network.requestWillBeSent', doc()); session.signal('Network.loadingFinished', {requestId: 'doc', timestamp: 1.5})
  session.signal('Network.requestWillBeSent', rpc()); session.signal('Network.responseReceivedExtraInfo', {requestId: 'rpc', headers: {[ownedRPCHeader]: nonce(1)}})
  const out = helper.requestContexts(helper.select(tracker.ownedRPCEntries()))
  assert.equal(out.targets[0].context.document.status, 'AVAILABLE')
  for (const key of ['completionGateChanged', 'qualifiesNavigation', 'responseDeliveryEstablished', 'protocolSessionIdentityEstablished']) assert.equal(out[key], false)
  assert.equal(out.additionalProtocolCommands, 0); assert.equal(out.additionalHTTPRequests, 0)
  await assert.rejects(tracker.wait(performance.now() + 10), /original navigation deadline elapsed/); assert.equal(tracker.diagnostic().activeRequests, 1)
  session.signal('Network.loadingFinished', {requestId: 'rpc', timestamp: 3}); await tracker.wait(performance.now() + 500, 1); tracker.close()
})

assert.equal(controls.length, 20)
const digest = path => createHash('sha256').update(readFileSync(new URL(path, import.meta.url))).digest('hex')
console.log('RPC-REQUEST-CONTEXT-CONTROLS ' + JSON.stringify({node: process.version, controls, controlCount: controls.length, pass: true,
  observerSourceSHA256: digest('./market-rpc-request-context.mjs'), factorySourceSHA256: digest('./market-owned-request-diagnostics.mjs'),
  correlationSourceSHA256: digest('./market-rpc-correlation-support.mjs'), navigationSourceSHA256: digest('./market-navigation-support.mjs'),
  SDKBrowserSocketOrHTTPExecutions: 0, originalContinueArgumentsAndOutcomePreserved: true,
  originalNetworkCompletionGatePreserved: true, rawPrivateValuesReported: false}))
