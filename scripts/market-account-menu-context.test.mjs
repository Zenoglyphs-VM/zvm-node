// Memory-only capabilities test request selection and exact unchanged continuation.
import assert from 'node:assert/strict'
import {createAccountMenuContextProbe} from './market-account-menu-context.mjs'
const origin = 'http://127.0.0.1:12345', owner = '0x'+'1'.repeat(40), buyer = '0x'+'2'.repeat(40)
let cases = 0
const check = async operation => {await operation(); cases++}
const request = (overrides = {}, body = {}) => ({url: () => origin+'/rpc', method: () => 'POST',
  postData: () => JSON.stringify({jsonrpc: '2.0', id: 7, method: 'eth_getBalance', params: [owner, 'latest'], ...body}), ...overrides})
function fixture({result = '0x0', envelope = {}, status = 200, otherRequest = false, failContinue = false, batch = false, responseShape = 'auto'} = {}) {
  const continued = [], single = request(), req = batch ? request({postData: () => JSON.stringify([JSON.parse(single.postData())])}) : single
  const page = {waitForResponse: async predicate => {
    const response = {request: () => otherRequest ? request() : req, status: () => status,
      json: async () => {
        const value = {jsonrpc: '2.0', id: 7, result, ...envelope}
        return responseShape === 'two' ? [value, value] : (responseShape === 'one' || (responseShape === 'auto' && batch)) ? [value] : value
      }}
    if (!otherRequest) assert.ok(predicate(response))
    return response
  }}
  const probe = createAccountMenuContextProbe({origin, enabled: true, continueRequest: async value => {
    continued.push(value); if (failContinue) throw new Error('actual continuation failed')
  }})
  return {probe, page, req, continued}
}
for (const value of ['https://127.0.0.1:12345', 'http://example.test:12345', 'http://127.0.0.1',
  origin+'/rpc', 'http://user:pass@127.0.0.1:12345', origin+'/?x=1', origin+'/#x']) {
  await check(() => assert.throws(() => createAccountMenuContextProbe({origin: value, enabled: true, continueRequest() {}})))
}
await check(() => assert.throws(() => createAccountMenuContextProbe({origin, enabled: 1, continueRequest() {}})))
await check(() => assert.throws(() => createAccountMenuContextProbe({origin, enabled: true, continueRequest: null})))
await check(() => {
  const probe = createAccountMenuContextProbe({origin, enabled: false, continueRequest() {assert.fail('inactive continuation')}})
  assert.equal(probe.intercept({url() {assert.fail('inactive request was inspected')}}, {}), false)
  assert.throws(() => probe.arm({}, owner))
})
for (const bad of ['', '0x00', null, owner+'0']) await check(() => {const {probe, page} = fixture(); assert.throws(() => probe.arm(page, bad))})
await check(() => {const {probe, page} = fixture(); probe.arm(page, owner); assert.throws(() => probe.arm(page, owner)); assert.throws(() => probe.assertIdle())})
const ignored = [
  request({url: () => 'http://127.0.0.1:54321/rpc'}), request({url: () => origin+'/other'}),
  request({url: () => origin+'/rpc?other=1'}), request({method: () => 'GET'}),
  request({postData: () => '{'}), request({postData: () => 'x'.repeat(4097)}), request({postData: () => null}),
  request({postData: () => '[{}]'}), request({}, {jsonrpc: '1.0'}), request({}, {id: null}),
  request({}, {id: -1}), request({}, {id: 1.5}), request({}, {id: ''}), request({}, {id: 'x'.repeat(129)}),
  request({}, {method: 'eth_call'}), request({}, {params: [buyer, 'latest']}),
  request({}, {params: [owner, 'pending']}), request({}, {params: [owner]}), request({}, {params: [owner, 'latest', 1]}),
  request({postData() {throw new Error('unavailable metadata')}}),
  request({postData: () => JSON.stringify([JSON.parse(request().postData()), JSON.parse(request().postData())])}),
]
for (const req of ignored) await check(async () => {
  const {probe, page, req: actual, continued} = fixture(), ready = probe.arm(page, owner)
  assert.equal(probe.intercept(req, page), false); assert.equal(continued.length, 0)
  assert.equal(probe.intercept(actual, {}), false); assert.equal(probe.intercept(actual, page), true)
  await ready; await probe.release(page, '0x0'); assert.deepEqual(continued, [actual])
})
for (const options of [{status: 500}, {result: '0x1'}, {result: '-1'}, {result: '0x00'},
  {envelope: {jsonrpc: '1.0'}}, {envelope: {id: 8}}, {envelope: {error: {code: -1}}},
  {otherRequest: true}, {failContinue: true}]) await check(async () => {
  const {probe, page, req, continued} = fixture(options), ready = probe.arm(page, owner)
  assert.equal(probe.intercept(req, page), true); await ready
  await assert.rejects(probe.release(page, '0x0'))
  assert.deepEqual(continued, [req]); assert.throws(() => probe.arm(page, owner)); assert.throws(() => probe.assertIdle())
  await assert.rejects(probe.release(page, '0x0')); assert.equal(continued.length, 1)
})
await check(async () => {
  const continued = []
  let selected, expectedResult
  const page = {waitForResponse: async predicate => {
    const response = {request: () => selected, status: () => 200,
      json: async () => ({jsonrpc: '2.0', id: 7, result: expectedResult})}
    assert.ok(predicate(response)); return response
  }}
  const probe = createAccountMenuContextProbe({origin, enabled: true, continueRequest: async req => {continued.push(req)}})
  let prior
  for (let i = 0; i < 2; i++) {
    const req = request(), raw = req.postData()
    selected = req; expectedResult = i === 0 ? '0x0' : '0xde0b6b3a7640000'
    const ready = probe.arm(page, owner)
    if (prior) assert.equal(probe.intercept(prior, page), false)
    assert.equal(probe.intercept(req, page), true); assert.equal(probe.intercept(req, page), false)
    await ready; assert.throws(() => probe.arm(page, owner))
    const actual = await probe.release(page, expectedResult)
    assert.deepEqual(actual, {actualOriginalRequestContinued: true, actualMatchedResponseAmount: BigInt(expectedResult).toString(), actualOriginalRPCBatchEntries: 0})
    assert.equal(req.postData(), raw)
    assert.equal(continued[i], req); prior = req
  }
  assert.equal(continued.length, 2); assert.notEqual(continued[0], continued[1])
  assert.deepEqual(probe.assertIdle(), {heldOriginalRequests: 2, continuedOriginalRequests: 2, replacedRequestsOrReplies: 0})
  assert.throws(() => probe.arm(page, owner))
})
for (const result of ['0x0', '0xde0b6b3a7640000']) await check(async () => {
  const {probe, page, req, continued} = fixture({batch: true, result}), raw = req.postData(), ready = probe.arm(page, owner)
  assert.equal(probe.intercept(req, page), true); await ready
  assert.deepEqual(await probe.release(page, result), {actualOriginalRequestContinued: true,
    actualMatchedResponseAmount: BigInt(result).toString(), actualOriginalRPCBatchEntries: 1})
  assert.deepEqual(continued, [req]); assert.equal(req.postData(), raw)
})
for (const options of [{batch: true, responseShape: 'object'}, {batch: true, responseShape: 'two'},
  {batch: true, envelope: {id: 8}}, {batch: true, envelope: {error: {code: -1}}},
  {responseShape: 'one'}]) await check(async () => {
  const {probe, page, req, continued} = fixture(options), ready = probe.arm(page, owner)
  assert.equal(probe.intercept(req, page), true); await ready
  await assert.rejects(probe.release(page, '0x0'))
  assert.deepEqual(continued, [req]); assert.throws(() => probe.arm(page, owner))
  await assert.rejects(probe.release(page, '0x0')); assert.equal(continued.length, 1)
})
console.log('ACCOUNT-MENU-CONTEXT-CONTROLS '+JSON.stringify({cases, pass: true, browserExecutions: 0,
  originalFinancialProfileChecksChanged: false, productAPIOrRPCRepliesReplaced: 0}))
