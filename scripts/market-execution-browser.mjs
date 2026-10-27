// Actual production listing controls, real EIP-712 signatures from a public
// test signer and the real Go node/market/RPC fixture. No market or RPC reply
// is fabricated here. Native transaction transport remains a separate boundary.
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
assert.equal(signer.address.toLowerCase(), fixture.seller.toLowerCase())
const executablePath = [process.env.CHROME_BIN, '/usr/bin/google-chrome', '/usr/bin/chromium'].find(p => p && existsSync(p))
assert.ok(executablePath, 'preinstalled Chrome required; no browser download fallback')
const profile = await mkdtemp(join(tmpdir(), 'zvm-signed-market-'))
const result = {app: fixture.app, kind: fixture.kind, checks: [], pass: false}
const signatures = [], posts = [], methods = [], failedHTTP = [], exceptions = [], external = []
let browser, context, page, chrome, stopPromise
const stop = () => stopPromise ??= (async () => {if (browser) await browser.close()})()
process.once('SIGTERM', () => {void stop()})

function inject({seller, buyer, chainId}) {
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
    const listed = await page.evaluate(() => [...document.querySelectorAll('main button')].some(b => b.checkVisibility() &&
      (b.dataset.testid === 'buy' || b.textContent.trim() === 'Buy now')))
    assert.equal(listed, id - 1 !== filled, 'actual page fillability differs from executed canonical state')
    const owner = id - 1 === filled ? fixture.buyer : fixture.seller
    assert.ok(await page.evaluate(owner => [...document.querySelectorAll('main a')].some(a =>
      a.getAttribute('href')?.toLowerCase().includes('/account/' + owner.toLowerCase())), owner), 'executed owner link missing')
    record(name + ' actual item ' + id + ' owner and listing')
  }
}

try {
  browser = await puppeteer.launch({executablePath, headless: true, userDataDir: profile,
    args: ['--no-sandbox', '--disable-gpu', '--disable-background-networking', '--disable-component-update',
      '--disable-sync', '--no-first-run', '--proxy-server=http://127.0.0.1:9', '--proxy-bypass-list=127.0.0.1;localhost']})
  chrome = await browser.version()
  context = await browser.createBrowserContext()
  page = await context.newPage()
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
  record('real HTTP admission binds actual inputs without changing the derived prefix')
  await projection('admitted', -1)
  for (const name of ['filled', 'replacement', 'restored']) {
    state = await phase(name)
    await projection(name, state.filled)
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
  assert.ok(methods.every(m => ['eth_accounts', 'eth_requestAccounts', 'eth_chainId', 'wallet_switchEthereumChain', 'eth_signTypedData_v4'].includes(m)))
  record('no transaction method external request page exception or failed HTTP')
  assert.equal(result.checks.length, 14)
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
    providerMethods: methods, externalRequests: external.length, pageExceptions: exceptions.length, failedHTTP,
    actualProductionPageActionsAndGoAdmission: true, physicalWalletOrEVMTransactionTransport: false,
    signer: 'public scalar-one EIP-1193 test signer', browserJoined: !!browser?.process() && browser.process().exitCode !== null}))
}
