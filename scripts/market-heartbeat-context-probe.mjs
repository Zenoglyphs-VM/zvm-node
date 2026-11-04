// Hold and continue one original height request from the production page.
// The picker and endpoint watcher run normally; no reply or product state is replaced.
import assert from 'node:assert/strict'

const quantity = value => typeof value === 'string' && /^0x(?:0|[1-9a-f][0-9a-f]*)$/i.test(value)

export function createHeartbeatContextProbe({origin, enabled, continueRequest}) {
  const base = new URL(origin)
  assert.ok(base.protocol === 'http:' && base.hostname === '127.0.0.1' && base.port &&
    base.pathname === '/' && !base.search && !base.hash && !base.username && !base.password)
  assert.equal(typeof enabled, 'boolean'); assert.equal(typeof continueRequest, 'function')
  let armed = null, held = null, holds = 0, releases = 0, failed = false
  return Object.freeze({
    arm(page) {
      assert.ok(enabled && !failed && !armed && !held && holds === 0 && releases === 0)
      assert.ok(page && typeof page === 'object')
      let ready
      const selected = new Promise(resolve => {ready = resolve})
      armed = {page, ready}
      return selected
    },
    intercept(request, page) {
      if (!armed || page !== armed.page) return false
      let body, batch
      try {
        const target = new URL(request.url()), raw = request.postData()
        if (target.origin !== base.origin || target.pathname !== '/rpc' || target.search ||
            request.method() !== 'POST' || typeof raw !== 'string' || raw.length > 4096) return false
        body = JSON.parse(raw); batch = Array.isArray(body)
        if (batch) {if (body.length !== 1) return false; body = body[0]}
        if (!body || typeof body !== 'object' || Array.isArray(body) || body.jsonrpc !== '2.0' ||
            !((Number.isSafeInteger(body.id) && body.id >= 0) || (typeof body.id === 'string' && body.id.length > 0 && body.id.length <= 128)) ||
            body.method !== 'eth_blockNumber' || (Object.hasOwn(body, 'params') && (!Array.isArray(body.params) || body.params.length !== 0))) return false
      } catch {return false}
      const selected = armed; armed = null
      held = {page, request, id: body.id, batch}; holds++
      selected.ready()
      return true
    },
    async release(page, expectedHeight) {
      assert.ok(!failed && !armed && held && page === held.page && quantity(expectedHeight))
      const selected = held; held = null
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
        assert.equal(BigInt(envelope.result), BigInt(expectedHeight))
        releases++
        return {actualOriginalHeightRequestContinued: true,
          actualMatchedHeight: BigInt(envelope.result).toString(), actualOriginalRPCBatchEntries: selected.batch ? 1 : 0}
      } catch (error) {failed = true; throw error}
    },
    assertIdle() {
      assert.ok(enabled && !failed && !armed && !held && holds === 1 && releases === 1)
      return {heldOriginalRequests: holds, continuedOriginalRequests: releases, replacedRequestsOrReplies: 0}
    },
    async dispose() {
      armed = null
      if (held) {
        const selected = held; held = null
        await continueRequest(selected.request)
      }
    },
  })
}

function pickerState() {
  const dialogs = [...document.querySelectorAll('dialog')].filter(dialog => dialog.open &&
    dialog.querySelector('h2')?.textContent.trim() === 'Node')
  if (dialogs.length !== 1) return null
  const dialog = dialogs[0], fields = {}
  for (const label of dialog.querySelectorAll('dt')) fields[label.textContent.trim()] = label.parentElement.querySelector('dd')?.textContent.trim()
  return {status: fields.Status, chain: fields.Chain ?? null, block: fields.Block ?? null,
    exchange: fields.Exchange ?? null, invalidURL: dialog.textContent.includes('That is not a URL.')}
}

async function bounded(promise) {
  let timer
  try {return await Promise.race([promise, new Promise((_, reject) => {timer = setTimeout(() => reject(new Error('heartbeat height request was not observed')), 12000)})])}
  finally {clearTimeout(timer)}
}

async function recheck(page) {
  await page.waitForFunction(() => [...document.querySelectorAll('dialog[open] button')].some(button =>
    button.textContent.trim() === 'Check again' && !button.disabled), {timeout: 12000, polling: 50})
  await page.evaluate(() => {
    const dialog = [...document.querySelectorAll('dialog[open]')].find(value => value.querySelector('h2')?.textContent.trim() === 'Node')
    const button = [...dialog.querySelectorAll('button')].find(value => value.textContent.trim() === 'Check again')
    if (!button || button.disabled) throw new Error('node recheck unavailable')
    button.click()
  })
}

