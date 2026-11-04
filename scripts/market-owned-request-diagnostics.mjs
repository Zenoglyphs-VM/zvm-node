// Compose existing purpose marking with independent non-completing request context.
import {createOwnedRPCCorrelation} from './market-rpc-correlation-support.mjs'
import {createQuoteRequestPurposeObserver} from './market-quote-request-purpose.mjs'
import {createOwnedRPCRequestContext} from './market-rpc-request-context.mjs'

export function createOwnedRequestDiagnostics(origin, fixture, codec) {
  const quotePurpose = createQuoteRequestPurposeObserver()
  const requestContext = createOwnedRPCRequestContext(origin, {
    targets: {collection: fixture.collection, exchange: fixture.exchange, token: fixture.tokenPayment?.token},
    selectors() {
      const {encodeFunctionData, parseAbi} = codec(), zero = '0x' + '0'.repeat(40)
      const specs = [
        ['APPROVAL_FOR_ALL', 'function isApprovedForAll(address owner, address operator) view returns (bool)', 'isApprovedForAll', [zero, zero]],
        ['ALLOWANCE', 'function allowance(address owner, address spender) view returns (uint256)', 'allowance', [zero, zero]],
        ['TOKEN_BALANCE', 'function balanceOf(address owner) view returns (uint256)', 'balanceOf', [zero]],
        ['TOKEN_OWNER', 'function ownerOf(uint256 id) view returns (address)', 'ownerOf', [1n]],
        ['ROYALTY_INFO', 'function royaltyInfo(uint256 id, uint256 price) view returns (address,uint256)', 'royaltyInfo', [1n, 1n]],
        ['FEE_BPS', 'function feeBps() view returns (uint256)', 'feeBps', []],
        ['FEE_RECIPIENT', 'function feeRecipient() view returns (address)', 'feeRecipient', []],
      ]
      return Object.fromEntries(specs.map(([kind, abi, functionName, args]) =>
        [kind, encodeFunctionData({abi: parseAbi([abi]), functionName, args}).slice(0, 10).toLowerCase()]))
    },
  })
  const rpcCorrelation = createOwnedRPCCorrelation(origin, {requestPurpose: quotePurpose.describe, requestContext})
  return {rpcCorrelation, quotePurpose}
}
