// Pure fake objects and emitters; no SDK, browser, protocol command or network.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {readCurrentRequestHandling, matchCurrentRequestHandling} from './market-request-handling-support.mjs'
import {readBrowserConnection} from './market-rpc-progress-support.mjs'
import {createPageClientObserver} from './market-page-client-support.mjs'
import {createNavigationRequestTracker} from './market-navigation-support.mjs'

const names = []
const control = (name, run) => {assert.ok(!names.includes(name)); run(); names.push(name)}
const id = 'owned-opaque', url = 'http://127.0.0.1:1/rpc?secret=private-query'
const entry = () => ({requestIdentity: {status: 'AVAILABLE', requestId: id}, url, method: 'POST'})
const maps = request => ({active: new Map([[request, entry()]]), recent: new Map()})
const matched = ({active, recent}) => matchCurrentRequestHandling(active, recent, id, url, 'POST')

control('current-true-and-false-states-remain-distinct', () => {
  for (const state of [true, false]) {
    const value = readCurrentRequestHandling({isInterceptResolutionHandled: () => state})
    assert.equal(value.status, 'AVAILABLE'); assert.equal(value.reason, null)
    assert.equal(value.interceptResolutionHandled, state)
  }
})
control('one-unmodified-method-call-with-original-receiver', () => {
  let calls = 0
  const request = {isInterceptResolutionHandled(...args) {assert.equal(this, request); assert.equal(args.length, 0); calls++; return false}}
  assert.equal(readCurrentRequestHandling(request).interceptResolutionHandled, false); assert.equal(calls, 1)
})
control('fresh-query-sees-post-call-state-reset', () => {
  let handled = true
  const request = {isInterceptResolutionHandled: () => handled}, state = maps(request)
  const earlier = matched(state); handled = false
  const later = matched(state)
  assert.equal(earlier.observation.interceptResolutionHandled, true)
  assert.equal(later.observation.interceptResolutionHandled, false)
  assert.equal(later.observation.protocolAcknowledgementEstablished, false)
})
control('unsupported-requests-or-methods-stay-unavailable', () => {
  for (const request of [null, undefined, 3, 'text', {}, {isInterceptResolutionHandled: true}])
    assert.equal(readCurrentRequestHandling(request).status, 'UNAVAILABLE')
})
control('non-boolean-state-is-never-coerced', () => {
  for (const value of [null, undefined, 0, 1, 'true', {}, Promise.resolve(true)]) {
    const result = readCurrentRequestHandling({isInterceptResolutionHandled: () => value})
    assert.equal(result.reason, 'NON_BOOLEAN_STATE'); assert.equal(result.interceptResolutionHandled, null)
  }
})
control('throwing-method-or-getter-never-leaks-error-values', () => {
  for (const request of [{get isInterceptResolutionHandled() {throw new Error('private-state-error')}},
    {isInterceptResolutionHandled() {throw new Error('private-state-error')}}]) {
    const result = readCurrentRequestHandling(request)
    assert.equal(result.reason, 'READ_UNAVAILABLE'); assert.ok(!JSON.stringify(result).includes('private-state-error'))
  }
})
control('fresh-read-never-reads-request-payload-or-client', () => {
  const request = {isInterceptResolutionHandled: () => true}
  for (const name of ['headers', 'postData', 'fetchPostData', 'url', 'client', 'id', '_interceptionId', 'continue', 'abort'])
    Object.defineProperty(request, name, {get() {assert.fail('sensitive or operational request property read')}})
  assert.equal(readCurrentRequestHandling(request).status, 'AVAILABLE')
})
control('same-cached-identity-url-and-method-are-required', () => {
  let calls = 0; const request = {isInterceptResolutionHandled() {calls++; return true}}, state = maps(request)
  for (const [key, value] of [['requestId', 'other'], ['url', 'other'], ['method', 'GET']]) {
    const row = state.active.get(request), original = key === 'requestId' ? row.requestIdentity[key] : row[key]
    if (key === 'requestId') row.requestIdentity[key] = value; else row[key] = value
    assert.equal(matched(state).status, 'NO_MATCH')
    if (key === 'requestId') row.requestIdentity[key] = original; else row[key] = original
  }
  assert.equal(calls, 0); assert.equal(matched(state).status, 'MATCHED'); assert.equal(calls, 1)
})
control('ambiguous-identities-are-not-read-or-selected', () => {
  const state = maps({isInterceptResolutionHandled() {assert.fail('ambiguous state read')}})
  state.recent.set({isInterceptResolutionHandled() {assert.fail('ambiguous state read')}}, entry())
  assert.equal(matched(state).status, 'AMBIGUOUS'); assert.equal(matched(state).observation, null)
})
control('recent-record-is-readable-until-evicted', () => {
  const request = {isInterceptResolutionHandled: () => true}, state = maps(request)
  state.active.delete(request); state.recent.set(request, entry())
  assert.equal(matched(state).observation.interceptResolutionHandled, true)
  state.recent.clear(); assert.equal(matched(state).status, 'NO_MATCH')
})
control('record-and-opaque-query-bounds-refuse-before-reading', () => {
  const request = {isInterceptResolutionHandled() {assert.fail('out-of-bound state read')}}, state = maps(request)
  for (let i=0; i<256; i++) state.active.set({}, {})
  assert.equal(matched(state).status, 'UNAVAILABLE'); state.active = new Map([[request, entry()]])
  for (let i=0; i<9; i++) state.recent.set({}, {})
  assert.equal(matched(state).status, 'UNAVAILABLE'); state.recent.clear()
  for (const bad of ['', 'x'.repeat(129), 'private\nvalue', null])
    assert.equal(matchCurrentRequestHandling(state.active, state.recent, bad, url, 'POST').status, 'UNAVAILABLE')
})
control('result-omits-identifiers-and-keeps-independent-snapshots', () => {
  const state = maps({isInterceptResolutionHandled: () => false}), first = matched(state)
  first.observation.interceptResolutionHandled = true
  assert.equal(matched(state).observation.interceptResolutionHandled, false)
  const text = JSON.stringify(matched(state)); for (const privateValue of [id, url, 'private-query']) assert.ok(!text.includes(privateValue))
})
control('modern-browser-connected-boolean-is-supported', () => {
  for (const value of [true, false]) assert.equal(readBrowserConnection({get connected() {return value}}), value)
})
control('modern-browser-state-precedes-legacy-method', () => {
  assert.equal(readBrowserConnection({connected: false, isConnected() {assert.fail('legacy method used over modern state')}}), false)
})
control('legacy-browser-method-retains-original-receiver', () => {
  const browser = {isConnected(...args) {assert.equal(this, browser); assert.equal(args.length, 0); return true}}
  assert.equal(readBrowserConnection(browser), true)
})
control('unsupported-browser-values-remain-null', () => {
  for (const browser of [null, undefined, {}, {connected: 'true'}, {connected: Promise.resolve(true)}, {isConnected: () => 1}])
    assert.equal(readBrowserConnection(browser), null)
})
control('throwing-browser-access-is-bounded-and-not-logged', () => {
  for (const browser of [{get connected() {throw new Error('private-browser-error')}},
    {get isConnected() {throw new Error('private-browser-error')}}, {isConnected() {throw new Error('private-browser-error')}}])
    assert.equal(readBrowserConnection(browser), null)
})
control('available-page-client-reason-remains-null', () => {
  const active = new Map(), recent = new Map(), client = new EventEmitter(), row = entry()
  const request = {client}; active.set(request, row)
  const observer = createPageClientObserver(active, recent)
  try {observer.observe(request, row); const value = observer.matches(id, url, 'POST').observations[0]
    assert.equal(value.status, 'AVAILABLE'); assert.equal(value.reason, null)
  } finally {observer.close()}
})
control('unavailable-page-client-retains-its-real-reason', () => {
  const active = new Map(), recent = new Map(), row = entry(), request = {client: null}
  active.set(request, row); const observer = createPageClientObserver(active, recent)
  try {observer.observe(request, row); assert.equal(observer.matches(id, url, 'POST').observations[0].reason, 'CLIENT_UNAVAILABLE')}
  finally {observer.close()}
})
control('fresh-false-state-never-completes-the-original-network-gate', () => {
  const session = new EventEmitter(), page = new EventEmitter(); session.detached = false
  const tracker = createNavigationRequestTracker(session, page), client = new EventEmitter()
  const request = {id, client, frame: () => ({page: () => page}), url: () => url, method: () => 'POST', isInterceptResolutionHandled: () => false}
  try {
    session.emit('*', 'Network.requestWillBeSent', {requestId: id, timestamp: 1, type: 'Fetch', request: {url, method: 'POST'}})
    page.emit('request', request)
    const observed = tracker.diagnostic()
    assert.equal(observed.requests[0].pageRequestHandlingMatches.observation.interceptResolutionHandled, false)
    assert.equal(observed.activeRequests, 1); assert.equal(observed.completed, 0)
    assert.equal(observed.requests[0].pageRequestHandlingMatches.completionGateChanged, false)
  } finally {tracker.close()}
  assert.equal(client.eventNames().length, 0)
})

assert.equal(names.length, 20)
console.log('NAVIGATION-HANDLING-STATE-CONTROLS '+JSON.stringify({controls: names.length, pass: true,
  SDKOrBrowserExecutions: 0, networkRequests: 0, protocolCommands: 0, completionGateChanged: false,
  rawIdentifiersLogged: false, onlyCurrentSDKBooleanRead: true, methodFulfillmentIsProtocolAcknowledgement: false,
  modernAndLegacyBrowserConnectionSupported: true, availableReasonPreservesNull: true}))
