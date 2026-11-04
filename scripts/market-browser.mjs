// Real production pages and the live Go fixture. The injected wallet speaks
// EIP-6963/EIP-1193 but is a declared stand-in, never a real signing wallet.
import assert from 'node:assert/strict'
import {boundedNavigation} from './market-navigation-support.mjs'
import {execFileSync} from 'node:child_process'
import {existsSync} from 'node:fs'
import {readFile, mkdtemp, rm} from 'node:fs/promises'
import {createRequire} from 'node:module'
import {tmpdir} from 'node:os'
import {join} from 'node:path'
import {pathToFileURL} from 'node:url'

let input = ''
for await (const chunk of process.stdin) {
  input += chunk
  assert.ok(input.length <= 16384, 'bounded fixture input required')
}
const fixture = JSON.parse(input)
const base = new URL(fixture.base)
const navigation = boundedNavigation(base.origin)
assert.equal(base.protocol, 'http:')
assert.equal(base.hostname, '127.0.0.1')
assert.ok(base.port && /^[a-f0-9]{64}$/.test(fixture.token))
const pins = JSON.parse(await readFile(new URL('./market-browser-pins.json', import.meta.url)))
const git = (root, ...args) => execFileSync('git', ['--no-replace-objects', ...args], {cwd: root, encoding: 'utf8'}).trim()
for (const name of ['nft', 'karum']) {
  assert.equal(git(fixture.roots[name], 'rev-parse', 'HEAD'), pins['zvm-' + name])
  assert.equal(git(join(fixture.roots[name], 'zvm-core'), 'rev-parse', 'HEAD'), pins['shared-core'])
}
const requireKarum = createRequire(join(fixture.roots.karum, 'package.json'))
const puppeteer = (await import(pathToFileURL(requireKarum.resolve('puppeteer-core')).href)).default
const executablePath = [process.env.CHROME_BIN, '/usr/bin/google-chrome', '/usr/bin/chromium'].find(p => p && existsSync(p))
assert.ok(executablePath, 'preinstalled Chrome required; no browser download fallback')
const profile = await mkdtemp(join(tmpdir(), 'zvm-market-browser-'))
const browser = await puppeteer.launch({executablePath, headless: true, userDataDir: profile,
  args: ['--no-sandbox', '--disable-gpu', '--disable-background-networking', '--disable-component-update',
    '--disable-sync', '--no-first-run', '--proxy-server=http://127.0.0.1:9', '--proxy-bypass-list=127.0.0.1;localhost']})
const chrome = await browser.version(), results = [], external = [], problems = [], failedHTTP = []
let shuttingDown = false
const stop = async () => {if (!shuttingDown) {shuttingDown = true; await browser.close()}}
process.once('SIGTERM', () => {void stop()})
const buyer = '0x0000000000000000000000000000000000000077'

function inject(account) {
  const state = {accounts: [account], chain: '0x45', calls: []}, listeners = new Map()
  const provider = {
    on(event, fn) {const items = listeners.get(event) || new Set(); items.add(fn); listeners.set(event, items); return provider},
    removeListener(event, fn) {listeners.get(event)?.delete(fn); return provider},
    async request({method, params = []}) {
      state.calls.push({method, params})
      if (method === 'eth_accounts' || method === 'eth_requestAccounts') return state.accounts
      if (method === 'eth_chainId') return state.chain
      if (method === 'wallet_switchEthereumChain') {
        if (params[0]?.chainId !== '0x45') throw new Error('unexpected fixture chain')
        state.chain = '0x45'
        for (const fn of listeners.get('chainChanged') || []) fn(state.chain)
        return null
      }
      throw new Error('fixture wallet refuses unsupported capability: ' + method)
    },
  }
  const announce = () => window.dispatchEvent(new CustomEvent('eip6963:announceProvider', {detail: Object.freeze({
    info: {uuid: 'zvm-market-browser-fixture', name: 'Fixture wallet', icon: '', rdns: 'test.zvm'}, provider})}))
  window.addEventListener('eip6963:requestProvider', announce)
  window.__marketFixture = {state, emit(event, payload) {
    if (event === 'accountsChanged') state.accounts = payload
    if (event === 'chainChanged') state.chain = payload
    for (const fn of listeners.get(event) || []) fn(payload)
  }}
}

