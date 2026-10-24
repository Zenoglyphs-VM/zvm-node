import assert from 'node:assert/strict'
import test from 'node:test'
import {concatHex, decodeFunctionResult, hashStruct, hashTypedData, keccak256} from 'viem'
import {artifacts, errorName, INITIAL, MAX, random, signer, START, world, ZERO} from './evm.mjs'
const events = (f, result, name, at) => f.events(result, at).filter((e) => e.eventName === name).map((e) => e.args)
export const orderTypes = {Order: [
  ['trader', 'address'], ['side', 'uint8'], ['collection', 'address'], ['tokenId', 'uint256'], ['anyToken', 'bool'], ['amount', 'uint256'],
  ['paymentToken', 'address'], ['price', 'uint256'], ['listingTime', 'uint64'], ['expirationTime', 'uint64'], ['nonce', 'uint256'], ['salt', 'uint256'],
].map(([name, type]) => ({name, type}))}
const rootTypes = {OrderRoot: [{name: 'root', type: 'bytes32'}]}
const digest = (order) => hashStruct({types: orderTypes, primaryType: 'Order', data: order})
const domain = (exchange) => ({name: 'ZVM Exchange', version: '1', chainId: 7340469, verifyingContract: exchange})

async function market() {
  const f = await world(), c = await f.collection(), exchange = await f.deploy('ZVMExchange', [f.owner, f.revenue, 250n])
  const currency = await f.deploy('BenchERC20', ['Fixture currency', 'FIX', 10n ** 24n])
  await f.write(f.alice, c.collection, 'setRoyalty', [f.royalty, 500])
  await f.write(f.alice, c.collection, 'setApprovalForAll', [exchange, true])
  async function mint(to = f.alice) {
    const id = await f.read(c.collection, 'nextTokenId'); await f.write(f.alice, c.collection, 'mint', [to, '']); return id
  }
  const order = (id, overrides = {}) => ({trader: f.alice, side: 0, collection: c.collection, tokenId: id, anyToken: false, amount: 1n,
    paymentToken: ZERO, price: 10_000n, listingTime: 0n, expirationTime: START + 1000n, nonce: 0n, salt: id, ...overrides})
  const input = (o, overrides = {}) => ({order: o, kind: 2, signature: '0x', proof: [], ...overrides})
  const approve = async (o) => {await f.write(o.trader, exchange, 'approveOrders', [[o]]); return input(o)}
  const buy = (o, value = o.order.price, options = {}) => f.write(f.bob, exchange, 'buy', [o], value, options)
  const fund = async (to, amount = 100_000n, approval = MAX) => {
    await f.write(f.owner, currency, 'transfer', [to, amount]); await f.write(to, currency, 'approve', [exchange, approval])
  }
  return Object.assign(f, {collection: c.collection, collectionFactory: c.factory, implementation: c.implementation, exchange, currency, mint, order, input, approve, buy, fund})
}

test('factory calls create initialized clones, lock the implementation and isolate owners and metadata', async () => {
  const f = await world(), a = await f.collection()
  assert.equal(await f.read(a.implementation, 'owner'), f.owner)
  await f.write(f.alice, a.implementation, 'initialize', [f.alice, 'Bad', 'BAD', '', '', 0], 0n, {error: 'AlreadyInitialized'})
  await f.write(f.bob, a.collection, 'initialize', [f.bob, 'Bad', 'BAD', '', '', 0], 0n, {error: 'AlreadyInitialized'})
  assert.equal(await f.read(a.collection, 'owner'), f.alice)
  assert.equal(await f.read(a.collection, 'name'), 'Fixture collection')
  assert.equal(await f.read(a.collection, 'symbol'), 'FIX')
  assert.equal(await f.read(a.factory, 'isCollection', [a.collection]), true)
  assert.equal(await f.read(a.factory, 'collectionsLength'), 1n)
  const result = await f.write(f.bob, a.factory, 'createCollection', ['Second fixture', 'TWO', '', '', 1000])
  const second = events(f, result, 'CollectionCreated', a.factory)[0].collection
  f.register(second, artifacts.ZVMCollection.abi)
  assert.equal(await f.read(second, 'owner'), f.bob)
  assert.equal(await f.read(a.collection, 'owner'), f.alice)
  await f.write(f.bob, a.factory, 'createCollection', ['Invalid', 'BAD', '', '', 1001], 0n, {error: 'RoyaltyTooHigh'})
  assert.equal(await f.read(a.factory, 'collectionsLength'), 2n)
  await f.conserved()
})

