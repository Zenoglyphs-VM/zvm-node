// Actual production listing controls, real EIP-712 signatures from a public
// test signer and the real Go node/market/RPC fixture. No market or RPC reply
// is fabricated here. The public buyer signs ordinary EVM transactions for
// the actual relayer CLI; independent NoM and daemon RPC remain boundaries.
import assert from 'node:assert/strict'
import {execFileSync} from 'node:child_process'
import {existsSync} from 'node:fs'
import {readFile, mkdtemp, rm} from 'node:fs/promises'
import {createRequire} from 'node:module'
import {tmpdir} from 'node:os'
import {join} from 'node:path'
import {pathToFileURL} from 'node:url'

let rawInput = ''
for await (const chunk of process.stdin) {
  rawInput += chunk
  assert.ok(rawInput.length <= 16384, 'bounded fixture input required')
}
const fixture = JSON.parse(rawInput), base = new URL(fixture.base)
assert.equal(base.protocol, 'http:')
assert.equal(base.hostname, '127.0.0.1')
assert.ok(base.port && /^[a-f0-9]{64}$/.test(fixture.token))
assert.ok(['nft', 'karum'].includes(fixture.app) && ['single', 'bulk'].includes(fixture.kind))
assert.ok(['buy', 'cancel', 'nonce', 'approval', 'batch', 'partial-batch'].includes(fixture.action))
const batchMode = ['batch', 'partial-batch'].includes(fixture.action)
const partialBatch = fixture.action === 'partial-batch'
const pins = JSON.parse(await readFile(new URL('./market-browser-pins.json', import.meta.url)))
const git = (root, ...args) => execFileSync('git', ['--no-replace-objects', ...args], {cwd: root, encoding: 'utf8'}).trim()
for (const app of ['nft', 'karum']) {
  assert.equal(git(fixture.roots[app], 'rev-parse', 'HEAD'), pins['zvm-' + app])
  assert.equal(git(join(fixture.roots[app], 'zvm-core'), 'rev-parse', 'HEAD'), pins['shared-core'])
}
const requireApp = createRequire(join(fixture.roots[fixture.app], 'package.json'))
const requireKarum = createRequire(join(fixture.roots.karum, 'package.json'))
const puppeteer = (await import(pathToFileURL(requireKarum.resolve('puppeteer-core')).href)).default
const sdk = JSON.parse(await readFile(join(fixture.roots[fixture.app], 'node_modules/viem/package.json')))
assert.equal(sdk.version, '2.56.8')
// The scalar-one signer is a deliberately public disposable test identity.
const signer = requireApp('viem/accounts').privateKeyToAccount('0x' + '0'.repeat(63) + '1')
const buyerSigner = requireApp('viem/accounts').privateKeyToAccount('0x' + '0'.repeat(63) + '2')
assert.equal(buyerSigner.address.toLowerCase(), fixture.buyer.toLowerCase())
const {createPublicClient, http, decodeEventLog, keccak256, parseAbi, stringToHex} = requireApp('viem')
const chain = createPublicClient({transport: http(new URL('/rpc', base).href, {retryCount: 0})})
const transactions = []
const actorSigner = fixture.action === 'buy' || batchMode ? buyerSigner : signer
assert.equal(actorSigner.address.toLowerCase(), fixture.actor.toLowerCase())
const targetAddress = fixture.action === 'approval' ? fixture.collection : fixture.exchange
let fundedNonce, fundedSellerNonce
let releaseBuyer, heldBuyerReadyResolve
const buyerGate = new Promise(resolve => {releaseBuyer = resolve})
const heldBuyerReady = new Promise(resolve => {heldBuyerReadyResolve = resolve})
assert.equal(signer.address.toLowerCase(), fixture.seller.toLowerCase())
const executablePath = [process.env.CHROME_BIN, '/usr/bin/google-chrome', '/usr/bin/chromium'].find(p => p && existsSync(p))
assert.ok(executablePath, 'preinstalled Chrome required; no browser download fallback')
const profile = await mkdtemp(join(tmpdir(), 'zvm-signed-market-'))
const result = {app: fixture.app, kind: fixture.kind, action: fixture.action, checks: [], pass: false}
const signatures = [], posts = [], methods = [], failedHTTP = [], exceptions = [], external = [], providerFailures = []
let browser, context, page, chrome, stopPromise
const stop = () => stopPromise ??= (async () => {if (browser) await browser.close()})()
process.once('SIGTERM', () => {void stop()})

