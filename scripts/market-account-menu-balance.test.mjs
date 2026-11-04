// Independent fixed examples for the observer's visible decimal comparison.
import assert from 'node:assert/strict'
import {accountMenuBalanceText} from './market-account-menu-balance.mjs'
for (const [amount, expected] of [[0n, '0'], [1n, '0.000000000000000001'], [1000000000n, '0.000000001'],
  [1234567890123456789n, '1.2345'], [1000000000000000000000n, '1,000']]) {
  assert.equal(accountMenuBalanceText(amount), expected)
}
for (const amount of [-1n, 1n << 256n, 1]) assert.throws(() => accountMenuBalanceText(amount))
console.log('ACCOUNT-MENU-BALANCE-CONTROLS '+JSON.stringify({cases: 8, pass: true, browserExecutions: 0,
  originalFinancialProfileChecksChanged: false, productAPIOrRPCRepliesReplaced: 0}))
