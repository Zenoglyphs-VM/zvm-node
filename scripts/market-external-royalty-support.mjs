// Independent SDK observations for actual production external-royalty purchases.
import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'

export function createExternalRoyaltyChecks({fixture, page: currentPage, chain, viem, record}) {
  const {parseAbi, encodeFunctionData, decodeFunctionResult, decodeEventLog, keccak256, stringToHex} = viem
  const config = fixture.externalRoyalty, zero = '0x' + '0'.repeat(40)
  assert.equal(config.profile, 'cap-and-revert'); assert.equal(config.price, '200000000000000018')
  assert.equal(config.reportedBps, 5000); assert.equal(config.capBps, 1000)
  assert.equal(fixture.kind, 'single'); assert.equal(fixture.action, 'batch')
  assert.equal(config.authenticatedNoMQualified, false); assert.equal(config.physicalWalletOrPublicNetwork, false)
  const price = BigInt(config.price), cap = price * 1000n / 10000n, fee = price * 200n / 10000n
  assert.equal(price * 1200n / 10000n - cap - fee, 1n)
  const royaltyAbi = parseAbi(['function royaltyInfo(uint256,uint256) view returns (address,uint256)'])
  const exchangeAbi = parseAbi(['function feeBps() view returns (uint256)', 'function feeRecipient() view returns (address)', 'function MAX_ROYALTY_BPS() view returns (uint256)'])
  const fillAbi = parseAbi(['event OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint8 side, address collection, uint256 tokenId, address paymentToken, uint256 price, uint256 royalty, uint256 fee)'])
  const observations = [], previews = [], productReverts = [], responseErrors = [], pendingResponses = new Set()
  let baseline, mined, receiptChecked = false, restored = false
  async function read(to, abi, method, args, hash) {
    const data = encodeFunctionData({abi, functionName: method, args})
    const raw = await chain.request({method: 'eth_call', params: [{to, data}, {blockHash: hash, requireCanonical: true}]})
    return decodeFunctionResult({abi, functionName: method, data: raw})
  }
  async function canonical(hash) {
    const header = await chain.request({method: 'eth_getBlockByHash', params: [hash, false]})
    assert.equal(header.hash.toLowerCase(), hash.toLowerCase())
    const selected = await chain.request({method: 'eth_getBlockByNumber', params: [header.number, false]})
    assert.equal(selected.hash.toLowerCase(), hash.toLowerCase())
    return header.number
  }
  async function royaltyReports(hash) {
    const [receiver, reported] = await read(fixture.collection, royaltyAbi, 'royaltyInfo', [1n, price], hash)
    assert.equal(receiver.toLowerCase(), fixture.payouts.royaltyRecipient.toLowerCase())
    assert.equal(reported, price * 5000n / 10000n)
    await assert.rejects(() => read(fixture.collection, royaltyAbi, 'royaltyInfo', [2n, price], hash), /execution reverted/i)
    return {receiver, reported: reported.toString(), secondTokenCallReverted: true}
  }
  function observeResponse(response) {
    if (new URL(response.url()).pathname !== '/rpc' || response.request().method() !== 'POST') return
    let requests
    try {requests = JSON.parse(response.request().postData() ?? 'null')} catch {return}
    const call = encodeFunctionData({abi: royaltyAbi, functionName: 'royaltyInfo', args: [2n, price]}).toLowerCase()
    const selected = (Array.isArray(requests) ? requests : [requests]).filter(row => row?.method === 'eth_call' &&
      row.params?.[0]?.to?.toLowerCase() === fixture.collection.toLowerCase() && row.params[0].data?.toLowerCase() === call)
    if (!selected.length) return
    assert.ok(pendingResponses.size < 64)
    let promise
    promise = (async () => {
      const raw = await response.text(); assert.ok(raw.length <= 65536)
      const decoded = JSON.parse(raw), replies = Array.isArray(decoded) ? decoded : [decoded]
      for (const request of selected) {
        const reply = replies.find(row => row.id === request.id)
        assert.ok(reply?.error && /execution reverted/i.test(reply.error.message))
        assert.match(request.params[1]?.blockHash ?? '', /^0x[0-9a-f]{64}$/i)
        assert.equal(request.params[1].requireCanonical, true)
        assert.ok(productReverts.length < 64)
        productReverts.push({hash: request.params[1].blockHash, exactPrice: config.price, token: '2', errorCode: reply.error.code})
      }
    })().catch(error => {responseErrors.push(String(error.message).slice(0, 160))}).finally(() => pendingResponses.delete(promise))
    pendingResponses.add(promise)
  }
  async function state(phase, state) {
    const value = state.payouts
    assert.ok(value.pass && value.hashBoundContractAndBalanceReads && value.allPayoutCreditsZero)
    assert.ok(value.actualAboveCapReport && value.actualRevertingRoyaltyCall && value.sourceBoundExternalRuntime)
    assert.equal(value.sourceSHA256, config.sourceSHA256); assert.equal(value.artifactSHA256, config.artifactSHA256)
    const included = state.included, fills = included ? 2n : 0n
    const gross = price * fills, royalty = included ? cap : 0n, fees = fee * fills
    assert.equal(value.expectedFills, Number(fills)); assert.equal(value.nativeGross, gross.toString())
    assert.equal(value.nativeRoyalty, royalty.toString()); assert.equal(value.nativeProtocolFee, fees.toString())
    assert.equal(value.nativeSellerProceeds, (gross - royalty - fees).toString()); assert.equal(value.sellerReceiptFee, '0')
    const addresses = {buyer: fixture.buyer, seller: fixture.seller, feeRecipient: fixture.payouts.feeRecipient, royaltyRecipient: fixture.payouts.royaltyRecipient}
    assert.equal(new Set(Object.values(addresses).map(x => x.toLowerCase())).size, 4)
    const actual = {}
    for (const [name, address] of Object.entries(addresses)) {
      actual[name] = BigInt(await chain.request({method: 'eth_getBalance', params: [address, {blockHash: value.canonicalHash, requireCanonical: true}]}))
      assert.equal(actual[name].toString(), value.balances[name])
    }
    const number = await canonical(value.canonicalHash)
    const reports = await royaltyReports(value.canonicalHash)
    if (phase === 'bind') {
      assert.equal(fixture.payouts.feeBps, 200); assert.equal(fixture.payouts.royaltyBps, 5000)
      assert.equal(await read(fixture.exchange, exchangeAbi, 'feeBps', [], value.canonicalHash), 200n)
      assert.equal(await read(fixture.exchange, exchangeAbi, 'MAX_ROYALTY_BPS', [], value.canonicalHash), 1000n)
      assert.equal((await read(fixture.exchange, exchangeAbi, 'feeRecipient', [], value.canonicalHash)).toLowerCase(), addresses.feeRecipient.toLowerCase())
      const runtime = await chain.request({method: 'eth_getCode', params: [config.implementation, {blockHash: value.canonicalHash, requireCanonical: true}]})
      assert.ok(/^0x[0-9a-f]+$/i.test(runtime) && runtime.length > 2)
      assert.equal(createHash('sha256').update(Buffer.from(runtime.slice(2), 'hex')).digest('hex'), config.runtimeSHA256)
      assert.equal(actual.feeRecipient, 0n); assert.equal(actual.royaltyRecipient, 0n)
      baseline = actual
      record('actual independent royalty runtime reports an above-cap amount and executes the second token royalty revert')
    }
    assert.ok(baseline)
    assert.equal(actual.buyer, baseline.buyer - gross - BigInt(value.buyerReceiptFee))
    assert.equal(actual.seller, baseline.seller + gross - royalty - fees)
    assert.equal(actual.feeRecipient, baseline.feeRecipient + fees)
    assert.equal(actual.royaltyRecipient, baseline.royaltyRecipient + royalty)
    if (phase === 'mined') mined = structuredClone(value)
    if (phase === 'replacement') assert.deepEqual(actual, baseline)
    if (phase === 'restored') {
      assert.deepEqual(value, mined); restored = true
      record('actual external royalty cap fallback receipt fees and balances roll back and restore at canonical hashes')
    }
    observations.push({phase, hash: value.canonicalHash, number, reports, state: structuredClone(value)})
  }
  async function receipt(receipt, state) {
    const topic = keccak256(stringToHex('OrderFilled(bytes32,address,address,uint8,address,uint256,address,uint256,uint256,uint256)'))
    const fills = receipt.logs.filter(row => row.address.toLowerCase() === fixture.exchange.toLowerCase() && row.topics[0] === topic)
      .map(row => decodeEventLog({abi: fillAbi, data: row.data, topics: row.topics, strict: true}).args)
    assert.deepEqual(fills.map(fill => String(fill.tokenId)).sort(), ['1', '2'])
    for (const fill of fills) {
      assert.equal(fill.price, price); assert.equal(fill.royalty, fill.tokenId === 1n ? cap : 0n); assert.equal(fill.fee, fee)
      assert.equal(fill.paymentToken.toLowerCase(), zero); assert.equal(fill.side, 0)
      assert.equal(fill.maker.toLowerCase(), fixture.seller.toLowerCase()); assert.equal(fill.taker.toLowerCase(), fixture.buyer.toLowerCase())
      assert.equal(fill.orderHash.toLowerCase(), state.hashes[Number(fill.tokenId) - 1].toLowerCase())
    }
    receiptChecked = true
    record('actual external royalty fill logs retain exact native prices separate floors cap and zero-royalty fallback')
  }
  const preview = {
    async verify(ids) {
      assert.equal(fixture.app, 'karum'); assert.equal(ids.length, 1)
      const id = ids[0], page = currentPage(), wanted = id === 1 ? 'ready' : 'unavailable'
      assert.ok(id === 1 || id === 2)
      await page.waitForFunction((state, price) => {
        const view = document.querySelector('[data-testid="listing-proceeds"]')
        return view?.getAttribute('data-state') === state && (state === 'unavailable' || view.getAttribute('data-price') === price)
      }, {timeout: 12000, polling: 50}, wanted, config.price)
      if (id === 2) {
        const deadline = Date.now() + 12000
        while (!productReverts.length && !responseErrors.length && Date.now() < deadline) {
          await Promise.all([...pendingResponses]); await new Promise(resolve => setTimeout(resolve, 25))
        }
        assert.deepEqual(responseErrors, []); assert.ok(productReverts.length > 0)
        await page.waitForFunction(() => document.querySelector('[data-testid="listing-proceeds"]')?.getAttribute('data-state') === 'unavailable', {timeout: 12000})
      }
      const view = await page.$eval('[data-testid="listing-proceeds"]', el => ({...el.dataset}))
      let hash = view.block
      if (id === 1) {
        assert.equal(view.price, config.price)
        for (const [key, value] of [['royalty', cap], ['fee', fee], ['proceeds', price - cap - fee]]) {
          assert.equal(view[key + 'Min'], value.toString()); assert.equal(view[key + 'Max'], value.toString())
        }
      } else {
        for (const key of ['block', 'price', 'royaltyMin', 'royaltyMax', 'feeMin', 'feeMax', 'proceedsMin', 'proceedsMax']) assert.equal(view[key], '')
        assert.equal(await page.$eval('[data-testid="list-submit"]', el => el.disabled), false)
        const head = await chain.request({method: 'eth_getBlockByNumber', params: ['latest', false]}); hash = head.hash
      }
      const reports = await royaltyReports(hash), number = await canonical(hash)
      previews.push({id, state: wanted, hash, number, reports, displayed: view, exactPrice: config.price})
    },
    finish() {
      assert.deepEqual(previews.map(row => row.id), [1, 2])
      record('actual per-item preview displays the capped amount and preserves unavailable state after a real royalty revert')
    },
    summary: passed => ({pass: !!passed && previews.length === 2, observations: previews,
      actualProductionPreviewAndIndependentHashSelectedSDKReads: true, actualAboveCapCollectionOrRevertingRoyaltyExecutionQualified: !!passed,
      actualUnmodifiedProductRPCRevertResponses: productReverts,
      failedRoyaltyReadDisplayedAsUnavailable: true, productAPIOrRPCRepliesReplaced: 0}),
  }
  return {state, receipt, preview, observeResponse, summary: passed => ({pass: !!passed && receiptChecked && restored && observations.length === 5,
    configuration: config, canonicalMinedState: mined, observations, independentSDKBalanceReads: true, receiptChecked, restored,
    sourceBoundExternalRuntime: true, actualProductionPageAndProviderExecution: !!passed, authenticatedNoMQualified: false,
    physicalWalletOrPublicNetwork: false, productAPIOrRPCRepliesReplaced: 0})}
}