function inject({seller, buyer, chainId, actor, action}) {
  const viewer = new URL(location.href).searchParams.get('__viewer') === 'buyer'
  const state = {accounts: [viewer ? buyer : seller], chain: '0x' + BigInt(chainId).toString(16)}
  const listeners = new Map()
  const provider = {
    on(name, fn) {const set = listeners.get(name) || new Set(); set.add(fn); listeners.set(name, set); return provider},
    removeListener(name, fn) {listeners.get(name)?.delete(fn); return provider},
    async request({method, params = []}) {
      await window.__executedMarketMethod(method)
      if (method === 'eth_accounts' || method === 'eth_requestAccounts') return state.accounts
      if (method === 'eth_chainId') return state.chain
      if (method === 'wallet_switchEthereumChain') {
        if (params[0]?.chainId !== state.chain) throw new Error('unexpected fixture network')
        for (const fn of listeners.get('chainChanged') || []) fn(state.chain)
        return null
      }
      if (method === 'eth_signTypedData_v4') {
        if (state.accounts[0].toLowerCase() !== seller.toLowerCase()) throw new Error('only the public fixture trader may sign')
        return window.__executedMarketSign(params)
      }
      if (method === 'eth_sendTransaction') {
        if (state.accounts[0].toLowerCase() !== actor.toLowerCase() && !(action === 'partial-batch' && state.accounts[0].toLowerCase() === seller.toLowerCase())) throw new Error('only the selected public fixture actors may send')
        return window.__executedMarketSend(params)
      }
      throw new Error('unsupported fixture wallet method: ' + method)
    },
  }
  const announce = () => window.dispatchEvent(new CustomEvent('eip6963:announceProvider', {detail: Object.freeze({
    info: {uuid: 'e5b6718a-5392-4b82-9999-000000000001', name: 'Fixture wallet', icon: '', rdns: 'test.zvm'}, provider})}))
  window.addEventListener('eip6963:requestProvider', announce)
}

const record = name => {
  result.checks.push(name)
  console.log('SIGNED-BROWSER-EVENT ' + JSON.stringify({app: fixture.app, kind: fixture.kind, check: name, pass: true}))
}
async function until(fn, name, ...args) {
  await page.waitForFunction(fn, {timeout: 12000, polling: 100}, ...args).catch(() => {throw new Error(name)})
}
async function click(text) {
  const action = (value, press) => {
    const button = [...document.querySelectorAll('button')].find(b => b.checkVisibility() && !b.disabled && b.textContent.trim() === value)
    if (press) button?.click()
    return !!button
  }
  await until(action, 'actual button unavailable: ' + text, text, false)
  assert.ok(await page.evaluate(action, text, true))
}
function url(path, viewer = false) {
  const value = new URL('/zvm-' + fixture.app + path, base)
  value.searchParams.set('rpc', new URL('/rpc', base).href)
  if (viewer) value.searchParams.set('__viewer', 'buyer')
  return value.href
}
async function navigate(path, viewer = false) {
  await page.goto(url(path, viewer), {waitUntil: 'networkidle0', timeout: 20000})
}
async function connect() {
  const needed = await page.evaluate(() => [...document.querySelectorAll('header button')]
    .some(b => b.checkVisibility() && b.textContent.trim() === 'Connect wallet'))
  if (needed) {await click('Connect wallet'); await click('Fixture wallet')}
}
async function phase(name) {
  const response = await fetch(new URL('/__fixture/phase?name=' + name, base), {
    method: 'POST', headers: {'X-ZVM-Fixture': fixture.token}, signal: AbortSignal.timeout(20000)})
  assert.equal(response.status, 200)
  const state = await response.json()
  assert.equal(state.phase, name)
  assert.equal(state.actualAcceptedBrowserInputs, true)
  assert.equal(state.alreadyDerivedSourcePrefixPreserved, true)
  assert.equal(state.hashes.length, 2)
  return state
}
async function orderRows() {
  const response = await fetch(new URL('/api/nft/orders?all=1&collection=' + fixture.collection, base), {signal: AbortSignal.timeout(5000)})
  assert.equal(response.status, 200)
  return response.json()
}
async function waitOrders(number) {
  const deadline = Date.now() + 12000
  while (Date.now() < deadline) {
    const rows = await orderRows()
    if (rows.length === number && rows.every(r => r.status === 'open' && r.code === 0)) return rows
    await new Promise(resolve => setTimeout(resolve, 100))
  }
  throw new Error('actual signed orders did not reach the exchange-checked book')
}
async function projection(name, filled) {
  for (const id of [1, 2]) {
    await navigate('/item/' + fixture.collection + '/' + id, true)
    await until(id => document.querySelector('main h1')?.textContent.trim() === '#' + id, 'actual signed item page did not render', id)
    const listed = await page.evaluate(app => {
      // Karum renders independently purchasable sibling cards in the same
      // main element. Only the current item's price-card action is relevant.
      const buttons = document.querySelectorAll(app === 'karum' ? 'main button[data-testid="buy"]' : 'main button')
      return [...buttons].some(b => b.checkVisibility() && (app === 'karum' || b.textContent.trim() === 'Buy now'))
    }, fixture.app)
    assert.equal(listed, id - 1 !== filled, 'actual page fillability differs from executed canonical state')
    const owner = id - 1 === filled ? fixture.buyer : fixture.seller
    assert.ok(await page.evaluate(owner => [...document.querySelectorAll('main a')].some(a =>
      a.getAttribute('href')?.toLowerCase().includes('/account/' + owner.toLowerCase())), owner), 'executed owner link missing')
    record(name + ' actual item ' + id + ' owner and listing')
  }
}

