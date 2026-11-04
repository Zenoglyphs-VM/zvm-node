// Additional production-page observations using the fixture's existing accounts.
// Retain original requests and replies. No product APIs, RPC results or state are replaced.
import assert from 'node:assert/strict'
import {accountMenuBalanceText} from './market-account-menu-balance.mjs'

const address = value => typeof value === 'string' && /^0x[0-9a-fA-F]{40}$/.test(value)
const quantity = value => typeof value === 'string' && /^0x(?:0|[1-9a-f][0-9a-f]*)$/i.test(value)

export function createAccountMenuContextProbe({origin, enabled, continueRequest}) {
  const base = new URL(origin)
  assert.ok(base.protocol === 'http:' && base.hostname === '127.0.0.1' && base.port &&
    !base.username && !base.password && base.pathname === '/' && !base.search && !base.hash)
  assert.equal(typeof enabled, 'boolean'); assert.equal(typeof continueRequest, 'function')
  const seen = new WeakSet()
  let armed = null, held = null, releases = 0, holds = 0, failed = false
  return Object.freeze({
    arm(page, owner) {
      assert.ok(enabled && !failed && !armed && !held && holds === releases && holds < 2)
      assert.ok(page && typeof page === 'object' && address(owner))
      let ready
      const selected = new Promise(resolve => {ready = resolve})
      armed = {page, owner: owner.toLowerCase(), ready}
      return selected
    },
    intercept(request, page) {
      if (!armed || page !== armed.page || seen.has(request)) return false
      let body, batch
      try {
        const target = new URL(request.url()), raw = request.postData()
        if (target.origin !== base.origin || target.pathname !== '/rpc' || target.search ||
            request.method() !== 'POST' || typeof raw !== 'string' || raw.length > 4096) return false
        body = JSON.parse(raw)
        batch = Array.isArray(body)
        if (batch) {if (body.length !== 1) return false; body = body[0]}
        if (!body || typeof body !== 'object' || Array.isArray(body) || body.jsonrpc !== '2.0' ||
            !((Number.isSafeInteger(body.id) && body.id >= 0) || (typeof body.id === 'string' && body.id.length > 0 && body.id.length <= 128)) ||
            body.method !== 'eth_getBalance' || !Array.isArray(body.params) || body.params.length !== 2 ||
            !address(body.params[0]) || body.params[0].toLowerCase() !== armed.owner || body.params[1] !== 'latest') return false
      } catch {return false}
      const selected = armed; armed = null; holds++; seen.add(request)
      held = {request, page, id: body.id, batch}
      selected.ready()
      return true
    },
    async release(page, expectedQuantity) {
      assert.ok(!failed && !armed && held && page === held.page && quantity(expectedQuantity))
      const selected = held; held = null
      // Start observation before the original request is continued exactly once.
      const responseReady = page.waitForResponse(response => response.request() === selected.request, {timeout: 12000})
      void responseReady.catch(() => {})
      try {
        await continueRequest(selected.request)
        const response = await responseReady
        assert.equal(response.request(), selected.request); assert.equal(response.status(), 200)
        const received = await response.json()
        if (selected.batch) assert.ok(Array.isArray(received) && received.length === 1)
        else assert.ok(!Array.isArray(received))
        const envelope = selected.batch ? received[0] : received
        assert.ok(envelope && !Array.isArray(envelope) && envelope.jsonrpc === '2.0' &&
          envelope.id === selected.id && !Object.hasOwn(envelope, 'error') && quantity(envelope.result))
        assert.equal(BigInt(envelope.result), BigInt(expectedQuantity))
        releases++
        return {actualOriginalRequestContinued: true, actualMatchedResponseAmount: BigInt(envelope.result).toString(), actualOriginalRPCBatchEntries: selected.batch ? 1 : 0}
      } catch (error) {failed = true; throw error}
    },
    assertIdle() {
      assert.ok(enabled && !failed && !armed && !held && holds === 2 && releases === 2)
      return {heldOriginalRequests: holds, continuedOriginalRequests: releases, replacedRequestsOrReplies: 0}
    },
  })
}

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
async function bounded(promise) {
  let timer
  try {return await Promise.race([promise, new Promise((_, reject) => {timer = setTimeout(() => reject(new Error('account balance request was not observed')), 12000)})])}
  finally {clearTimeout(timer)}
}

