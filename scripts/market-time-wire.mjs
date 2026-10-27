import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {readFileSync} from 'node:fs'
import {createRequire, stripTypeScriptTypes} from 'node:module'
import {resolve} from 'node:path'
import {spawnSync} from 'node:child_process'
import {createContext, SourceTextModule, SyntheticModule} from 'node:vm'

let bytes = 0
const chunks = []
for await (const chunk of process.stdin) {
  bytes += chunk.length
  assert.ok(bytes <= 128 << 10, 'bounded time-wire fixture input required')
  chunks.push(chunk)
}
const fixture = JSON.parse(Buffer.concat(chunks).toString())
assert.ok(['nft', 'karum'].includes(fixture.app))
assert.ok(['single', 'bulk'].includes(fixture.kind))
const base = new URL(fixture.base)
assert.equal(base.protocol, 'http:')
assert.ok(['127.0.0.1', '[::1]'].includes(base.hostname))
assert.equal(base.username + base.password + base.search + base.hash, '')
const root = resolve(fixture.root)
const pins = JSON.parse(readFileSync(new URL('./market-browser-pins.json', import.meta.url)))
const head = spawnSync('git', ['rev-parse', 'HEAD'], {cwd: root, encoding: 'utf8', timeout: 5000, maxBuffer: 4096})
assert.equal(head.status, 0)
assert.equal(head.stdout.trim(), pins['zvm-' + fixture.app])
const requireApp = createRequire(resolve(root, 'package.json'))
const sdk = requireApp('viem')
const lock = JSON.parse(readFileSync(resolve(root, 'package-lock.json')))
const SDKVersion = JSON.parse(readFileSync(resolve(root, 'node_modules/viem/package.json'))).version
assert.equal(SDKVersion, '2.56.8')
assert.equal(lock.packages['node_modules/viem'].version, SDKVersion)
// Deliberately public disposable scalar-one fixture account.
const signer = requireApp('viem/accounts').privateKeyToAccount('0x' + '0'.repeat(63) + '1')
assert.equal(signer.address.toLowerCase(), fixture.seller.toLowerCase())
const zero = '0x' + '0'.repeat(40)
const info = {l2: {chainId: fixture.chainId}, apps: {exchange: fixture.exchange, wrappedZnn: zero}}
const wallet = {info, evm: {address: signer.address, provider: {}}, address: signer.address, provider: {}}
const endpoint = {info, chainId: fixture.chainId}
const posts = [], replies = [], digests = []
let reads = 0
const request = async (path, body) => {
  const response = await fetch(new URL(path, base), {method: body === undefined ? 'GET' : 'POST',
    headers: {'Content-Type': 'application/json'}, body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(10000)})
  const text = await response.text()
  assert.ok(text.length <= 128 << 10, 'bounded real API response required')
  return {status: response.status, body: JSON.parse(text)}
}
const api = {
  submitOrders: async (inputs) => {
    posts.push(JSON.parse(JSON.stringify(inputs)))
    const reply = await request('/api/nft/orders', {orders: inputs})
    assert.equal(reply.status, 200)
    replies.push(reply.body)
    return reply.body
  },
  order: async (hash) => {
    reads++
    const reply = await request('/api/nft/orders/' + hash)
    assert.equal(reply.status, 200)
    return reply.body
  },
}
const forbidden = () => {throw new Error('undeclared time-wire capability')}
const evm = {publicClient: forbidden, walletClient: () => ({signTypedData: async (value) => {
  digests.push(sdk.hashTypedData(value))
  return signer.signTypedData(value)
}})}
const modules = new Map([
  ['viem', {concat: sdk.concat, encodeFunctionData: sdk.encodeFunctionData, hashStruct: sdk.hashStruct,
    keccak256: sdk.keccak256, parseAbi: sdk.parseAbi}],
  ['./api', {api}], ['./evm', evm], ['@zvm/web/evm', evm], ['./wallet', {wallet}],
  ['./endpoint', {endpoint}], ['./format', {ZERO: zero}],
])
const source = readFileSync(resolve(root, 'src/core/orders.ts'), 'utf8')
const context = createContext({console: {log: forbidden}, fetch: forbidden})
const program = new SourceTextModule(stripTypeScriptTypes(source, {mode: 'strip'}), {context})
await program.link(specifier => {
  assert.ok(modules.has(specifier), 'undeclared actual order-module import')
  const exports = modules.get(specifier)
  return new SyntheticModule(Object.keys(exports), function () {
    for (const [key, value] of Object.entries(exports)) this.setExport(key, value)
  }, {context})
})
await program.evaluate({timeout: 5000})
const orders = fixture.orders.map(program.namespace.fromWire)
assert.equal(orders.length, 4)
const inputs = fixture.kind === 'bulk' ? await program.namespace.signOrders(orders) :
  await Promise.all(orders.map(program.namespace.signOrder))