async function batchProjection(name, stage) {
  for (const id of [1, 2]) {
    await navigate('/item/' + fixture.collection + '/' + id, true)
    await until(id => document.querySelector('main h1')?.textContent.trim() === '#' + id, 'batch item page did not render', id)
    const available = await page.evaluate(app => [...document.querySelectorAll(app === 'karum' ? 'main button[data-testid="buy"]' : 'main button')]
      .some(button => button.checkVisibility() && (app === 'karum' || button.textContent.trim() === 'Buy now')), fixture.app)
    assert.equal(available, stage === 0 || (partialBatch && stage === 1 && id === 2))
    const owner = stage === 2 && (!partialBatch || id === 2) ? fixture.buyer : fixture.seller
    assert.ok(await page.evaluate(owner => [...document.querySelectorAll('main a')].some(link =>
      link.getAttribute('href')?.toLowerCase().includes('/account/' + owner.toLowerCase())), owner))
    record(name + ' actual batch item ' + id + ' owner and listing')
  }
}

async function boundedReady(promise, label) {
  let timer
  try {
    await Promise.race([promise, new Promise((_, reject) => {timer = setTimeout(() => reject(new Error(label)), 12000)})])
  } finally {clearTimeout(timer)}
}

async function preparePage(page) {
  await page.setViewport({width: 1360, height: 1000})
  await page.exposeFunction('__executedMarketMethod', method => {assert.ok(methods.length < 128); methods.push(method)})
  await page.exposeFunction('__executedMarketSign', async params => {
    assert.equal(params.length, 2)
    assert.equal(params[0].toLowerCase(), fixture.seller.toLowerCase())
    assert.ok(typeof params[1] === 'string' && params[1].length <= 16384)
    const typed = JSON.parse(params[1])
    assert.equal(BigInt(typed.domain.chainId), BigInt(fixture.chainId))
    assert.equal(typed.domain.verifyingContract.toLowerCase(), fixture.exchange.toLowerCase())
    assert.equal(typed.primaryType, fixture.kind === 'bulk' ? 'OrderRoot' : 'Order')
    assert.ok(signatures.length < 2)
    const signature = await signer.signTypedData({...typed, domain: {...typed.domain, chainId: Number(BigInt(typed.domain.chainId))}})
    signatures.push({primaryType: typed.primaryType})
    return signature
  })
  await page.exposeFunction('__executedMarketSend', async params => {
    try {
    assert.equal(params.length, 1)
    const request = structuredClone(params[0])
    const fromBuyer = request.from.toLowerCase() === fixture.buyer.toLowerCase()
    assert.ok(request.from.toLowerCase() === fixture.actor.toLowerCase() ||
      (partialBatch && request.from.toLowerCase() === fixture.seller.toLowerCase()))
    assert.equal(request.to.toLowerCase(), targetAddress.toLowerCase())
    const value = fromBuyer && batchMode ? 500000000000000000n : fixture.action === 'buy' ? 250000000000000000n : 0n
    assert.equal(BigInt(request.value ?? '0x0'), value)
    if (request.chainId !== undefined) assert.equal(BigInt(request.chainId), BigInt(fixture.chainId))
    if (partialBatch && fromBuyer) {
      assert.equal(transactions.length, 0)
      heldBuyerReadyResolve()
      await buyerGate
      assert.equal(transactions.length, 1)
    } else assert.equal(transactions.length, 0)
    const account = partialBatch && !fromBuyer ? signer : actorSigner
    const nonce = await chain.getTransactionCount({address: account.address, blockTag: 'pending'})
    assert.equal(nonce, partialBatch && !fromBuyer ? fundedSellerNonce : fundedNonce)
    if (request.nonce !== undefined) assert.equal(BigInt(request.nonce), BigInt(nonce))
    const serializedTransaction = await account.signTransaction({type: 'eip1559', chainId: fixture.chainId,
      nonce, to: request.to, data: request.data, value, gas: BigInt(request.gas),
      maxFeePerGas: BigInt(request.maxFeePerGas), maxPriorityFeePerGas: BigInt(request.maxPriorityFeePerGas)})
    const hash = await chain.sendRawTransaction({serializedTransaction})
    assert.ok(!transactions.includes(hash))
    transactions.push(hash)
    return hash
    } catch (error) {
      const causes = []
      let cause = error
      for (let i = 0; i < 4 && cause; i++, cause = cause.cause) {
        causes.push(String(cause.details ?? cause.shortMessage ?? cause.message ?? cause).slice(0, 500))
      }
      const failure = {app: fixture.app, kind: fixture.kind, action: fixture.action, causes}
      if (providerFailures.length < 4) providerFailures.push(failure)
      console.error('SIGNED-PROVIDER-FAILURE ' + JSON.stringify(failure))
      throw error
    }
  })
  await page.evaluateOnNewDocument(inject, fixture)
  await page.setRequestInterception(true)
  page.on('request', request => {
    const target = new URL(request.url())
    if (target.protocol === 'data:' || target.origin === base.origin) {
      if (request.method() === 'POST' && target.pathname === '/api/nft/orders') {
        const raw = request.postData()
        if (typeof raw !== 'string' || raw.length > 256*1024) {
          failedHTTP.push({path: target.pathname, status: 'invalid-observed-request'})
          void request.abort('blockedbyclient')
          return
        }
        try {posts.push(JSON.parse(raw).orders)} catch {
          failedHTTP.push({path: target.pathname, status: 'invalid-observed-JSON'})
          void request.abort('blockedbyclient')
          return
        }
      }
      void request.continue()
    } else {external.push(target.protocol); void request.abort('blockedbyclient')}
  })
  page.on('pageerror', () => exceptions.push('page-exception'))
  page.on('response', response => {
    if (response.status() >= 400 && failedHTTP.length < 32) failedHTTP.push({path: new URL(response.url()).pathname, status: response.status()})
  })
}