test('collection owner mint/batch, metadata, royalty and approved burns execute real ERC721 state', async () => {
  const f = await world(), {collection: c} = await f.collection()
  await f.write(f.bob, c, 'mint', [f.bob, ''], 0n, {error: 'OwnableUnauthorizedAccount'})
  await f.write(f.alice, c, 'mintBatch', [f.bob, ['ipfs://one', '']])
  assert.equal(await f.read(c, 'ownerOf', [1n]), f.bob)
  assert.equal(await f.read(c, 'tokenURI', [1n]), 'ipfs://one')
  assert.equal(await f.read(c, 'tokenURI', [2n]), 'ipfs://fixture/2')
  await f.write(f.alice, c, 'setBaseURI', ['ipfs://next/'])
  await f.write(f.alice, c, 'setTokenURI', [2n, 'ipfs://two'])
  await f.write(f.alice, c, 'setContractURI', ['ipfs://updated-contract'])
  assert.equal(await f.read(c, 'contractURI'), 'ipfs://updated-contract')
  assert.equal(await f.read(c, 'supportsInterface', ['0x49064906']), true)
  await f.write(f.alice, c, 'setRoyalty', [f.royalty, 1000])
  assert.deepEqual(await f.read(c, 'royaltyInfo', [1n, 10_001n]), [f.royalty, 1000n])
  await f.write(f.alice, c, 'setRoyalty', [f.royalty, 1001], 0n, {error: 'RoyaltyTooHigh'})
  await f.write(f.carol, c, 'burn', [1n], 0n, {error: 'ERC721InsufficientApproval'})
  await f.write(f.bob, c, 'approve', [f.carol, 1n]); await f.write(f.carol, c, 'burn', [1n])
  assert.equal(await f.read(c, 'totalSupply'), 1n)
  assert.equal(await f.read(c, 'nextTokenId'), 3n)
  await f.write(f.alice, c, 'tokenURI', [1n], 0n, {error: 'ERC721NonexistentToken'})
  await f.conserved()
})

test('public drop boundaries, exact payment, cumulative wallet/supply caps and payout rollback are enforced', async () => {
  const f = await world(), {collection: c} = await f.collection()
  await f.write(f.bob, c, 'publicMint', [1n], 0n, {error: 'DropNotOpen'})
  await f.write(f.alice, c, 'setDrop', [100n, 3, 2, Number(START + 1n), Number(START + 100n)])
  await f.write(f.bob, c, 'publicMint', [1n], 100n, {error: 'DropNotOpen'})
  f.mine(1n)
  await f.write(f.bob, c, 'publicMint', [0n], 0n, {error: 'ZeroQuantity'})
  for (const amount of [199n, 201n]) await f.write(f.bob, c, 'publicMint', [2n], amount, {error: 'WrongPayment'})
  await f.write(f.bob, c, 'publicMint', [2n], 200n)
  await f.write(f.bob, c, 'burn', [1n])
  await f.write(f.bob, c, 'publicMint', [1n], 100n, {error: 'WalletLimit'})
  await f.write(f.carol, c, 'publicMint', [2n], 200n, {error: 'DropSoldOut'})
  await f.write(f.carol, c, 'publicMint', [1n], 100n)
  assert.equal(await f.balance(f.alice), INITIAL + 300n)
  const wallet = await f.deploy('PayoutWallet')
  await f.write(f.owner, wallet, 'configure', [1n, ZERO, '0x'])
  const other = await f.collection({creator: f.bob})
  await f.write(f.bob, other.collection, 'transferOwnership', [wallet])
  await f.forward(wallet, other.collection, 'setDrop', [100n, 10, 10, Number(START), 0])
  await f.write(f.carol, other.collection, 'publicMint', [1n], 100n, {error: 'PayoutFailed'})
  assert.equal(await f.read(other.collection, 'nextTokenId'), 1n)
  assert.equal(await f.read(other.collection, 'dropMinted', [f.carol]), 0n)
  f.mine(100n); await f.write(f.dan, c, 'publicMint', [1n], 100n, {error: 'DropNotOpen'})
  await f.conserved()
})