export async function checkHeartbeatContext({page, chain, source, origin, probe, effectCounts}) {
  assert.match(source, /^[a-f0-9]{40}$/)
  assert.equal(typeof effectCounts, 'function')
  const effects = effectCounts()
  assert.deepEqual(Object.keys(effects).sort(), ['orderPOSTs', 'providerAccountRequests', 'providerTransactions', 'typedDataSignatures'])
  assert.ok(Object.values(effects).every(value => Number.isSafeInteger(value) && value >= 0))
  const rpc = new URL('/rpc', origin).href
  const before = await chain.request({method: 'eth_getBlockByNumber', params: ['latest', false]})
  assert.ok(quantity(before.number)); assert.match(before.hash, /^0x[a-fA-F0-9]{64}$/)
  await page.click('[data-testid="node-picker"]')
  await page.waitForFunction(height => [...document.querySelectorAll('dialog[open] dt')].some(label =>
    label.textContent.trim() === 'Block' && label.parentElement.querySelector('dd')?.textContent.trim().replaceAll(',', '') === height),
  {timeout: 12000, polling: 50}, BigInt(before.number).toString())
  // Select the next ordinary heartbeat while this endpoint stays healthy.
  // The pinned production heartbeat is the page's sole height-method producer.
  const selected = probe.arm(page)
  await bounded(selected)
  await page.waitForFunction(() => [...document.querySelectorAll('dialog[open] dd')].some(value => value.textContent.trim() === 'Reachable, market open') &&
    ![...document.querySelectorAll('dialog[open] button')].find(value => value.textContent.trim() === 'Check again')?.disabled, {timeout: 12000, polling: 50})
  const pending = await page.evaluate(pickerState)
  assert.equal(pending.status, 'Reachable, market open'); assert.ok(pending.chain && pending.exchange)
  assert.equal(pending.block.replaceAll(',', ''), BigInt(before.number).toString(), 'the selected live heartbeat context changed before invalidation')
  await page.$eval('#rpc-url', input => {input.value = 'http://['; input.dispatchEvent(new Event('input', {bubbles: true}))})
  await page.click('dialog[open] form button[type="submit"]')
  await page.waitForFunction(() => [...document.querySelectorAll('dialog[open]')].some(dialog => dialog.textContent.includes('That is not a URL.')), {timeout: 12000, polling: 50})
  const invalid = await page.evaluate(pickerState)
  assert.equal(invalid.status, 'Not reachable'); assert.equal(invalid.chain, null)
  assert.equal(invalid.block, null); assert.equal(invalid.exchange, null); assert.ok(invalid.invalidURL)
  await page.evaluate(() => {
    const facts = {samples: 0, staleBlockSamples: 0}
    const observer = new MutationObserver(() => {
      const dialog = [...document.querySelectorAll('dialog[open]')].find(value => value.querySelector('h2')?.textContent.trim() === 'Node')
      if (!dialog) return
      facts.samples++
      if ([...dialog.querySelectorAll('dt')].some(label => label.textContent.trim() === 'Block')) facts.staleBlockSamples++
    })
    observer.observe(document.body, {subtree: true, childList: true, characterData: true, attributes: true})
    window.__executedHeartbeatContextObservation = {observer, facts}
  })
  const originalResponse = await probe.release(page, before.number)
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
  const stale = await page.evaluate(pickerState)
  const observed = await page.evaluate(() => {
    const {observer, facts} = window.__executedHeartbeatContextObservation
    observer.disconnect(); delete window.__executedHeartbeatContextObservation
    return facts
  })
  assert.deepEqual(stale, invalid, 'the previous height response repopulated the refused node')
  assert.equal(observed.staleBlockSamples, 0)
  // Invalid input is refused before replacing the selected URL. Retry that
  // original node through the ordinary picker action rather than product hooks.
  assert.equal(await page.$eval('[data-testid="node-picker"]', button => button.getAttribute('title')), rpc)
  await recheck(page)
  await page.waitForFunction(() => [...document.querySelectorAll('dialog[open] dt')].some(label => label.textContent.trim() === 'Block'), {timeout: 12000, polling: 50})
  const restored = await page.evaluate(pickerState)
  assert.equal(restored.status, 'Reachable, market open'); assert.equal(restored.chain, pending.chain)
  assert.equal(restored.exchange, pending.exchange); assert.equal(restored.block.replaceAll(',', ''), BigInt(before.number).toString())
  await page.click('dialog[open] button[title="Close"]')
  const after = await chain.request({method: 'eth_getBlockByNumber', params: ['latest', false]})
  assert.equal(after.hash, before.hash)
  assert.deepEqual(effectCounts(), effects)
  console.log('ACTUAL-HEARTBEAT-CONTEXT '+JSON.stringify({schema: 2, source, profile: 'karum-single',
    canonicalBlockHash: before.hash, actualNodePickerRechecks: 1, actualInvalidURLSelections: 1,
    actualOriginalNodeRestorations: 1, selectedLiveHeartbeatRequestHeld: true,
    oldHeightResponseReleasedAfterInvalidSelection: true, staleBlockMutationSamples: observed.staleBlockSamples,
    actualProductionPageAndRPC: true, sameCanonicalHeadBeforeAndAfter: true, ...originalResponse, ...probe.assertIdle(),
    unchangedOriginalFinancialEffectCounts: effects,
    originalFinancialProfileChecksChanged: false, originalNavigationPolicyChanged: false,
    providerPromptsOrTransactionsAdded: 0, productAPIOrRPCRepliesReplaced: 0, physicalWalletOrPublicNetwork: false}))
}