async function until(page, fn, what) {
  await page.waitForFunction(fn, {timeout: 12000, polling: 100}).catch(() => {throw new Error(what)})
}
async function click(page, text, prefix = false) {
  const predicate = (text, prefix, press) => {
    const item = [...document.querySelectorAll('button')].find(b => b.checkVisibility() && !b.disabled &&
      (prefix ? b.textContent.trim().startsWith(text) : b.textContent.trim() === text))
    if (press) item?.click()
    return !!item
  }
  await page.waitForFunction(predicate, {timeout: 12000}, text, prefix, false)
  assert.ok(await page.evaluate(predicate, text, prefix, true), 'visible action disappeared')
}
async function phase(name) {
  const res = await fetch(new URL('/__fixture/phase?name=' + name, base), {
    method: 'POST', headers: {'X-ZVM-Fixture': fixture.token}, signal: AbortSignal.timeout(12000)})
  assert.equal(res.status, 200)
  const body = await res.json()
  assert.equal(body.phase, name)
  assert.equal(body.owner.toLowerCase(), (name === 'orphan' ? fixture.bob : fixture.alice).toLowerCase())
  assert.equal(body.listed, ['prefix', 'rechecked'].includes(name))
}
function record(result, name) {
  result.checks.push(name)
  console.log('MARKET-BROWSER-EVENT ' + JSON.stringify({app: result.app, check: name, pass: true}))
}