test('native sale distributes exact seller/royalty/fee value and late wrong-payment failures restore the NFT', async () => {
  const f = await market(), id = await f.mint(), o = f.order(id), input = await f.approve(o), h = digest(o)
  assert.equal(await f.read(f.exchange, 'hashOrder', [o]), h)
  for (const value of [9999n, 10_001n]) await f.buy(input, value, {error: 'WrongPayment'})
  assert.equal(await f.read(f.collection, 'ownerOf', [id]), f.alice)
  assert.equal(await f.read(f.exchange, 'filled', [h]), 0n)
  const result = await f.buy(input)
  assert.equal(await f.read(f.collection, 'ownerOf', [id]), f.bob)
  assert.equal(await f.balance(f.bob), INITIAL - 10_000n)
  assert.equal(await f.balance(f.alice), INITIAL + 9250n)
  assert.equal(await f.balance(f.royalty), INITIAL + 500n)
  assert.equal(await f.balance(f.revenue), INITIAL + 250n)
  assert.equal(events(f, result, 'OrderFilled', f.exchange)[0].orderHash, h)
  assert.deepEqual(await f.read(f.exchange, 'checkOrder', [input]), [6, 0n])
  await f.conserved([f.currency])
})

test('ERC20 sale debits the exact aggregate allowance and undoes earlier payouts on a late shortfall', async () => {
  const f = await market(), id = await f.mint(), o = f.order(id, {paymentToken: f.currency}), input = await f.approve(o)
  await f.fund(f.bob, 10_000n, 9999n)
  await f.buy(input, 0n, {error: 'ERC20InsufficientAllowance'})
  assert.equal(await f.read(f.currency, 'balanceOf', [f.royalty]), 0n)
  assert.equal(await f.read(f.collection, 'ownerOf', [id]), f.alice)
  await f.write(f.bob, f.currency, 'approve', [f.exchange, 10_000n])
  await f.buy(input, 0n)
  for (const [at, value] of [[f.bob, 0n], [f.alice, 9250n], [f.royalty, 500n], [f.revenue, 250n]]) assert.equal(await f.read(f.currency, 'balanceOf', [at]), value)
  assert.equal(await f.read(f.currency, 'allowance', [f.bob, f.exchange]), 0n)
  await f.conserved([f.currency])
})

test('order status distinguishes malformed, unauthorized, cancelled, stale, time and possession conditions', async () => {
  const f = await market(), id = await f.mint(), base = f.order(id)
  for (const override of [{amount: 0n}, {price: 0n}, {trader: ZERO}, {amount: 2n}, {anyToken: true}, {side: 1}])
    assert.equal((await f.read(f.exchange, 'checkOrder', [f.input({...base, ...override})]))[0], 9)
  assert.equal((await f.read(f.exchange, 'checkOrder', [f.input(base)]))[0], 1)
  const valid = await f.approve(base)
  await f.write(f.bob, f.exchange, 'approveOrders', [[base]], 0n, {error: 'NotTrader'})
  await f.write(f.bob, f.exchange, 'cancelOrders', [[base]], 0n, {error: 'NotTrader'})
  assert.deepEqual(await f.read(f.exchange, 'checkOrder', [valid]), [0, 1n])
  for (const [override, code] of [[{listingTime: START + 1n, salt: 10n}, 4], [{expirationTime: START, salt: 11n}, 5]]) {
    const input = await f.approve({...base, ...override}); assert.equal((await f.read(f.exchange, 'checkOrder', [input]))[0], code)
  }
  await f.write(f.alice, f.collection, 'setApprovalForAll', [f.exchange, false])
  assert.equal((await f.read(f.exchange, 'checkOrder', [valid]))[0], 7)
  await f.write(f.alice, f.collection, 'setApprovalForAll', [f.exchange, true])
  await f.write(f.alice, f.exchange, 'cancelOrders', [[base]])
  assert.equal((await f.read(f.exchange, 'checkOrder', [valid]))[0], 2)
  const next = await f.approve({...base, salt: 12n})
  await f.write(f.alice, f.exchange, 'incrementNonce')
  assert.equal((await f.read(f.exchange, 'checkOrder', [next]))[0], 3)
})