await program.namespace.submitOrders(inputs)
assert.equal(posts.length, 1)
assert.ok(replies[0].results.every(row => row.ok && row.code === 0))
const hashes = orders.map(program.namespace.orderHash)
const abiTimes = []
for (let i = 0; i < orders.length; i++) {
  assert.equal(String(posts[0][i].order.expirationTime), String(fixture.orders[i].expirationTime))
  if (orders[i].expirationTime > 9007199254740991n) assert.equal(typeof posts[0][i].order.expirationTime, 'string')
  const returned = await program.namespace.fetchInput(hashes[i])
  assert.equal(program.namespace.orderHash(returned.order), hashes[i])
  assert.equal(returned.order.expirationTime, orders[i].expirationTime)
  assert.equal(returned.signature, inputs[i].signature)
  assert.deepEqual([...returned.proof], [...inputs[i].proof])
  const call = fixture.app === 'nft'
    ? program.namespace.buyCall(fixture.exchange, returned, 'Read-only time-wire encoding')
    : program.namespace.buyCall(returned, 'Read-only time-wire encoding')
  assert.equal(call.to.toLowerCase(), fixture.exchange.toLowerCase())
  assert.equal(call.value, returned.order.price)
  const decoded = sdk.decodeFunctionData({abi: program.namespace.exchangeAbi, data: call.data})
  assert.equal(decoded.functionName, 'buy')
  assert.equal(BigInt(decoded.args[0].order.listingTime), orders[i].listingTime)
  assert.equal(BigInt(decoded.args[0].order.expirationTime), orders[i].expirationTime)
  abiTimes.push(String(decoded.args[0].order.expirationTime))
}

// A future full-width listing reaches the actual exchange and is refused as
// not started. Its original signed bytes are not rounded into an earlier time.
const future = program.namespace.fromWire({...fixture.orders[0], listingTime: '9007199254740993',
  expirationTime: '18446744073709551615', salt: '10000'})
const futureInput = await program.namespace.signOrder(future)
await assert.rejects(program.namespace.submitOrders([futureInput]), /not open yet/)
assert.equal(replies[1].results[0].code, 4)
assert.equal(replies[1].results[0].ok, false)
const rounded = JSON.parse(JSON.stringify(posts[0][0]))
rounded.order.expirationTime = JSON.parse('9007199254740993')
const rejected = await request('/api/nft/orders', {orders: [rounded]})
assert.equal(rejected.status, 400)
assert.match(rejected.body.error, /requires a decimal string/)

console.log('MARKET-TIME-WIRE-CLIENT ' + JSON.stringify({app: fixture.app, kind: fixture.kind, pass: true,
  head: head.stdout.trim(), sourceSha256: createHash('sha256').update(source).digest('hex'),
  node: process.version, SDKVersion, pins, hashes, digests, abiTimes,
  signedInputs: posts[0], futureInput: posts[1][0], actualTypedDataSignatures: digests.length,
  actualOrderPOSTs: 2, successfulRealOrderReads: reads, actualNodeAPIAndExchangeChecks: true,
  futureOrderCode: 4, unsafeNumericHTTPStatus: rejected.status,
  actualPublicTestSignatures: true, actualEVMTransactions: 0, browserExecutions: 0,
  physicalWalletOrPublicNetwork: false}))
