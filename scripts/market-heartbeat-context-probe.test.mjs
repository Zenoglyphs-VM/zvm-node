import test from 'node:test'
import assert from 'node:assert/strict'
import {createHeartbeatContextProbe} from './market-heartbeat-context-probe.mjs'

const origin = 'http://127.0.0.1:49123'
const body = {jsonrpc: '2.0', id: 7, method: 'eth_blockNumber', params: []}

function fixture({batch = false, omittedParams = false, enabled = true, responsePatch = {}, status = 200, continueError} = {}) {
  let resolve, predicate, continued = 0, waits = 0
  const responseReady = new Promise(ready => {resolve = ready})
  const request = {url: () => origin + '/rpc', method: () => 'POST', postData: () => {const value = {...body}; if (omittedParams) delete value.params; return JSON.stringify(batch ? [value] : value)}}
  const response = {request: () => request, status: () => status,
    json: async () => batch ? [{jsonrpc: '2.0', id: 7, result: '0x6', ...responsePatch}] : {jsonrpc: '2.0', id: 7, result: '0x6', ...responsePatch}}
  const page = {waitForResponse: (fn, options) => {assert.equal(options.timeout, 12000); predicate = fn; waits++; return responseReady}}
  const probe = createHeartbeatContextProbe({origin, enabled, continueRequest: async selected => {
    assert.equal(selected, request); continued++
    if (continueError) throw continueError
    if (predicate) assert.ok(predicate(response))
    resolve(response)
  }})
  return {probe, page, request, response, stats: () => ({continued, waits})}
}

for (const batch of [false, true]) for (const omittedParams of [false, true]) test('preserves and joins the exact original ' + (batch ? 'one-entry batch' : 'height request') + (omittedParams ? ' without params' : ' with empty params'), async () => {
  const f = fixture({batch, omittedParams}), ready = f.probe.arm(f.page)
  assert.equal(f.probe.intercept(f.request, f.page), true); await ready
  assert.equal(f.probe.intercept(f.request, f.page), false)
  assert.deepEqual(await f.probe.release(f.page, '0x6'), {actualOriginalHeightRequestContinued: true,
    actualMatchedHeight: '6', actualOriginalRPCBatchEntries: batch ? 1 : 0})
  assert.deepEqual(f.probe.assertIdle(), {heldOriginalRequests: 1, continuedOriginalRequests: 1, replacedRequestsOrReplies: 0})
  await f.probe.dispose(); await f.probe.dispose()
  assert.deepEqual(f.stats(), {continued: 1, waits: 1})
  assert.throws(() => f.probe.arm(f.page))
})

const refused = [
  ['another origin', {url: () => 'http://127.0.0.1:49124/rpc'}],
  ['another path', {url: () => origin + '/else'}],
  ['a query', {url: () => origin + '/rpc?other=1'}],
  ['a non-POST method', {method: () => 'GET'}],
  ['a malformed URL', {url: () => 'http://['}],
  ['missing body', {postData: () => undefined}],
  ['an oversized body', {postData: () => ' '.repeat(4097)}],
  ['invalid JSON', {postData: () => '{'}],
  ['a null entry', {postData: () => 'null'}],
  ['an empty batch', {postData: () => '[]'}],
  ['a multi-entry batch', {postData: () => JSON.stringify([body, body])}],
  ['a nested batch', {postData: () => JSON.stringify([[body]])}],
  ...[
    ['a missing ID', {id: undefined}], ['a negative ID', {id: -1}], ['a boolean ID', {id: false}],
    ['an empty string ID', {id: ''}], ['an oversized string ID', {id: 'x'.repeat(129)}],
    ['another protocol', {jsonrpc: '1.0'}], ['another RPC method', {method: 'eth_chainId'}],
    ['null parameters', {params: null}], ['unexpected parameters', {params: ['latest']}],
  ].map(([name, patch]) => [name, {postData: () => JSON.stringify({...body, ...patch})}]),
]
for (const [name, patch] of refused) test('does not capture ' + name, async () => {
  const f = fixture(); f.probe.arm(f.page)
  assert.equal(f.probe.intercept({...f.request, ...patch}, f.page), false)
  assert.equal(f.probe.intercept(f.request, {}), false)
  assert.deepEqual(f.stats(), {continued: 0, waits: 0})
  await f.probe.dispose()
})

for (const patch of [{id: 8}, {jsonrpc: '1.0'}, {error: {code: -1}}, {result: '0x7'}, {result: '0x06'}, {result: null}]) {
  test('rejects an unmatched or malformed actual response ' + JSON.stringify(patch), async () => {
    const f = fixture({responsePatch: patch}); f.probe.arm(f.page); f.probe.intercept(f.request, f.page)
    await assert.rejects(f.probe.release(f.page, '0x6'))
    assert.throws(() => f.probe.assertIdle()); assert.throws(() => f.probe.arm(f.page))
    await f.probe.dispose(); assert.equal(f.stats().continued, 1)
  })
}

test('rejects failed transport status', async () => {
  const f = fixture({status: 500}); f.probe.arm(f.page); f.probe.intercept(f.request, f.page)
  await assert.rejects(f.probe.release(f.page, '0x6')); assert.throws(() => f.probe.assertIdle())
})
test('preserves a continuation failure without a successful idle result', async () => {
  const failure = new Error('continuation failed'), f = fixture({continueError: failure})
  f.probe.arm(f.page); f.probe.intercept(f.request, f.page)
  await assert.rejects(f.probe.release(f.page, '0x6'), error => error === failure)
  await f.probe.dispose(); assert.equal(f.stats().continued, 1); assert.throws(() => f.probe.assertIdle())
})
test('cleanup continues one outstanding original request exactly once', async () => {
  const f = fixture(); f.probe.arm(f.page); f.probe.intercept(f.request, f.page)
  await f.probe.dispose(); await f.probe.dispose()
  assert.deepEqual(f.stats(), {continued: 1, waits: 0}); assert.throws(() => f.probe.assertIdle())
})
test('disabled probes cannot capture a request', () => {
  const f = fixture({enabled: false}); assert.throws(() => f.probe.arm(f.page))
  assert.equal(f.probe.intercept(f.request, f.page), false)
})
test('rejects duplicate ownership and foreign-page release before continuing', async () => {
  const f = fixture(); f.probe.arm(f.page); assert.throws(() => f.probe.arm(f.page))
  f.probe.intercept(f.request, f.page)
  await assert.rejects(f.probe.release({}, '0x6')); await assert.rejects(f.probe.release(f.page, '0x06'))
  assert.equal(f.stats().continued, 0); await f.probe.dispose(); assert.equal(f.stats().continued, 1)
})
