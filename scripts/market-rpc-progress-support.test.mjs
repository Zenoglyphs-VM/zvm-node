import assert from 'node:assert/strict'
import {readOwnedRPCProgress, validateOwnedRPCSnapshot, RPC_PROGRESS_BODY_LIMIT, RPC_PROGRESS_TIMEOUT} from './market-rpc-progress-support.mjs'

const base = 'http://127.0.0.1:12345', token = 'a'.repeat(64)
const sample = () => ({version: 1, scope: 'owned-loopback-POST-rpc-handler-lifecycle', maximumTrackedActive: 32, maximumRecent: 8,
  started: 2, finished: 1, normalReturns: 1, abnormalExits: 0, active: 1, overflowStarted: 0, untrackedActive: 0,
  completeTracking: true, activeRecords: [{sequence: 2, ageMilliseconds: 3, contextCanceled: false}],
  recent: [{sequence: 1, durationMilliseconds: 2, returnedNormally: true, contextCanceled: false}],
  HTTPStatusBodyOrDeliveryObserved: false, protocolRequestIdentityEstablished: false, responseWriterOrRequestReplaced: false})
const response = value => new Response(JSON.stringify(value), {headers: {'content-type': 'application/json'}})
let controls = 0
const check = async action => {await action(); controls++}

await check(async () => {
  let calls = 0
  const got = await readOwnedRPCProgress(base, token, {fetchImpl: async (url, options) => {
    calls++; assert.equal(url.href, base + '/__fixture/rpc-progress'); assert.equal(options.method, 'GET')
    assert.equal(options.redirect, 'error'); assert.deepEqual(options.headers, {'X-ZVM-Fixture': token})
    assert.equal(Object.hasOwn(options, 'body'), false); assert.equal(options.signal.aborted, false)
    return response(sample())
  }})
  assert.equal(calls, 1); assert.equal(got.status, 'AVAILABLE'); assert.equal(got.qualifiesNavigation, false)
  assert.deepEqual(got.snapshot, sample())
  assert.equal(JSON.stringify(got).includes(token), false)
})
for (const input of ['https://127.0.0.1:12345', 'http://example.invalid:12345', 'http://127.0.0.1',
  'http://user:password@127.0.0.1:12345', base + '/other', base + '?private=yes', base + '#private', 'invalid']) {
  await check(async () => {
    let calls = 0
    const value = await readOwnedRPCProgress(input, token, {fetchImpl: async () => {calls++; throw new Error('must not execute')}})
    assert.equal(calls, 0); assert.equal(value.reason, 'CONFIGURATION')
  })
}
for (const config of [{capability: 'private-invalid'}, {timeoutMilliseconds: 0}, {timeoutMilliseconds: RPC_PROGRESS_TIMEOUT + 1}]) {
  await check(async () => {
    let calls = 0
    const value = await readOwnedRPCProgress(base, config.capability ?? token, {...config, fetchImpl: async () => {calls++; throw new Error('must not execute')}})
    assert.equal(calls, 0); assert.equal(value.reason, 'CONFIGURATION')
  })
}
for (const change of [value => {value.protocolRequestIdentityEstablished = true}, value => {value.HTTPStatusBodyOrDeliveryObserved = true},
  value => {value.responseWriterOrRequestReplaced = true}, value => {value.active = 2}, value => {value.finished = 3},
  value => {value.recent[0].sequence = 2}, value => {value.activeRecords[0].ageMilliseconds = -1},
  value => {value.extra = 'private-response'}, value => {value.maximumTrackedActive = 33},
  value => {value.completeTracking = false}]) {
  await check(async () => {
    const value = sample(); change(value)
    assert.throws(() => validateOwnedRPCSnapshot(value))
    const result = await readOwnedRPCProgress(base, token, {fetchImpl: async () => response(value)})
    assert.equal(result.status, 'UNAVAILABLE'); assert.equal(result.qualifiesNavigation, false)
    assert.equal(JSON.stringify(result).includes('private-response'), false)
  })
}
await check(async () => {
  const value = sample(); value.started = 3; value.active = 2; value.overflowStarted = 1; value.untrackedActive = 1; value.completeTracking = false
  validateOwnedRPCSnapshot(value)
  const result = await readOwnedRPCProgress(base, token, {fetchImpl: async () => response(value)})
  assert.equal(result.status, 'AVAILABLE'); assert.equal(result.snapshot.completeTracking, false); assert.equal(result.qualifiesNavigation, false)
})
await check(async () => {
  const value = await readOwnedRPCProgress(base, token, {fetchImpl: async () => new Response('private', {status: 403})})
  assert.equal(value.reason, 'HTTP_STATUS'); assert.equal(JSON.stringify(value).includes('private'), false)
})
await check(async () => {
  const value = await readOwnedRPCProgress(base, token, {fetchImpl: async () => new Response('private', {headers: {'content-type': 'text/plain'}})})
  assert.equal(value.reason, 'CONTENT_TYPE')
})
await check(async () => {
  const value = await readOwnedRPCProgress(base, token, {fetchImpl: async () => new Response('x'.repeat(RPC_PROGRESS_BODY_LIMIT + 1), {headers: {'content-type': 'application/json'}})})
  assert.equal(value.reason, 'BODY_LIMIT')
})
await check(async () => {
  const value = await readOwnedRPCProgress(base, token, {fetchImpl: async () => {throw new Error(token + ' private URL')}})
  assert.equal(value.status, 'UNAVAILABLE'); assert.equal(JSON.stringify(value).includes(token), false)
})
await check(async () => {
  let signal
  const value = await readOwnedRPCProgress(base, token, {timeoutMilliseconds: 2, fetchImpl: async (_, options) => {
    signal = options.signal; return new Promise(() => {})
  }})
  assert.equal(value.reason, 'DEADLINE'); assert.equal(signal.aborted, true)
})
assert.equal(controls, 28)
console.log('OWNED-RPC-PROGRESS-CONTROLS ' + JSON.stringify({pass: true, controls, SDKOrBrowserExecutions: 0,
  networkRequests: 0, maximumDiagnosticMilliseconds: RPC_PROGRESS_TIMEOUT, maximumBodyBytes: RPC_PROGRESS_BODY_LIMIT,
  originalNavigationGateChanged: false, privateMaterialReported: false}))
