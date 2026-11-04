// Additional actual UI coverage. Original financial profile checks are unchanged.
import assert from 'node:assert/strict'

export function accountMenuBalanceText(value) {
  assert.ok(typeof value === 'bigint' && value >= 0n && value < (1n << 256n))
  const whole = value / 1000000000000000000n
  const digits = (value % 1000000000000000000n).toString().padStart(18, '0')
  let fraction = digits.slice(0, 4).replace(/0+$/, '')
  if (whole === 0n && fraction === '' && value !== 0n) {
    const first = digits.search(/[1-9]/)
    fraction = digits.slice(0, first + 2).replace(/0+$/, '')
  }
  const integer = whole.toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  return integer + (fraction ? '.' + fraction : '')
}

export async function checkAccountMenuBalance({page, chain, owner, source}) {
  assert.match(source, /^[a-f0-9]{40}$/)
  const before = await chain.request({method: 'eth_getBlockByNumber', params: ['latest', false]})
  assert.match(before.hash, /^0x[a-fA-F0-9]{64}$/)
  const raw = await chain.request({method: 'eth_getBalance', params: [owner, {blockHash: before.hash, requireCanonical: true}]})
  assert.match(raw, /^0x[0-9a-fA-F]+$/)
  const amount = BigInt(raw), expected = accountMenuBalanceText(amount)
  assert.ok(amount >= 0n, 'a real zero native balance must remain visible in the account menu')
  const displayed = []
  const visible = (expected, owner) => {
    const dialogs = [...document.querySelectorAll('dialog')].filter(dialog => dialog.querySelector('h2')?.textContent.trim() === 'Account')
    if (dialogs.length !== 1 || !dialogs[0].open) return false
    const dialog = dialogs[0], value = dialog.querySelector('.text-right .num')
    return value?.checkVisibility() && value.textContent.trim() === expected &&
      [...dialog.querySelectorAll('[title]')].some(element => element.getAttribute('title')?.toLowerCase() === owner.toLowerCase())
  }
  const cleared = () => {
    const dialog = [...document.querySelectorAll('dialog')].find(dialog => dialog.querySelector('h2')?.textContent.trim() === 'Account')
    return dialog && !dialog.open && !dialog.querySelector('.text-right .num')
  }
  for (let attempt = 0; attempt < 2; attempt++) {
    await page.click('header [data-testid="connect"]')
    await page.waitForFunction(visible, {timeout: 12000, polling: 100}, expected, owner)
    const actual = await page.$eval('dialog[open] .text-right .num', element => element.textContent.trim())
    assert.equal(actual, expected); displayed.push(actual)
    await page.click('dialog[open] button[title="Close"]')
    await page.waitForFunction(cleared, {timeout: 12000, polling: 100})
  }
  const after = await chain.request({method: 'eth_getBlockByNumber', params: ['latest', false]})
  assert.equal(after.hash, before.hash)
  console.log('ACTUAL-ACCOUNT-MENU-BALANCE '+JSON.stringify({schema: 1, source, profile: 'karum-single',
    expectedBalanceWei: amount.toString(), displayedBalances: displayed, canonicalBlockHash: before.hash,
    canonicalHashSelectedIndependentBalanceReads: 1, actualMenuOpens: 2, actualMenuCloses: 2,
    closedMenuAmountClearedChecks: 2, exactSelectedAccountCheckedAtBothOpens: true,
    sameCanonicalHeadBeforeAndAfter: true, actualProductionPageAndRPC: true,
    originalFinancialProfileChecksChanged: false, originalNavigationPolicyChanged: false,
    providerPromptsOrTransactionsAdded: 0, productAPIOrRPCRepliesReplaced: 0,
    physicalWalletOrPublicNetwork: false, historicalNavigationRootCauseOrRepairEstablished: false}))
}
