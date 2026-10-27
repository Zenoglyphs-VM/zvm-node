import assert from 'node:assert/strict'
import test from 'node:test'
import {stripTypeScriptTypes} from 'node:module'
import {createContext, SourceTextModule, SyntheticModule} from 'node:vm'
import {createAccount, createAddressFromString} from '@ethereumjs/util'
import {artifacts, INITIAL, START, world, ZERO} from './evm.mjs'
import {loadMarketInputs, manifest} from './market-inputs.mjs'

const inputs = loadMarketInputs()
const plain = (value) => JSON.parse(JSON.stringify(value, (_, v) => typeof v === 'bigint' ? v.toString() : v))

async function adapter(name, f, {exchange = f.exchange, account, contractSignature} = {}) {
  const selected = inputs.frontends[name], sdk = selected.sdk
  // The same public scalar-one offline fixture used by evm.mjs. It has no
  // wallet file, RPC connection, real transaction signing or real funds.
  const offline = selected.account('0x' + '00'.repeat(31) + '01')
  const trader = account || offline.address
  const info = {l2: {chainId: f.chainId}, apps: {exchange, wrappedZnn: f.wznn}}
  const wallet = {info, address: trader, provider: {}, evm: {address: trader, provider: {}}}
  const endpoint = {info, chainId: f.chainId}
  const prompts = [], book = new Map()
  const wire = (o) => Object.fromEntries(Object.entries(o).map(([k, v]) => [k,
    typeof v === 'bigint' ? (['listingTime', 'expirationTime'].includes(k) ? Number(v) : v.toString()) : v]))
  const forbidden = () => {throw new Error('unexpected external capability')}
  let ui
  const api = {
    submitOrders: async (items) => {
      for (const input of items) book.set(ui.orderHash(ui.fromWire(input.order)).toLowerCase(), input)
      return {results: items.map(() => ({ok: true}))}
    },
    order: async (hash) => {
      const input = book.get(hash.toLowerCase())
      if (!input) return {fillable: false, reason: 'missing fixture row'}
      const [code] = await f.read(exchange, 'checkOrder', [{...input, order: ui.fromWire(input.order)}])
      return {fillable: code === 0, reason: 'EVM checkOrder code ' + code, input}
    },
  }
  const evm = {
    publicClient: (chainId) => {
      assert.equal(chainId, f.chainId)
      return {readContract: ({address, functionName, args = []}) => f.read(address, functionName, args)}
    },
    walletClient: (_provider, chainId, from) => {
      assert.equal(chainId, f.chainId)
      assert.equal(from, trader)
      return {signTypedData: async (request) => {
        prompts.push(request)
        assert.equal(request.account, trader)
        return contractSignature || offline.signTypedData(request)
      }}
    },
  }
  const modules = new Map([
    ['viem', Object.fromEntries(['concat', 'encodeFunctionData', 'hashStruct', 'keccak256', 'parseAbi'].map((k) => [k, sdk[k]]))],
    ['./api', {api}], ['./evm', evm], ['@zvm/web/evm', evm], ['./wallet', {wallet}],
    ['./endpoint', {endpoint}], ['./format', {ZERO}],
  ])
  class Clock extends Date {static now() {return Number(f.now * 1000n)}}
  let salt = 0
  const context = createContext({console, fetch: forbidden, Date: Clock,
    crypto: {getRandomValues(bytes) {bytes.fill(++salt); return bytes}}})
  const program = new SourceTextModule(stripTypeScriptTypes(selected.source, {mode: 'strip'}), {context})
  await program.link((specifier) => {
    assert.ok(modules.has(specifier), 'unexpected UI import: ' + specifier)
    const exports = modules.get(specifier)
    return new SyntheticModule(Object.keys(exports), function () {
      for (const [key, value] of Object.entries(exports)) this.setExport(key, value)
    }, {context})
  })
  await program.evaluate({timeout: 5000})
  ui = program.namespace
  const call = (method, ...args) => name === 'zvm-nft' ? ui[method](exchange, ...args) : ui[method](...args)
  const send = (from, request, options = {}) => f.frame({from, to: request.to, data: request.data, value: request.value, ...options})
  const remember = async (items) => {
    await ui.submitOrders(items)
    return items.map((i) => ui.orderHash(i.order))
  }
  return {ui, sdk, prompts, wallet, endpoint, call, send, remember, wire,
    buy: (input) => call('buyCall', input, 'Buy fixture'),
    many: (items, value) => call('buyManyCall', items, value),
    accept: (input, id) => call('acceptBidCall', input, id, 'Accept fixture bid'),
    cancel: (orders) => call('cancelOrdersCall', orders),
    cancelAll: () => call('incrementNonceCall')}
}

