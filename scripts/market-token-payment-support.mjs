// Independent SDK observations for token purchases and mixed-currency partial refunds.
import assert from 'node:assert/strict'

export function createTokenPaymentChecks({fixture, chain, viem, record}) {
  const {parseAbi, encodeFunctionData, decodeFunctionResult, decodeEventLog, keccak256, stringToHex} = viem
  const token = fixture.tokenPayment.token, zero = '0x' + '0'.repeat(40)
  assert.equal(token.toLowerCase(), '0x4200000000000000000000000000000000000101')
  const partial = fixture.action === 'partial-batch'
  const requiresApproval = fixture.tokenPayment.approvalRequired === true
  const approvalAmount = BigInt(fixture.tokenPayment.approvalAmount)
  const expected = id => fixture.tokenPayment.orders[String(id)]
  const actors = {buyer: fixture.buyer, seller: fixture.seller, feeRecipient: fixture.payouts.feeRecipient, royaltyRecipient: fixture.payouts.royaltyRecipient}
  assert.equal(new Set(Object.values(actors).map(x => x.toLowerCase())).size, 4)
  const erc20 = parseAbi(['function balanceOf(address) view returns (uint256)', 'function allowance(address,address) view returns (uint256)', 'function decimals() view returns (uint8)'])
  let before, mined, receiptChecked = false, restored = false
  async function at(address, abi, functionName, args, hash) {
    const data = encodeFunctionData({abi, functionName, args})
    const raw = await chain.request({method: 'eth_call', params: [{to: address, data}, {blockHash: hash, requireCanonical: true}]})
    return decodeFunctionResult({abi, functionName, data: raw})
  }
  async function state(name, value) {
    const p = value.payouts, hash = p.canonicalHash
    assert.ok(p.pass && p.hashBoundContractAndBalanceReads && p.allPayoutCreditsZero)
    const native = {}, qsr = {}
    for (const [key, address] of Object.entries(actors)) {
      native[key] = BigInt(await chain.request({method: 'eth_getBalance', params: [address, {blockHash: hash, requireCanonical: true}]}))
      qsr[key] = await at(token, erc20, 'balanceOf', [address], hash)
      assert.equal(native[key].toString(), p.balances[key]); assert.equal(qsr[key].toString(), p.qsrBalances[key])
    }
    const allowance = await at(token, erc20, 'allowance', [fixture.buyer, fixture.exchange], hash)
    assert.equal(allowance.toString(), p.qsrAllowance)
    assert.equal(BigInt(await at(token, erc20, 'decimals', [], hash)), 8n)
    const code = await chain.request({method: 'eth_getCode', params: [token, {blockHash: hash, requireCanonical: true}]})
    assert.ok(code.length > 2); assert.equal(keccak256(code), p.qsrCodeHash)
    let nativeGross = 0n, nativeRoyalty = 0n, nativeFee = 0n, qsrGross = 0n, qsrRoyalty = 0n, qsrFee = 0n, fills = 0
    for (const id of [1, 2]) {
      if (!value.included || (fixture.action === 'buy' && id === 2) || (partial && id === 1)) continue
      fills++
      const item = expected(id), price = BigInt(item.price)
      if (item.paymentToken.toLowerCase() === zero) {
        nativeGross += price; nativeRoyalty += price * 375n / 10000n; nativeFee += price * 200n / 10000n
      } else {
        assert.equal(item.paymentToken.toLowerCase(), token.toLowerCase()); assert.equal(price, 20000001n)
        qsrGross += price; qsrRoyalty += price * 375n / 10000n; qsrFee += price * 200n / 10000n
      }
    }
    assert.equal(p.expectedFills, fills)
    for (const [key, amount] of Object.entries({nativeGross, nativeRoyalty, nativeProtocolFee: nativeFee, nativeSellerProceeds: nativeGross - nativeRoyalty - nativeFee,
      qsrGross, qsrRoyalty, qsrProtocolFee: qsrFee, qsrSellerProceeds: qsrGross - qsrRoyalty - qsrFee})) assert.equal(p[key], amount.toString())
    if (name === 'bind') {
      assert.equal(fixture.payouts.feeBps, 200); assert.equal(fixture.payouts.royaltyBps, 375)
      assert.equal(qsr.buyer, 100000000n)
      assert.equal(qsr.seller, 0n); assert.equal(qsr.feeRecipient, 0n); assert.equal(qsr.royaltyRecipient, 0n)
      const total = [1, 2].filter(id => expected(id).paymentToken.toLowerCase() === token.toLowerCase()).reduce((sum, id) => sum + BigInt(expected(id).price), 0n)
      assert.equal(allowance, requiresApproval ? 0n : total)
      const fees = parseAbi(['function feeBps() view returns (uint256)', 'function feeRecipient() view returns (address)'])
      assert.equal(await at(fixture.exchange, fees, 'feeBps', [], hash), 200n)
      assert.equal((await at(fixture.exchange, fees, 'feeRecipient', [], hash)).toLowerCase(), actors.feeRecipient.toLowerCase())
      const royalty = parseAbi(['function royaltyInfo(uint256,uint256) view returns (address,uint256)'])
      for (const id of [1, 2]) {
        const price = BigInt(expected(id).price)
        const [receiver, amount] = await at(fixture.collection, royalty, 'royaltyInfo', [BigInt(id), price], hash)
        assert.equal(receiver.toLowerCase(), actors.royaltyRecipient.toLowerCase()); assert.equal(amount, price * 375n / 10000n)
      }
      before = {native, qsr, allowance}
      record('actual QSR predeploy bridge balance finite allowance and per-currency payout configuration are independently read')
    }
    assert.ok(before)
    assert.equal(native.buyer, before.native.buyer - nativeGross - BigInt(p.buyerReceiptFee))
    assert.equal(native.seller, before.native.seller + nativeGross - nativeRoyalty - nativeFee - BigInt(p.sellerReceiptFee))
    assert.equal(native.feeRecipient, before.native.feeRecipient + nativeFee)
    assert.equal(native.royaltyRecipient, before.native.royaltyRecipient + nativeRoyalty)
    assert.equal(qsr.buyer, before.qsr.buyer - qsrGross)
    assert.equal(qsr.seller, before.qsr.seller + qsrGross - qsrRoyalty - qsrFee)
    assert.equal(qsr.feeRecipient, before.qsr.feeRecipient + qsrFee)
    assert.equal(qsr.royaltyRecipient, before.qsr.royaltyRecipient + qsrRoyalty)
    const granted = requiresApproval && (value.included || name === 'approval-ordered' || name === 'approval-mined')
    assert.equal(allowance, (granted ? approvalAmount : before.allowance) - qsrGross)
    if (name === 'mined') mined = structuredClone(p)
    if (name === 'replacement') assert.deepEqual({native, qsr, allowance}, before)
    if (name === 'restored') {
      assert.deepEqual(p, mined); restored = true
      record('canonical removal and restoration preserve both currencies gas costs and the exact remaining token allowance')
    }
  }
  async function receipt(receipt, value) {
    const abi = parseAbi(['event OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint8 side, address collection, uint256 tokenId, address paymentToken, uint256 price, uint256 royalty, uint256 fee)'])
    const topic = keccak256(stringToHex('OrderFilled(bytes32,address,address,uint8,address,uint256,address,uint256,uint256,uint256)'))
    const fills = receipt.logs.filter(row => row.address.toLowerCase() === fixture.exchange.toLowerCase() && row.topics[0] === topic)
      .map(row => decodeEventLog({abi, data: row.data, topics: row.topics, strict: true}).args)
    assert.deepEqual(fills.map(row => String(row.tokenId)).sort(), fixture.action === 'batch' ? ['1', '2'] : [partial ? '2' : '1'])
    for (const fill of fills) {
      const selected = expected(fill.tokenId), price = BigInt(selected.price)
      assert.equal(fill.price, price); assert.equal(fill.paymentToken.toLowerCase(), selected.paymentToken.toLowerCase())
      assert.equal(fill.royalty, price * 375n / 10000n); assert.equal(fill.fee, price * 200n / 10000n)
      assert.equal(fill.side, 0); assert.equal(fill.collection.toLowerCase(), fixture.collection.toLowerCase())
      assert.equal(fill.maker.toLowerCase(), fixture.seller.toLowerCase()); assert.equal(fill.taker.toLowerCase(), fixture.buyer.toLowerCase())
      assert.equal(fill.orderHash.toLowerCase(), value.hashes[Number(fill.tokenId) - 1].toLowerCase())
    }
    const raw = await chain.request({method: 'eth_getTransactionReceipt', params: [receipt.transactionHash]})
    assert.equal(raw.blockHash, receipt.blockHash)
    const fee = BigInt(raw.gasUsed) * BigInt(raw.effectiveGasPrice) + BigInt(raw.l1Fee ?? '0x0')
    assert.equal(fee.toString(), value.payouts.buyerPurchaseReceiptFee)
    let approvalFee = 0n
    if (requiresApproval) {
      const approval = await chain.request({method: 'eth_getTransactionReceipt', params: [value.approval]})
      assert.equal(approval.transactionHash, value.approval); assert.equal(approval.status, '0x1')
      assert.equal(approval.from.toLowerCase(), fixture.buyer.toLowerCase())
      assert.equal(approval.to.toLowerCase(), token.toLowerCase())
      approvalFee = BigInt(approval.gasUsed) * BigInt(approval.effectiveGasPrice) + BigInt(approval.l1Fee ?? '0x0')
      assert.ok(approvalFee > 0n)
    }
    assert.equal(approvalFee.toString(), value.payouts.buyerApprovalReceiptFee)
    assert.equal((fee + approvalFee).toString(), value.payouts.buyerReceiptFee)
    if (partial) {
      const seller = await chain.request({method: 'eth_getTransactionReceipt', params: [value.cancellation]})
      assert.equal(seller.transactionHash, value.cancellation)
      assert.equal(seller.status, '0x1')
      assert.equal(seller.from.toLowerCase(), fixture.seller.toLowerCase())
      assert.equal(seller.to.toLowerCase(), fixture.exchange.toLowerCase())
      const sellerFee = BigInt(seller.gasUsed) * BigInt(seller.effectiveGasPrice) + BigInt(seller.l1Fee ?? '0x0')
      assert.ok(sellerFee > 0n); assert.equal(sellerFee.toString(), value.payouts.sellerReceiptFee)
      assert.equal(value.ownerModel.executed.nativeSpent, '0')
      assert.equal(value.ownerModel.executed.nativeUnspent, expected(1).price)
      assert.equal(value.ownerModel.executed.nativeCredit, '0')
    }
    receiptChecked = true
    record('actual token and native fill events preserve exact base units rounded payouts and independently read receipt gas')
  }
  const summary = passed => ({pass: !!passed && !!before && !!mined && receiptChecked && restored,
    configuration: fixture.payouts, tokenPayment: fixture.tokenPayment, canonicalMinedState: mined,
    independentHashSelectedSDKReads: true, actualBridgePredeploy: true,
    finiteExecutedAllowanceBeforeBuying: !requiresApproval, actualProviderApprovalBeforeBuying: !!passed && requiresApproval, missingAllowanceApprovalPathQualified: !!passed && requiresApproval,
    actualAdditionalProviderTransactions: requiresApproval ? 1 : 0, additionalApprovalProviderTransactions: requiresApproval ? 1 : 0, mockedProductAPIOrRPCReplies: 0})
  return {state, receipt, summary}
}