export async function checkAccountMenuContext({page, chain, owner, buyer, source, probe}) {
  assert.ok(address(owner) && address(buyer) && owner.toLowerCase() !== buyer.toLowerCase())
  assert.match(source, /^[a-f0-9]{40}$/)
  const before = await chain.request({method: 'eth_getBlockByNumber', params: ['latest', false]})
  assert.match(before.hash, /^0x[a-fA-F0-9]{64}$/)
  const read = account => chain.request({method: 'eth_getBalance', params: [account, {blockHash: before.hash, requireCanonical: true}]})
  const [ownerRaw, buyerRaw] = await Promise.all([read(owner), read(buyer)])
  assert.ok(quantity(ownerRaw) && quantity(buyerRaw))
  assert.equal(BigInt(ownerRaw), 0n); assert.ok(BigInt(buyerRaw) > 0n)
  const texts = {owner: accountMenuBalanceText(BigInt(ownerRaw)), buyer: accountMenuBalanceText(BigInt(buyerRaw))}
  const evidence = []
  for (const step of [
    {from: owner, to: buyer, expected: texts.buyer, oldRaw: ownerRaw},
    {from: buyer, to: owner, expected: texts.owner, oldRaw: buyerRaw},
  ]) {
    const selected = probe.arm(page, step.from)
    await page.click('header [data-testid="connect"]')
    await bounded(selected)
    await page.waitForFunction(owner => {
      const dialog = [...document.querySelectorAll('dialog')].find(dialog => dialog.querySelector('h2')?.textContent.trim() === 'Account')
      return dialog?.open && !dialog.querySelector('.text-right .num') &&
        [...dialog.querySelectorAll('[title]')].some(element => element.getAttribute('title')?.toLowerCase() === owner.toLowerCase())
    }, {timeout: 12000, polling: 50}, step.from)
    // Emit the fixture's standard provider event. The product wallet handler,
    // Vue watcher, original public client and original Node RPC execute normally.
    await page.evaluate(account => window.__executedMarketSelectAccount(account), step.to)
    await page.waitForFunction(visible, {timeout: 12000, polling: 50}, step.expected, step.to)
    await page.evaluate((owner, expected) => {
      const facts = {samples: 0, mismatches: 0}
      const observer = new MutationObserver(() => {
        const dialog = [...document.querySelectorAll('dialog')].find(dialog => dialog.querySelector('h2')?.textContent.trim() === 'Account')
        if (!dialog?.open) return
        const selected = [...dialog.querySelectorAll('[title]')].some(element => element.getAttribute('title')?.toLowerCase() === owner.toLowerCase())
        const amount = dialog.querySelector('.text-right .num')
        if (selected && amount?.checkVisibility()) {
          facts.samples++; if (amount.textContent.trim() !== expected) facts.mismatches++
        }
      })
      observer.observe(document.body, {subtree: true, childList: true, characterData: true, attributes: true})
      window.__executedMenuContextObservation = {observer, facts}
    }, step.to, step.expected)
    const actualResponse = await probe.release(page, step.oldRaw)
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
    assert.ok(await page.evaluate(visible, step.expected, step.to), 'the previous account response replaced the selected account amount')
    const observed = await page.evaluate(() => {
      const {observer, facts} = window.__executedMenuContextObservation
      observer.disconnect(); delete window.__executedMenuContextObservation
      return facts
    })
    assert.equal(observed.mismatches, 0)
    const displayed = await page.$eval('dialog[open] .text-right .num', element => element.textContent.trim())
    assert.equal(displayed, step.expected)
    evidence.push({...actualResponse, displayed, selectedAccountConfirmed: true, mismatchedVisibleMutationSamples: observed.mismatches})
    await page.click('dialog[open] button[title="Close"]')
    await page.waitForFunction(cleared, {timeout: 12000, polling: 50})
  }
  const after = await chain.request({method: 'eth_getBlockByNumber', params: ['latest', false]})
  assert.equal(after.hash, before.hash)
  const idle = probe.assertIdle()
  console.log('ACTUAL-ACCOUNT-MENU-CONTEXT '+JSON.stringify({schema: 1, source, profile: 'karum-single',
    canonicalBlockHash: before.hash, canonicalHashSelectedIndependentBalanceReads: 2,
    initialOwnerBalanceWei: BigInt(ownerRaw).toString(), positiveBuyerBalanceWei: BigInt(buyerRaw).toString(),
    actualProviderAccountChanges: 2, actualAdditionalMenuOpens: 2, actualAdditionalMenuCloses: 2,
    closedMenuAmountClearedChecks: 2, sameCanonicalHeadBeforeAndAfter: true, originalSellerContextRestored: true,
    actualProductionPageAndRPC: true, delayedResponseComparisons: evidence, ...idle,
    originalFinancialProfileChecksChanged: false, originalNavigationPolicyChanged: false,
    providerPromptsOrTransactionsAdded: 0, productAPIOrRPCRepliesReplaced: 0, physicalWalletOrPublicNetwork: false}))
}