try {
  browser = await puppeteer.launch({executablePath, headless: true, userDataDir: profile,
    args: ['--no-sandbox', '--disable-gpu', '--disable-background-networking', '--disable-component-update',
      '--disable-sync', '--no-first-run', '--proxy-server=http://127.0.0.1:9', '--proxy-bypass-list=127.0.0.1;localhost']})
  chrome = await browser.version()
  context = await browser.createBrowserContext()
  page = await context.newPage()
  await preparePage(page)
  await navigate(fixture.kind === 'bulk' ? '/account/' + fixture.seller : '/item/' + fixture.collection + '/1')
  await connect()
  record('actual production page and EIP-1193 connection')
  if (fixture.kind === 'single') {
    for (const id of [1, 2]) {
      if (id === 2) {await navigate('/item/' + fixture.collection + '/2'); await connect()}
      await click('List for sale')
      const selector = fixture.app === 'nft' ? 'input[aria-label="Asking price"]' : '#list-price'
      await page.waitForSelector(selector, {visible: true, timeout: 12000})
      await page.type(selector, '0.25')
      if (fixture.app === 'nft') await click('List it')
      else {await page.waitForSelector('[data-testid="list-submit"]', {visible: true}); await page.click('[data-testid="list-submit"]')}
      await waitOrders(id)
    }
    record('actual single listing controls sign and submit both tokens')
  } else {
    if (fixture.app === 'karum') await click('Select to list')
    for (const id of [1, 2]) {
      const selector = 'input[aria-label="Select #' + id + '"]'
      await page.waitForSelector(selector, {visible: true, timeout: 12000})
      await page.click(selector)
    }
    if (fixture.app === 'nft') {
      await page.type('input[aria-label="Price each"]', '0.25')
      await click('List 2 selected')
    } else {
      await click('List 2')
      await page.waitForSelector('#list-price', {visible: true, timeout: 12000})
      await page.type('#list-price', '0.25')
      await page.click('[data-testid="list-submit"]')
    }
    await waitOrders(2)
    record('actual bulk selection and listing controls submit both tokens')
  }
  const expected = fixture.kind === 'bulk' ? 1 : 2
  assert.equal(signatures.length, expected)
  assert.equal(posts.length, expected)
  assert.equal(posts.flat().length, 2)
  record('actual typed-data prompt and HTTP submission cardinality')
  let state = await phase('bind')
  fundedNonce = state.actorNonce
  fundedSellerNonce = state.sellerNonce
  if (partialBatch) assert.ok(Number.isSafeInteger(fundedSellerNonce) && fundedSellerNonce >= 3)
  assert.ok(Number.isSafeInteger(fundedNonce) && fundedNonce >= 3)
  assert.equal(state.actor.toLowerCase(), fixture.actor.toLowerCase())
  record('real HTTP admission binds actual inputs without changing the derived prefix')
  await projection('admitted', -1)
  if (fixture.action === 'buy') {
  await navigate('/item/' + fixture.collection + '/1', true)
  await connect()
  if (fixture.app === 'karum') await page.click('main button[data-testid="buy"]')
  else await click('Buy now')
  const pendingState = fixture.app === 'karum' ? 'ordered' : 'published'
  await until(value => !!document.querySelector('main li[data-state="' + value + '"]'), 'actual transaction did not remain pending', pendingState)
  assert.equal(transactions.length, 1)
  assert.equal(await page.$('main li[data-state="done"]'), null)
  record('actual buy control sends one signed transaction and waits without a receipt')
  state = await phase('ordered')
  assert.equal(state.transaction, transactions[0])
  await until(() => !!document.querySelector('main li[data-state="done"]'), 'actual application did not consume the mined receipt')
  record('actual application consumes the executed receipt before success')
  state = await phase('mined')
  const receipt = await chain.getTransactionReceipt({hash: transactions[0]})
  assert.equal(receipt.status, 'success')
  assert.equal(receipt.from.toLowerCase(), fixture.actor.toLowerCase())
  assert.equal(receipt.to.toLowerCase(), fixture.exchange.toLowerCase())
  assert.ok(receipt.gasUsed > 21000n)
  assert.equal(await chain.getTransactionCount({address: actorSigner.address}), fundedNonce + 1)
  record('actual typed transaction receipt gas sender target and consumed nonce')
  await projection('filled', state.filled)
  state = await phase('replacement')
  await assert.rejects(chain.getTransactionReceipt({hash: transactions[0]}))
  assert.equal(await chain.getTransactionCount({address: actorSigner.address}), fundedNonce)
  record('orphaned transaction loses its canonical receipt and releases its nonce')
  await projection('replacement', state.filled)
  state = await phase('restored')
  const restoredReceipt = await chain.getTransactionReceipt({hash: transactions[0]})
  assert.equal(restoredReceipt.blockHash, receipt.blockHash)
  assert.equal(restoredReceipt.transactionHash, receipt.transactionHash)
  assert.equal(await chain.getTransactionCount({address: actorSigner.address}), fundedNonce + 1)
  record('restored canonical transaction recovers its exact receipt and nonce')
  await projection('restored', state.filled)
  } else if (batchMode) {
    const buyerBalanceBefore = await chain.getBalance({address: buyerSigner.address})
    await navigate('/collection/' + fixture.collection, true)
    await connect()
    const sweepInput = 'input[aria-label="How many to sweep"]'
    await page.waitForSelector(sweepInput, {visible: true, timeout: 12000})
    if (fixture.app === 'karum') await until(selector => Number(document.querySelector(selector)?.max) >= 2, 'two real floor listings are unavailable', sweepInput)
    await page.$eval(sweepInput, input => {input.value = '2'; input.dispatchEvent(new Event('input', {bubbles: true}))})
    if (fixture.app === 'nft') await click('Sweep the floor')
    else {
      await click('Add 2 to cart')
      await page.waitForSelector('[data-testid="checkout"]', {visible: true, timeout: 12000})
      await until(() => {
        const button = document.querySelector('[data-testid="checkout"]')
        if (!button || button.disabled || !button.checkVisibility()) return false
        const box = button.getBoundingClientRect()
        const hit = document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2)
        return box.width > 0 && box.height > 0 && box.x >= 0 && box.right <= innerWidth && box.y >= 0 && box.bottom <= innerHeight && !!hit && button.contains(hit)
      }, 'the actual cart checkout has not finished entering the visible viewport')
      await page.click('[data-testid="checkout"]')
    }
    record('actual collection sweep selects two real admitted listings')
    if (partialBatch) {
      await boundedReady(heldBuyerReady, 'the prepared buyer request did not reach the public test wallet')
      assert.equal(transactions.length, 0)
      record('prepared two-input buyer request waits before signing or publication')
      const buyerPage = page
      page = await context.newPage()
      await preparePage(page)
      await navigate('/item/' + fixture.collection + '/1')
      await connect()
      await click(fixture.app === 'nft' ? 'Take off sale' : 'Cancel listing')
      const pendingState = fixture.app === 'karum' ? 'ordered' : 'published'
      await until(value => !!document.querySelector('main li[data-state="' + value + '"]'), 'independent seller cancellation is not pending', pendingState)
      assert.equal(transactions.length, 1)
      assert.equal(await page.$('main li[data-state="done"]'), null)
      record('independent production seller cancellation waits for its own receipt')
      state = await phase('cancel-ordered')
      assert.equal(state.cancellation, transactions[0])
      await until(() => !!document.querySelector('main li[data-state="done"]'), 'seller page did not consume its actual cancellation receipt')
      await until(() => [...document.querySelectorAll('main button')].some(button => button.checkVisibility() && button.textContent.trim() === 'List for sale'), 'seller page did not refresh the cancelled listing')
      state = await phase('cancel-mined')
      assert.equal(state.batchStage, 1)
      const cancellation = await chain.getTransactionReceipt({hash: transactions[0]})
      assert.equal(cancellation.status, 'success')
      assert.equal(cancellation.from.toLowerCase(), fixture.seller.toLowerCase())
      assert.equal(await chain.getTransactionCount({address: signer.address}), fundedSellerNonce + 1)
      await page.close()
      page = buyerPage
      releaseBuyer()
      record('independent seller cancellation executes before the original buyer request is released')
    }
    const scope = fixture.app === 'karum' ? 'aside[aria-label="Cart"] ' : 'main '
    const pendingState = fixture.app === 'karum' ? 'ordered' : 'published'
    await until((scope, value) => !!document.querySelector(scope + 'li[data-state="' + value + '"]'), 'actual batch did not remain pending', scope, pendingState)
    assert.equal(transactions.length, partialBatch ? 2 : 1)
    assert.equal(await page.$(scope + 'li[data-state="done"]'), null)
    record('actual batch checkout publishes its original signed request and waits for a receipt')
    state = await phase('ordered')
    const buyerHash = transactions.at(-1)
    assert.equal(state.transaction, buyerHash)
    await until(() => !!document.querySelector('[data-batch-outcome]'), 'actual batch page did not reconcile its executed receipt')
    const outcome = await page.$eval('[data-batch-outcome]', element => element.textContent.trim())
    assert.ok(outcome.startsWith(partialBatch ? 'Bought 1 of 2 items.' : 'Bought 2 of 2 items.'))
    if (partialBatch) assert.ok(outcome.includes('0.25 ZNN unspent (returned or recorded as exchange credit).'))
    else assert.ok(!outcome.includes('unspent'))
    if (fixture.app === 'karum') {
      await until((chainId, partial, cancelledHash) => {
        const items = (JSON.parse(localStorage.getItem('karum.cart') ?? '{}')[String(chainId)] ?? [])
        return items.length === (partial ? 1 : 0) && (!partial ||
          (String(items[0].tokenId) === '1' && items[0].hash.toLowerCase() === cancelledHash.toLowerCase()))
      }, 'checkout cleared skipped entries or retained purchased entries', fixture.chainId, partialBatch, state.hashes[0])
    }
    record('actual batch result counts executed fills and preserves skipped cart entries')
    state = await phase('mined')
    assert.equal(state.batchStage, 2)
    const receipts = await Promise.all(transactions.map(hash => chain.getTransactionReceipt({hash})))
    const receipt = receipts.at(-1)
    assert.equal(receipt.status, 'success')
    assert.equal(receipt.from.toLowerCase(), fixture.buyer.toLowerCase())
    assert.equal(receipt.to.toLowerCase(), fixture.exchange.toLowerCase())
    assert.ok(receipt.gasUsed > 21000n)
    const filledAbi = parseAbi(['event OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint8 side, address collection, uint256 tokenId, address paymentToken, uint256 price, uint256 royalty, uint256 fee)'])
    const filledTopic = keccak256(stringToHex('OrderFilled(bytes32,address,address,uint8,address,uint256,address,uint256,uint256,uint256)'))
    const fills = receipt.logs.filter(log => log.address.toLowerCase() === fixture.exchange.toLowerCase() && log.topics[0] === filledTopic)
      .map(log => decodeEventLog({abi: filledAbi, data: log.data, topics: log.topics, strict: true}).args)
    assert.equal(fills.length, partialBatch ? 1 : 2)
    assert.deepEqual(fills.map(fill => String(fill.tokenId)).sort(), partialBatch ? ['2'] : ['1', '2'])
    for (const fill of fills) {
      assert.equal(fill.orderHash.toLowerCase(), state.hashes[Number(fill.tokenId) - 1].toLowerCase())
      assert.equal(fill.maker.toLowerCase(), fixture.seller.toLowerCase())
      assert.equal(fill.taker.toLowerCase(), fixture.buyer.toLowerCase())
      assert.equal(fill.side, 0)
      assert.equal(fill.price, 250000000000000000n)
      assert.equal(fill.royalty, 0n)
      assert.equal(fill.fee, 0n)
    }
    const spent = partialBatch ? 250000000000000000n : 500000000000000000n
    const unused = 500000000000000000n - spent
    const accounting = state.ownerModel.executed
    assert.equal(accounting.nativeSpent, spent.toString())
    assert.equal(accounting.nativeUnspent, unused.toString())
    assert.equal(accounting.nativeCredit, '0')
    assert.equal(buyerBalanceBefore - await chain.getBalance({address: buyerSigner.address}), spent + BigInt(accounting.buyerReceiptFee))
    assert.equal(await chain.getTransactionCount({address: buyerSigner.address}), fundedNonce + 1)
    result.batch = {requested: 2, bought: fills.length, nativeUnused: unused.toString(), actualCanonicalBalanceRefundChecked: true,
      actualIndependentSellerPageCancellation: partialBatch, originalBuyerRequestRetained: true}
    record('actual batch receipt and balances prove exact fill count payment and native refund')
    await batchProjection('filled', 2)
    state = await phase('replacement')
    for (const hash of transactions) await assert.rejects(chain.getTransactionReceipt({hash}))
    assert.equal(await chain.getTransactionCount({address: buyerSigner.address}), fundedNonce)
    if (partialBatch) assert.equal(await chain.getTransactionCount({address: signer.address}), fundedSellerNonce)
    assert.equal(await chain.getBalance({address: buyerSigner.address}), buyerBalanceBefore)
    record('orphaned batch and cancellation lose their receipts nonces and balance effects')
    await batchProjection('replacement', 0)
    state = await phase('restored')
    for (const [i, hash] of transactions.entries()) {
      const restored = await chain.getTransactionReceipt({hash})
      assert.equal(restored.blockHash, receipts[i].blockHash)
      assert.equal(restored.transactionHash, receipts[i].transactionHash)
    }
    assert.equal(await chain.getTransactionCount({address: buyerSigner.address}), fundedNonce + 1)
    if (partialBatch) assert.equal(await chain.getTransactionCount({address: signer.address}), fundedSellerNonce + 1)
    record('restored batch and cancellation recover their exact receipts and account nonces')
    await batchProjection('restored', 2)
  } else {
  async function permissionState(expected) {
    await until((collection, value) => [...document.querySelectorAll('[data-testid="collection-permission"]')]
      .some(row => row.dataset.collection.toLowerCase() === collection.toLowerCase() && row.dataset.approval === value),
      'actual permission view differs from executed approval', fixture.collection, expected)
  }
  async function permissionPage(expected) {
    await navigate('/account/' + fixture.seller)
    await connect()
    await permissionState(expected)
  }
  async function ownerProjection(name, included) {
    for (const id of [1, 2]) {
      await navigate('/item/' + fixture.collection + '/' + id, true)
      await until(id => document.querySelector('main h1')?.textContent.trim() === '#' + id, 'actual owner-action item page did not render', id)
      const visible = await page.evaluate(app => [...document.querySelectorAll(app === 'karum' ? 'main button[data-testid="buy"]' : 'main button')]
        .some(button => button.checkVisibility() && (app === 'karum' || button.textContent.trim() === 'Buy now')), fixture.app)
      const valid = !included || (fixture.action === 'cancel' && id === 2)
      assert.equal(visible, valid, 'actual page offered an invalidated order or lost an unaffected listing')
      assert.ok(await page.evaluate(owner => [...document.querySelectorAll('main a')].some(a =>
        a.getAttribute('href')?.toLowerCase().includes('/account/' + owner.toLowerCase())), fixture.seller))
      record(name + ' actual item ' + id + ' owner and invalidation')
    }
  }
  if (fixture.action === 'approval') await permissionPage('granted')
  else {await navigate(fixture.action === 'cancel' ? '/item/' + fixture.collection + '/1' : '/account/' + fixture.seller); await connect()}
  if (fixture.action === 'approval') await page.click('[data-testid="revoke-exchange"]')
  else if (fixture.action === 'cancel') await click(fixture.app === 'nft' ? 'Take off sale' : 'Cancel listing')
  else await click(fixture.app === 'nft' ? 'Cancel all 2 orders' : 'Cancel all orders')
  const pendingState = fixture.app === 'karum' ? 'ordered' : 'published'
  await until(value => !!document.querySelector('main li[data-state="' + value + '"]'), 'owner transaction did not remain pending', pendingState)
  assert.equal(transactions.length, 1)
  assert.equal(await page.$('main li[data-state="done"]'), null)
  record('actual owner control sends one signed transaction and waits without a receipt')
  state = await phase('ordered')
  assert.equal(state.transaction, transactions[0])
  await until(() => !!document.querySelector('main li[data-state="done"]'), 'actual owner application did not consume the mined receipt')
  record('actual owner application consumes the executed receipt before success')
  if (fixture.action === 'approval') await permissionState('removed')
  else if (fixture.action === 'cancel') await until(() => [...document.querySelectorAll('main button')].some(b => b.checkVisibility() && b.textContent.trim() === 'List for sale'), 'cancelled item stayed listed after its receipt')
  else await until(() => ![...document.querySelectorAll('main button')].some(b => b.checkVisibility() && /^Cancel all /.test(b.textContent.trim())), 'old signed orders stayed available after the nonce receipt')
  record('actual current page refreshes its owner action after the real receipt')
  state = await phase('mined')
  const receipt = await chain.getTransactionReceipt({hash: transactions[0]})
  assert.equal(receipt.status, 'success')
  assert.equal(receipt.from.toLowerCase(), fixture.actor.toLowerCase())
  assert.equal(receipt.to.toLowerCase(), targetAddress.toLowerCase())
  assert.ok(receipt.gasUsed > 21000n)
  assert.equal(await chain.getTransactionCount({address: actorSigner.address}), fundedNonce + 1)
  record('actual owner receipt gas sender target and consumed account nonce')
  await ownerProjection('invalidated', true)
  if (fixture.action === 'approval') await permissionPage('removed')
  state = await phase('replacement')
  await assert.rejects(chain.getTransactionReceipt({hash: transactions[0]}))
  assert.equal(await chain.getTransactionCount({address: actorSigner.address}), fundedNonce)
  record('orphaned owner transaction loses its canonical receipt and releases its account nonce')
  if (fixture.action === 'approval') {await permissionState('granted'); record('live permission view restores the orphaned approval without navigation')}
  await ownerProjection('replacement', false)
  if (fixture.action === 'approval') await permissionPage('granted')
  state = await phase('restored')
  const restoredReceipt = await chain.getTransactionReceipt({hash: transactions[0]})
  assert.equal(restoredReceipt.blockHash, receipt.blockHash)
  assert.equal(restoredReceipt.transactionHash, receipt.transactionHash)
  assert.equal(await chain.getTransactionCount({address: actorSigner.address}), fundedNonce + 1)
  record('restored owner transaction recovers its exact receipt and account nonce')
  if (fixture.action === 'approval') {await permissionState('removed'); record('live permission view reapplies restored revocation without navigation')}
  await ownerProjection('restored', true)
  }
  for (const original of posts.flat()) {
    const id = String(original.order.tokenId)
    const rows = await orderRows()
    const selected = rows.find(r => String(r.tokenId) === id)
    assert.ok(selected)
    const response = await fetch(new URL('/api/nft/orders/' + selected.hash, base), {signal: AbortSignal.timeout(5000)})
    assert.equal(response.status, 200)
    const {input} = await response.json()
    const {root: derivedRoot, ...unchanged} = input
    const canonical = value => {
      const {root: cacheOnly, ...input} = structuredClone(value)
      for (const field of ['trader', 'collection', 'paymentToken']) {
        assert.match(input.order[field], /^0x[0-9a-fA-F]{40}$/)
        input.order[field] = input.order[field].toLowerCase()
      }
      return input
    }
    assert.deepEqual(canonical(unchanged), canonical(original), 'accepted browser signature/proof/order was replaced')
  }
  record('actual browser order signature and proof survive both replacements')
  assert.equal(external.length, 0)
  assert.equal(exceptions.length, 0)
  assert.deepEqual(failedHTTP, [])
  assert.deepEqual(providerFailures, [])
  assert.ok(methods.every(m => ['eth_accounts', 'eth_requestAccounts', 'eth_chainId', 'wallet_switchEthereumChain', 'eth_signTypedData_v4', 'eth_sendTransaction'].includes(m)))
  assert.equal(methods.filter(m => m === 'eth_sendTransaction').length, partialBatch ? 2 : 1)
  record((partialBatch ? 'two' : 'one') + ' transaction method and no external request page exception or failed HTTP')
  assert.equal(result.checks.length, batchMode ? (partialBatch ? 23 : 20) : fixture.action === 'buy' ? 19 : fixture.action === 'approval' ? 22 : 20)
  result.pass = true
} catch (error) {
  result.failure = String(error.message).split('\n')[0].slice(0, 220)
  if (page && !page.isClosed()) result.page = await page.evaluate(() => ({path: location.pathname,
    heading: document.querySelector('main h1')?.textContent.trim().slice(0, 80),
    buttons: [...document.querySelectorAll('button')].filter(b => b.checkVisibility()).slice(0, 24).map(b => b.textContent.trim().slice(0, 80)),
    message: document.querySelector('main')?.textContent.slice(-700)})).catch(() => null)
  process.exitCode = 1
} finally {
  if (context) await context.close()
  await stop()
  await rm(profile, {recursive: true, force: false})
  console.log('EXECUTED-MARKET-BROWSER-QUALIFICATION ' + JSON.stringify({node: process.version, chrome, pins,
    result, SDKVersion: sdk.version, actualTypedDataSignatures: signatures.length, actualOrderPOSTs: posts.length,
    providerMethods: methods, providerFailures, externalRequests: external.length, pageExceptions: exceptions.length, failedHTTP,
    actualProductionPageActionsAndGoAdmission: true, actualProviderEVMTransactions: transactions.length,
    actualOfficialRelayerTransport: true, physicalWalletOrPublicNetwork: false,
    signer: 'public scalar-one seller and scalar-two buyer EIP-1193 test signers', browserJoined: !!browser?.process() && browser.process().exitCode !== null}))
}