test('actual offline EIP712 single and bulk signatures match independent digests and enforce domains/proofs', async () => {
  const f = await market(), ids = [await f.mint(signer.address), await f.mint(signer.address), await f.mint(signer.address)]
  await f.write(signer.address, f.collection, 'setApprovalForAll', [f.exchange, true])
  const orders = ids.map((id) => f.order(id, {trader: signer.address}))
  const signature = await signer.signTypedData({domain: domain(f.exchange), types: orderTypes, primaryType: 'Order', message: orders[0]})
  assert.equal(await f.read(f.exchange, 'orderDigest', [digest(orders[0])]), hashTypedData({domain: domain(f.exchange), types: orderTypes, primaryType: 'Order', message: orders[0]}))
  const other = await f.deploy('ZVMExchange', [f.owner, f.revenue, 250n])
  assert.equal((await f.read(other, 'checkOrder', [f.input(orders[0], {kind: 0, signature})]))[0], 1)
  await f.buy(f.input(orders[0], {kind: 0, signature}))
  assert.equal((await f.read(f.exchange, 'checkOrder', [f.input({...orders[1], price: 1n}, {kind: 0, signature})]))[0], 1)
  const leaves = orders.slice(1).map(digest), root = keccak256(concatHex([...leaves].sort()))
  const bulk = await signer.signTypedData({domain: domain(f.exchange), types: rootTypes, primaryType: 'OrderRoot', message: {root}})
  assert.equal(await f.read(f.exchange, 'rootDigest', [root]), hashTypedData({domain: domain(f.exchange), types: rootTypes, primaryType: 'OrderRoot', message: {root}}))
  for (let i = 0; i < 2; i++) {
    const input = f.input(orders[i + 1], {kind: 1, signature: bulk, proof: [leaves[1 - i]]})
    assert.equal((await f.read(f.exchange, 'checkOrder', [{...input, proof: []}]))[0], 1)
    await f.buy(input)
  }
  await f.conserved([f.currency])
})

test('ERC1271 contract approval authorizes a real NFT sale and a changed digest is refused', async () => {
  const f = await market(), wallet = await f.deploy('ContractSigner'), id = await f.mint(wallet), o = f.order(id, {trader: wallet})
  await f.forward(wallet, f.collection, 'setApprovalForAll', [f.exchange, true])
  const input = f.input(o, {kind: 0, signature: '0x1122'})
  assert.equal((await f.read(f.exchange, 'checkOrder', [input]))[0], 1)
  await f.write(f.owner, wallet, 'setDigest', [await f.read(f.exchange, 'orderDigest', [digest(o)])])
  assert.equal((await f.read(f.exchange, 'checkOrder', [input]))[0], 0)
  await f.buy(input)
  assert.equal(await f.read(f.collection, 'ownerOf', [id]), f.bob)
  await f.conserved([f.currency])
})

test('any-token bids consume their declared item count and exact-token mismatches roll back consumption', async () => {
  const f = await market(), ids = [await f.mint(), await f.mint()], o = f.order(0n, {trader: f.bob, side: 1, anyToken: true, amount: 2n, paymentToken: f.currency})
  await f.fund(f.bob)
  const input = await f.approve(o)
  for (let i = 0; i < 2; i++) {
    await f.write(f.alice, f.exchange, 'acceptBid', [input, ids[i]])
    assert.equal(await f.read(f.collection, 'ownerOf', [ids[i]]), f.bob)
    assert.equal(await f.read(f.exchange, 'filled', [digest(o)]), BigInt(i + 1))
  }
  assert.equal((await f.read(f.exchange, 'checkOrder', [input]))[0], 6)
  const another = await f.mint(), exact = f.order(another + 1n, {trader: f.bob, side: 1, paymentToken: f.currency})
  const fixed = await f.approve(exact)
  await f.write(f.alice, f.exchange, 'acceptBid', [fixed, another], 0n, {error: 'Unfillable'})
  assert.equal(await f.read(f.exchange, 'filled', [digest(exact)]), 0n)
  await f.conserved([f.currency])
})

test('sweeps skip cancelled orders, refund exact excess and undo earlier fills on an aggregate shortfall', async () => {
  const f = await market(), ids = [await f.mint(), await f.mint(), await f.mint()], inputs = []
  for (let i = 0; i < 3; i++) inputs.push(await f.approve(f.order(ids[i], {price: 10_000n + 1000n * BigInt(i)})))
  await f.write(f.alice, f.exchange, 'cancelOrders', [[inputs[1].order]])
  await f.write(f.bob, f.exchange, 'buyMany', [inputs], 21_999n, {error: 'WrongPayment'})
  for (const id of ids) assert.equal(await f.read(f.collection, 'ownerOf', [id]), f.alice)
  const result = await f.write(f.bob, f.exchange, 'buyMany', [inputs], 25_000n)
  assert.equal(decodeFunctionResult({abi: artifacts.ZVMExchange.abi, functionName: 'buyMany', data: result.returned}), 2n)
  assert.equal(await f.balance(f.bob), INITIAL - 22_000n)
  assert.equal(await f.read(f.collection, 'ownerOf', [ids[1]]), f.alice)
  await f.conserved([f.currency])
})