try {
  for (const app of ['nft', 'karum']) {
    const result = {app, checks: [], pass: false}, context = await browser.createBrowserContext(), providerMethods = []
    let page
    results.push(result)
    try {
      page = await context.newPage()
      await page.setViewport({width: 1360, height: 1000})
      await page.setRequestInterception(true)
      page.on('request', request => {
        const url = new URL(request.url())
        if (url.protocol === 'data:' || url.origin === base.origin) void request.continue()
        else {external.push({app, protocol: url.protocol}); void request.abort('blockedbyclient')}
      })
      page.on('pageerror', () => problems.push({app, kind: 'page-exception'}))
      page.on('response', response => {
        if (response.status() >= 400 && failedHTTP.length < 64) {
          const url = new URL(response.url())
          failedHTTP.push({app, path: url.pathname, query: Object.fromEntries([...url.searchParams]
            .filter(([key]) => ['collection', 'limit', 'sort', 'offset', 'collectionOnly'].includes(key))), status: response.status()})
        }
      })
      await page.evaluateOnNewDocument(inject, buyer)
      const url = new URL('/zvm-' + app + '/item/' + fixture.collection + '/1', base)
      url.searchParams.set('rpc', new URL('/rpc', base).href)
      const projection = async listed => {
        providerMethods.push(...await page.evaluate(() => window.__marketFixture?.state.calls.map(c => c.method) || []))
        await navigation.goto(page, url.href)
        await until(page, () => document.querySelector('main h1')?.textContent.trim() === '#1', 'actual item route did not render')
        const shown = await page.evaluate(() => !![...document.querySelectorAll('main button')].find(b => b.checkVisibility() &&
          (b.dataset.testid === 'buy' || b.textContent.trim() === 'Buy now')))
        assert.equal(shown, listed, 'actual page listing differs from the Go projection')
        const owner = listed ? fixture.alice : (result.currentPhase === 'orphan' ? fixture.bob : fixture.alice)
        assert.ok(await page.evaluate(owner => [...document.querySelectorAll('main a')]
          .some(a => a.getAttribute('href')?.toLowerCase().includes('/account/' + owner.toLowerCase())), owner), 'actual owner link missing')
      }
      await phase('prefix')
      await projection(true)
      record(result, 'actual item route and current listing')
      const itemPath = '/zvm-' + app + '/item/' + fixture.collection + '/1'
      const collectionPath = '/zvm-' + app + '/collection/' + fixture.collection
      const follow = async path => {
        await page.waitForFunction(path => [...document.querySelectorAll('main a')]
          .some(a => a.checkVisibility() && a.getAttribute('href') === path), {timeout: 12000}, path)
        await page.evaluate(path => [...document.querySelectorAll('main a')]
          .find(a => a.checkVisibility() && a.getAttribute('href') === path).click(), path)
        await page.waitForFunction(path => location.pathname === path, {timeout: 12000}, path)
      }
      await follow(collectionPath)
      await follow(itemPath)
      await until(page, () => document.querySelector('main h1')?.textContent.trim() === '#1', 'collection card did not return to the actual item')
      record(result, 'canonical collection and item links navigate the actual pages')
      if (app === 'karum') {
        await follow(collectionPath)
        await page.waitForFunction(path => [...document.querySelectorAll('main a')]
          .some(a => a.getAttribute('href') === path), {timeout: 12000}, itemPath)
        await page.evaluate(path => {
          const state = {...history.state, back: location.pathname, current: path, forward: null,
            position: (history.state?.position || 0) + 1}
          history.pushState(state, '', path)
          window.dispatchEvent(new PopStateEvent('popstate', {state}))
        }, '/zvm-karum/collection/not-an-address')
        await until(page, () => document.querySelector('main')?.textContent.includes('Invalid collection address.'), 'invalid collection route did not settle in its error state')
        assert.equal(await page.evaluate(path => [...document.querySelectorAll('main a')]
          .some(a => a.getAttribute('href') === path), itemPath), false, 'invalid route retained the prior item card')
        await projection(true)
        record(result, 'invalid collection route clears old data and recovers to a valid item')
      }
      if (app === 'nft') {
        const accountPath = '/zvm-nft/account/' + fixture.alice
        await follow(accountPath)
        await page.waitForFunction(path => [...document.querySelectorAll('main a')]
          .some(a => a.getAttribute('href') === path), {timeout: 12000}, itemPath)
        const explorer = await page.evaluate(() => [...document.querySelectorAll('main a')]
          .find(a => a.textContent.trim() === 'Explorer view')?.href)
        assert.equal(new URL(explorer).origin, base.origin)
        assert.equal(new URL(explorer).pathname, '/address/' + fixture.alice)
        await follow(itemPath)
        const legacyAccount = new URL('/zvm-nft/address/' + fixture.alice, base)
        legacyAccount.searchParams.set('rpc', new URL('/rpc', base).href)
        await navigation.goto(page, legacyAccount.href)
        await follow(itemPath)
        await until(page, () => document.querySelector('main h1')?.textContent.trim() === '#1', 'account item link did not navigate')
        record(result, 'owner account links and the legacy address alias preserve the item')
        const legacyItem = new URL('/zvm-nft/nft/' + fixture.collection + '/1', base)
        legacyItem.searchParams.set('rpc', new URL('/rpc', base).href)
        await navigation.goto(page, legacyItem.href)
        await until(page, () => document.querySelector('main h1')?.textContent.trim() === '#1', 'legacy item bookmark did not render')
        record(result, 'legacy NFT item bookmark resolves the same item')
        const legacyCollection = new URL('/zvm-nft/nft/' + fixture.collection, base)
        legacyCollection.searchParams.set('rpc', new URL('/rpc', base).href)
        await navigation.goto(page, legacyCollection.href)
        await follow(itemPath)
        await until(page, () => document.querySelector('main h1')?.textContent.trim() === '#1', 'legacy collection card did not navigate')
        record(result, 'legacy NFT collection bookmark generates a canonical item link')
        const home = new URL('/zvm-nft/', base)
        home.searchParams.set('rpc', new URL('/rpc', base).href)
        await navigation.goto(page, home.href)
        assert.equal(await page.evaluate(() => [...document.querySelectorAll('main a')]
          .some(a => a.getAttribute('href')?.startsWith('/zvm-nft/nft/'))), false)
        await follow('/zvm-nft/create')
        await until(page, () => document.querySelector('main h1')?.textContent.trim() === 'Create' &&
          document.querySelector('main')?.textContent.includes('Not available on this node'), 'actual guarded create page did not render')
        record(result, 'explore generates canonical collection and create links')
        await projection(true)
      }
      await click(page, 'Connect wallet')
      await click(page, 'Fixture wallet')
      await until(page, () => window.__marketFixture.state.calls.some(c => c.method === 'eth_requestAccounts') &&
        window.__marketFixture.state.calls.some(c => c.method === 'wallet_switchEthereumChain'), 'actual connect did not call the provider')
      await until(page, () => [...document.querySelectorAll('main button')].some(b => b.checkVisibility() && b.textContent.trim() === 'Buy now' && !b.disabled), 'connected buyer action did not enable')
      record(result, 'EIP-6963 discovery and EIP-1193 connect')
      await page.evaluate(owner => window.__marketFixture.emit('accountsChanged', [owner]), fixture.alice)
      await until(page, () => [...document.querySelectorAll('main button')].some(b => b.checkVisibility() &&
        ['Take off sale', 'Cancel listing'].includes(b.textContent.trim())), 'account event did not change owner actions')
      await page.evaluate(account => window.__marketFixture.emit('accountsChanged', [account]), buyer)
      record(result, 'provider account change updates page ownership actions')
      await page.evaluate(() => window.__marketFixture.emit('chainChanged', '0x2'))
      await click(page, app === 'nft' ? 'Switch network' : 'Switch to ', app === 'karum')
      await until(page, () => window.__marketFixture.state.calls.filter(c => c.method === 'wallet_switchEthereumChain').length === 2,
        'actual switch action did not restore fixture chain')
      record(result, 'provider chain event and visible network switch')
      await page.evaluate(() => window.__marketFixture.emit('accountsChanged', []))
      await until(page, () => [...document.querySelectorAll('header button')].some(b => b.checkVisibility() && b.textContent.trim() === 'Connect wallet'),
        'empty account event did not disconnect')
      record(result, 'empty provider account event disconnects')
      for (const name of ['orphan', 'unchecked', 'rechecked', 'replacement']) {
        result.currentPhase = name
        await phase(name)
        await projection(name === 'rechecked')
        record(result, 'actual Go HTTP/index/book ' + name)
      }
      providerMethods.push(...await page.evaluate(() => window.__marketFixture.state.calls.map(c => c.method)))
      assert.ok(providerMethods.every(method => ['eth_accounts', 'eth_requestAccounts', 'eth_chainId', 'wallet_switchEthereumChain'].includes(method)))
      result.providerMethods = providerMethods
      result.pass = true
    } catch (error) {
      result.failure = error.code === 'ERR_ASSERTION' ? 'browser assertion failed: ' + error.message.split('\n')[0] : error.message.split('\n')[0].slice(0, 160)
      if (page && !page.isClosed()) {
        result.page = await page.evaluate(() => ({path: location.pathname,
          heading: document.querySelector('main h1')?.textContent.trim().slice(0, 80),
          links: [...document.querySelectorAll('main a')].slice(0, 16).map(a => new URL(a.href).pathname)})).catch(() => null)
      }
    } finally {
      delete result.currentPhase
      await context.close()
    }
  }
  assert.equal(external.length, 0, 'external page requests attempted')
  assert.equal(problems.length, 0, 'actual page exceptions observed')
} finally {
  await stop()
  await rm(profile, {recursive: true, force: false})
  console.log('MARKET-BROWSER-QUALIFICATION ' + JSON.stringify({node: process.version, chrome, pins,
    results, failedHTTP, externalRequests: external.length, pageExceptions: problems.length,
    actualBrowserProductionPagesAndGoHTTP: true, boundedNavigation: navigation.summary(), walletProvider: 'declared EIP-6963/EIP-1193 stand-in',
    exchangeRPCChecksAndChainMetadata: 'declared controls; not execution or full-client evidence',
    actualTransactionsOrSignatures: 0, browserJoined: browser.process()?.exitCode !== null}))
}
assert.ok(results.length === 2 && results.every(r => r.pass && r.checks.length === (r.app === 'nft' ? 14 : 11)), 'both real page paths must pass every declared check')
assert.equal(failedHTTP.length, 0, 'actual page HTTP failures observed')