async function market() {
  const f = await world(), collection = await f.collection()
  f.nft = collection.collection
  f.exchange = await f.deploy('ZVMExchange', [f.owner, f.revenue, 250n])
  f.currency = await f.deploy('BenchERC20', ['Interface fixture', 'FIX', 10n ** 24n])
  f.wznn = await f.deploy('WZNN')
  await f.write(f.alice, f.nft, 'setRoyalty', [f.royalty, 500])
  await f.write(f.signer, f.nft, 'setApprovalForAll', [f.exchange, true])
  f.mint = async (to = f.signer) => {
    const id = await f.read(f.nft, 'nextTokenId')
    await f.write(f.alice, f.nft, 'mint', [to, ''])
    return id
  }
  f.order = (id, overrides = {}) => ({trader: f.signer, side: 0, collection: f.nft, tokenId: id, anyToken: false,
    amount: 1n, paymentToken: ZERO, price: 10_000n, listingTime: 0n, expirationTime: START + 1000n,
    nonce: 0n, salt: id, ...overrides})
  return f
}

for (const name of Object.keys(inputs.frontends)) {
  test(name + ': exact Go golden order/root hashes and digests execute in Solidity', async () => {
    const vectors = inputs.vectors, f = await world({chainId: vectors.chainId})
    const deployed = await f.deploy('ZVMExchange', [f.owner, f.revenue, 250n])
    // Relocate the actual freshly constructed runtime for a read-only golden
    // domain fixture. EIP-712 rebuilds its domain when cachedThis differs. No
    // owner/storage, sale or deployment provenance is asserted at this address.
    const address = createAddressFromString(vectors.exchange)
    await f.vm.stateManager.putAccount(address, createAccount({nonce: 1n}))
    await f.vm.stateManager.putCode(address, await f.vm.stateManager.getCode(createAddressFromString(deployed)))
    f.register(vectors.exchange, artifacts.ZVMExchange.abi)
    const a = await adapter(name, f, {exchange: vectors.exchange})
    for (const vector of vectors.orders) {
      const order = a.ui.fromWire(vector.order)
      assert.equal(a.ui.orderHash(order), vector.hash)
      assert.equal(await f.read(vectors.exchange, 'hashOrder', [order]), vector.hash)
      assert.equal(await f.read(vectors.exchange, 'orderDigest', [vector.hash]), vector.digest)
      assert.equal(a.sdk.hashTypedData({domain: {name: 'ZVM Exchange', version: '1', chainId: vectors.chainId,
        verifyingContract: vectors.exchange}, types: a.ui.orderTypes, primaryType: 'Order', message: order}), vector.digest)
    }
    const tree = a.ui.orderTree(vectors.bulk.leaves)
    assert.equal(tree.root, vectors.bulk.root)
    assert.deepEqual(plain(tree.proofs), vectors.bulk.proofs)
    assert.equal(await f.read(vectors.exchange, 'rootDigest', [tree.root]), vectors.bulk.digest)
    await f.conserved()
  })

  test(name + ': actual single signature and buy adapter conserve exact native seller, royalty and fee value', async () => {
    const f = await market(), a = await adapter(name, f), id = await f.mint()
    const input = await a.ui.signOrder(f.order(id))
    assert.deepEqual(await f.read(f.exchange, 'checkOrder', [input]), [0, 1n])
    assert.equal(a.prompts.length, 1)
    const result = await a.send(f.bob, a.buy(input))
    assert.equal(await f.read(f.nft, 'ownerOf', [id]), f.bob)
    for (const [at, value] of [[f.bob, INITIAL - 10_000n], [f.signer, INITIAL + 9250n],
      [f.royalty, INITIAL + 500n], [f.revenue, INITIAL + 250n]]) assert.equal(await f.balance(at), value)
    const event = f.events(result, f.exchange).find((e) => e.eventName === 'OrderFilled').args
    assert.equal(event.orderHash, a.ui.orderHash(input.order))
    assert.equal(event.royalty, 500n)
    assert.equal(event.fee, 250n)
    await f.conserved()
  })

  test(name + ': odd bulk signature, cancellation, aggregate rollback and exact sweep refund execute actual adapter calldata', async () => {
    const f = await market(), a = await adapter(name, f), ids = [await f.mint(), await f.mint(), await f.mint()]
    const orders = ids.map((id, i) => f.order(id, {price: 10_000n + BigInt(i) * 1000n}))
    const items = await a.ui.signOrders(orders), hashes = await a.remember(items)
    assert.equal(a.prompts.length, 1)
    assert.equal(await f.read(f.exchange, 'rootDigest', [a.ui.orderTree(orders.map(a.ui.orderHash)).root]), a.sdk.hashTypedData(a.prompts[0]))
    await a.send(f.signer, a.cancel([orders[1]]))
    const book = await a.ui.fetchFillable(hashes), fillable = name === 'zvm-nft' ? book : book.inputs
    assert.equal(fillable.length, 2)
    if (name === 'zvm-karum') assert.deepEqual(plain(book.gone), [hashes[1]])
    await a.send(f.bob, a.many(items, 21_999n), {error: 'WrongPayment'})
    for (const id of ids) assert.equal(await f.read(f.nft, 'ownerOf', [id]), f.signer)
    const result = await a.send(f.bob, a.many(items, 25_000n))
    assert.equal(a.sdk.decodeFunctionResult({abi: a.ui.exchangeAbi, functionName: 'buyMany', data: result.returned}), 2n)
    assert.equal(await f.balance(f.bob), INITIAL - 22_000n)
    assert.equal(await f.balance(f.signer), INITIAL + 20_350n)
    assert.equal(await f.balance(f.royalty), INITIAL + 1100n)
    assert.equal(await f.balance(f.revenue), INITIAL + 550n)
    assert.equal(await f.balance(f.exchange), 0n)
    for (let i = 0; i < ids.length; i++) assert.equal(await f.read(f.nft, 'ownerOf', [ids[i]]), i === 1 ? f.signer : f.bob)
    await f.conserved()
  })

  test(name + ': ERC20 buy adapter refuses a late allowance shortfall and then settles exact token amounts', async () => {
    const f = await market(), a = await adapter(name, f), id = await f.mint()
    const input = await a.ui.signOrder(f.order(id, {paymentToken: f.currency}))
    await f.write(f.owner, f.currency, 'transfer', [f.bob, 10_000n])
    await f.write(f.bob, f.currency, 'approve', [f.exchange, 9999n])
    assert.equal(a.buy(input).value, 0n)
    await a.send(f.bob, a.buy(input), {error: 'ERC20InsufficientAllowance'})
    assert.equal(await f.read(f.nft, 'ownerOf', [id]), f.signer)
    await f.write(f.bob, f.currency, 'approve', [f.exchange, 10_000n])
    await a.send(f.bob, a.buy(input))
    for (const [at, amount] of [[f.bob, 0n], [f.signer, 9250n], [f.royalty, 500n], [f.revenue, 250n]])
      assert.equal(await f.read(f.currency, 'balanceOf', [at]), amount)
    assert.equal(await f.read(f.currency, 'allowance', [f.bob, f.exchange]), 0n)
    await f.conserved([f.currency])
  })

  test(name + ': ERC1271 input survives actual signing/ABI adapters and only its approved digest fills', async () => {
    const f = await market(), wallet = await f.deploy('ContractSigner'), id = await f.mint(wallet)
    await f.forward(wallet, f.nft, 'setApprovalForAll', [f.exchange, true])
    const a = await adapter(name, f, {account: wallet, contractSignature: '0x1122'})
    const order = f.order(id, {trader: wallet}), input = await a.ui.signOrder(order)
    await f.write(f.owner, wallet, 'setDigest', [await f.read(f.exchange, 'orderDigest', [a.ui.orderHash(order)])])
    assert.deepEqual(await f.read(f.exchange, 'checkOrder', [input]), [0, 1n])
    const changed = {...input, order: {...order, price: 1n}}
    assert.equal((await f.read(f.exchange, 'checkOrder', [changed]))[0], 1)
    await a.send(f.bob, a.buy(changed), {error: true})
    await a.send(f.bob, a.buy(input))
    assert.equal(await f.read(f.nft, 'ownerOf', [id]), f.bob)
    await f.conserved()
  })

  test(name + ': per-order and nonce-wide cancellation invalidate the actual adapter book inputs', async () => {
    const f = await market(), a = await adapter(name, f), id = await f.mint()
    const input = await a.ui.signOrder(f.order(id)), [hash] = await a.remember([input])
    await a.send(f.signer, a.cancel([input.order]))
    await assert.rejects(() => a.ui.fetchInput(hash), /EVM checkOrder/)
    const next = await a.ui.signOrder(f.order(id, {salt: 100n})), [nextHash] = await a.remember([next])
    await a.send(f.signer, a.cancelAll())
    await assert.rejects(() => a.ui.fetchInput(nextHash), /EVM checkOrder/)
    const fresh = await a.ui.newOrder({side: 0, collection: f.nft, tokenId: id, price: 10_000n}, 1)
    assert.equal(fresh.nonce, 1n)
    const renewed = await a.ui.signOrder(fresh), [renewedHash] = await a.remember([renewed])
    assert.equal(a.ui.orderHash((await a.ui.fetchInput(renewedHash)).order), renewedHash)
    await f.conserved()
  })

  test(name + ': ownership and approval checkpoint/revert controls revalidate cached signed input against actual EVM state', async () => {
    const f = await market(), a = await adapter(name, f), id = await f.mint()
    const input = await a.ui.signOrder(f.order(id)), [hash] = await a.remember([input])
    for (const change of [() => f.write(f.signer, f.nft, 'transferFrom', [f.signer, f.dan, id]),
      () => f.write(f.signer, f.nft, 'setApprovalForAll', [f.exchange, false])]) {
      const before = await f.root()
      await f.vm.stateManager.checkpoint()
      await change()
      await assert.rejects(() => a.ui.fetchInput(hash), /EVM checkOrder/)
      await a.send(f.bob, a.buy(input), {error: true})
      await f.vm.stateManager.revert()
      assert.equal(await f.root(), before)
      assert.equal(a.ui.orderHash((await a.ui.fetchInput(hash)).order), hash)
    }
    await a.send(f.bob, a.buy(input))
    assert.equal(await f.read(f.nft, 'ownerOf', [id]), f.bob)
    await f.conserved()
  })

  test(name + ': exact-token and any-token bid adapters enforce token selection and remaining item count', async () => {
    const f = await market(), a = await adapter(name, f), ids = [await f.mint(f.alice), await f.mint(f.alice)]
    await f.write(f.alice, f.nft, 'setApprovalForAll', [f.exchange, true])
    await f.write(f.owner, f.currency, 'transfer', [f.signer, 30_000n])
    await f.write(f.signer, f.currency, 'approve', [f.exchange, 30_000n])
    const order = await a.ui.newOrder({side: 1, collection: f.nft, tokenId: 0n, anyToken: true,
      amount: 2n, price: 10_000n, paymentToken: f.currency}, 1)
    const input = await a.ui.signOrder(order)
    for (let i = 0; i < ids.length; i++) {
      await a.send(f.alice, a.accept(input, ids[i]))
      assert.equal(await f.read(f.nft, 'ownerOf', [ids[i]]), f.signer)
      assert.equal(await f.read(f.exchange, 'filled', [a.ui.orderHash(order)]), BigInt(i + 1))
    }
    const first = await f.mint(f.alice), other = await f.mint(f.alice)
    const exact = await a.ui.signOrder(f.order(first, {side: 1, anyToken: false, paymentToken: f.currency, salt: 999n}))
    await a.send(f.alice, a.accept(exact, other), {error: true})
    assert.equal(await f.read(f.exchange, 'filled', [a.ui.orderHash(exact.order)]), 0n)
    assert.equal(await f.read(f.currency, 'balanceOf', [f.signer]), 10_000n)
    assert.equal(await f.read(f.currency, 'balanceOf', [f.alice]), 18_500n)
    await f.conserved([f.currency])
  })

  test(name + ': wrapped-native funding adapter supplies exact collateral and finite exchange allowance once', async () => {
    const f = await market(), a = await adapter(name, f)
    const funding = name === 'zvm-nft' ? a.ui.bidFunding : a.ui.offerFunding
    const calls = await funding(f.signer, 20_000n)
    assert.equal(calls.length, 2)
    for (const call of calls) await a.send(f.signer, call)
    assert.equal(await f.read(f.wznn, 'balanceOf', [f.signer]), 20_000n)
    assert.equal(await f.read(f.wznn, 'allowance', [f.signer, f.exchange]), 20_000n)
    assert.equal(await f.balance(f.wznn), 20_000n)
    assert.equal((await funding(f.signer, 20_000n)).length, 0)
    await f.conserved([f.wznn])
  })

  test(name + ': on-chain approval and rejected native payouts expose backed credits and withdrawal in the adapter ABI', async () => {
    const f = await market(), a = await adapter(name, f), id = await f.mint(), order = f.order(id)
    assert.equal(a.ui.KIND_ONCHAIN, 2)
    const encode = (functionName, args = []) => a.sdk.encodeFunctionData({abi: a.ui.exchangeAbi, functionName, args})
    await f.frame({from: f.signer, to: f.exchange, data: encode('approveOrders', [[order]])})
    const input = {order, kind: a.ui.KIND_ONCHAIN, signature: '0x', proof: []}
    assert.deepEqual(await f.read(f.exchange, 'checkOrder', [input]), [0, 1n])
    const receiver = await f.deploy('PayoutWallet')
    await f.write(f.owner, receiver, 'configure', [1n, ZERO, '0x'])
    await f.write(f.owner, f.exchange, 'setFee', [receiver, 250n])
    await f.write(f.alice, f.nft, 'setRoyalty', [receiver, 500])
    await a.send(f.bob, a.buy(input))
    const credit = await f.frame({to: f.exchange, data: encode('credits', [receiver]), isStatic: true})
    assert.equal(a.sdk.decodeFunctionResult({abi: a.ui.exchangeAbi, functionName: 'credits', data: credit.returned}), 750n)
    assert.equal(await f.balance(f.exchange), 750n)
    await f.write(f.owner, receiver, 'configure', [0n, ZERO, '0x'])
    await f.write(f.owner, receiver, 'execute', [f.exchange, encode('withdrawCredits')])
    assert.equal(await f.balance(receiver), 750n)
    assert.equal(await f.read(f.exchange, 'credits', [receiver]), 0n)
    assert.equal(await f.balance(f.exchange), 0n)
    await f.conserved()
  })
}

console.log('MARKET-INTERFACE-QUALIFICATION ' + JSON.stringify({node: process.version,
  frontends: Object.fromEntries(Object.entries(inputs.frontends).map(([name, row]) => [name,
    {revision: manifest.frontends[name].revision, sourceSha256: row.sourceSha256, sdkVersion: manifest.runtimeVersion,
      exactSDKGraphSha256: row.sdkGraphSha256}])),
  GoVectorsRevision: manifest.core.revision, actualSolidityAndUIAdapters: true,
  apiAndProviderCapabilities: 'synthetic adapters backed by isolated EVM state',
  realBrowserNodeIndexerCanonicalReorgWalletOrChainExecution: false}))