test('rejected royalties/fees become backed credits and rejected/reentrant withdrawals cannot double-pay', async () => {
  const f = await market(), wallet = await f.deploy('PayoutWallet')
  await f.write(f.owner, wallet, 'configure', [1n, ZERO, '0x'])
  await f.write(f.owner, f.exchange, 'setFee', [wallet, 250n]); await f.write(f.alice, f.collection, 'setRoyalty', [wallet, 500])
  const id = await f.mint(); await f.buy(await f.approve(f.order(id)))
  assert.equal(await f.read(f.exchange, 'credits', [wallet]), 750n)
  assert.equal(await f.balance(f.exchange), 750n)
  await f.forward(wallet, f.exchange, 'withdrawCredits', [], 0n, {error: 'PayoutFailed'})
  await f.write(f.owner, wallet, 'configure', [2n, f.exchange, f.calldata(f.exchange, 'withdrawCredits')])
  const result = await f.forward(wallet, f.exchange, 'withdrawCredits'), callback = events(f, result, 'Callback', wallet)[0]
  assert.equal(callback.ok, false); assert.equal(errorName(callback.result), 'ReentrancyGuardReentrantCall')
  assert.equal(await f.balance(wallet), 750n)
  assert.equal(await f.read(f.exchange, 'credits', [wallet]), 0n)
  await f.conserved([f.currency])
})

test('untrusted royalty values are capped at 10% and fee administration is bounded by 250 basis points', async () => {
  const f = await market(), nft = await f.deploy('RoyaltyNFT')
  await f.write(f.bob, f.exchange, 'setFee', [f.revenue, 1n], 0n, {error: 'OwnableUnauthorizedAccount'})
  await f.write(f.owner, f.exchange, 'setFee', [f.revenue, 251n], 0n, {error: 'FeeTooHigh'})
  await f.write(f.owner, nft, 'mint', [f.alice, 1n]); await f.write(f.alice, nft, 'approve', [f.exchange, 1n])
  await f.write(f.owner, nft, 'setRoyalty', [f.royalty, 100_000n])
  const o = f.order(1n, {collection: nft}); await f.buy(await f.approve(o))
  assert.equal(await f.balance(f.royalty), INITIAL + 1000n)
  assert.equal(await f.balance(f.alice), INITIAL + 8750n)
  await f.conserved([f.currency])
})

test('seeded sales match independent NFT ownership, fill status and native payment models', async (t) => {
  for (const seed of [3, 17, 0x12345678, 0x9e3779b9]) await t.test('seed ' + seed, async () => {
    const f = await market(), next = random(seed)
    let seller = 0n, buyer = 0n, royalty = 0n, fee = 0n, bought = 0n
    for (let step = 0; step < 32; step++) {
      const id = await f.mint(), price = 1n + BigInt(next() % 100_000), o = f.order(id, {price}), input = await f.approve(o)
      const cancelled = next() % 4 === 0
      if (cancelled) {
        await f.write(f.alice, f.exchange, 'cancelOrders', [[o]])
        await f.buy(input, price, {error: 'Unfillable'})
      } else {
        if (next() % 3 === 0) await f.buy(input, price + 1n, {error: 'WrongPayment'})
        const result = await f.buy(input), r = price * 500n / 10_000n, charge = price * 250n / 10_000n
        seller += price - r - charge; buyer += price; royalty += r; fee += charge; bought++
        const receipt = events(f, result, 'OrderFilled', f.exchange)[0]
        assert.equal(receipt.price, price); assert.equal(receipt.royalty, r); assert.equal(receipt.fee, charge)
      }
      assert.equal(await f.read(f.collection, 'ownerOf', [id]), cancelled ? f.alice : f.bob)
      assert.equal(await f.read(f.exchange, 'filled', [digest(o)]), cancelled ? 0n : 1n)
      assert.equal(await f.balance(f.alice), INITIAL + seller); assert.equal(await f.balance(f.bob), INITIAL - buyer)
      assert.equal(await f.balance(f.royalty), INITIAL + royalty); assert.equal(await f.balance(f.revenue), INITIAL + fee)
      assert.equal(await f.read(f.collection, 'totalSupply'), BigInt(step + 1))
      assert.equal(await f.read(f.collection, 'balanceOf', [f.bob]), bought)
      assert.equal(await f.balance(f.exchange), 0n)
      await f.conserved([f.currency])
    }
  })
})
