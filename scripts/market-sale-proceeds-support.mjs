// Independent SDK observations of the production per-item proceeds display.
import assert from 'node:assert/strict'

export function createSaleProceedsChecks({fixture, page: currentPage, chain, viem, expectedPayment, record}) {
  const {parseAbi, encodeFunctionData, decodeFunctionResult} = viem
  const zero = '0x' + '0'.repeat(40), observations = []
  const feeAbi = parseAbi(['function feeBps() view returns (uint256)', 'function feeRecipient() view returns (address)', 'function MAX_ROYALTY_BPS() view returns (uint256)'])
  const royaltyAbi = parseAbi(['function royaltyInfo(uint256,uint256) view returns (address,uint256)'])
  async function read(to, abi, functionName, args, hash) {
    const data = encodeFunctionData({abi, functionName, args})
    const result = await chain.request({method: 'eth_call', params: [{to, data}, {blockHash: hash, requireCanonical: true}]})
    return decodeFunctionResult({abi, functionName, data: result})
  }
  async function verify(ids) {
    assert.equal(fixture.app, 'karum')
    const price = BigInt(expectedPayment(ids[0]).price)
    assert.ok(ids.every(id => BigInt(expectedPayment(id).price) === price))
    const page = currentPage()
    await page.waitForFunction(price => {
      const view = document.querySelector('[data-testid="listing-proceeds"]')
      return view?.getAttribute('data-state') === 'ready' && view.getAttribute('data-price') === price
    }, {timeout: 12000, polling: 50}, price.toString())
    const view = await page.$eval('[data-testid="listing-proceeds"]', element => ({...element.dataset}))
    assert.match(view.block, /^0x[0-9a-fA-F]{64}$/)
    const [bps, recipient, capBps] = await Promise.all([
      read(fixture.exchange, feeAbi, 'feeBps', [], view.block), read(fixture.exchange, feeAbi, 'feeRecipient', [], view.block),
      read(fixture.exchange, feeAbi, 'MAX_ROYALTY_BPS', [], view.block),
    ])
    assert.equal(capBps, 1000n)
    const items = []
    for (const id of ids) {
      const [receiver, reported] = await read(fixture.collection, royaltyAbi, 'royaltyInfo', [BigInt(id), price], view.block)
      const cap = price * capBps / 10000n, royalty = receiver.toLowerCase() === zero ? 0n : reported > cap ? cap : reported
      const fee = recipient.toLowerCase() === zero ? 0n : price * bps / 10000n
      items.push({id: String(id), receiver, reportedRoyalty: reported.toString(), royalty: royalty.toString(), fee: fee.toString(), proceeds: (price - royalty - fee).toString()})
    }
    for (const [field, values] of [['proceeds', items.map(x => BigInt(x.proceeds))], ['royalty', items.map(x => BigInt(x.royalty))], ['fee', items.map(x => BigInt(x.fee))]]) {
      const sorted = values.sort((a, b) => a < b ? -1 : a > b ? 1 : 0)
      assert.equal(view[field + 'Min'], sorted[0].toString()); assert.equal(view[field + 'Max'], sorted.at(-1).toString())
    }
    const header = await chain.request({method: 'eth_getBlockByHash', params: [view.block, false]})
    assert.equal(header.hash.toLowerCase(), view.block.toLowerCase())
    const canonical = await chain.request({method: 'eth_getBlockByNumber', params: [header.number, false]})
    assert.equal(canonical.hash.toLowerCase(), view.block.toLowerCase())
    observations.push({hash: view.block, number: header.number, price: price.toString(), feeBps: bps.toString(), feeRecipient: recipient, capBps: capBps.toString(), items})
  }
  function finish() {
    assert.equal(observations.length, fixture.kind === 'bulk' ? 1 : 2)
    assert.deepEqual(observations.flatMap(row => row.items.map(item => item.id)).sort(), ['1', '2'])
    record('actual per-item proceeds use independent canonical SDK royalty cap fee and exact amount reads')
  }
  return {verify, finish, summary: passed => ({pass: !!passed && observations.length === (fixture.kind === 'bulk' ? 1 : 2),
    observations, actualProductionPreviewAndIndependentHashSelectedSDKReads: true,
    actualAboveCapCollectionOrRevertingRoyaltyExecutionQualified: false, productAPIOrRPCRepliesReplaced: 0})}
}
