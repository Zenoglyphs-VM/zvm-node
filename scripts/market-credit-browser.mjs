// Actual account pages and original-hash recovery with a declared owner EOA
// wrapper for one compiled test account contract. No product reply is mocked.
import assert from 'node:assert/strict'
import {execFileSync} from 'node:child_process'
import {existsSync} from 'node:fs'
import {readFile, mkdtemp, rm} from 'node:fs/promises'
import {createRequire} from 'node:module'
import {tmpdir} from 'node:os'
import {join} from 'node:path'
import {pathToFileURL} from 'node:url'

let input = ''
for await (const chunk of process.stdin) {input += chunk; assert.ok(input.length <= 16384)}
const fixture = JSON.parse(input), base = new URL(fixture.base)
assert.equal(base.protocol, 'http:')
assert.equal(base.hostname, '127.0.0.1')
assert.ok(base.port && /^[a-f0-9]{64}$/.test(fixture.token))
assert.ok(['nft', 'karum'].includes(fixture.app))
assert.equal(fixture.expectedCredit, '5000000000000000')
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
const {createPublicClient, http, encodeFunctionData, parseAbi, keccak256, stringToHex} = requireApp('viem')
// Deliberately public scalar-two fixture owner, never a real wallet identity.
const owner = requireApp('viem/accounts').privateKeyToAccount('0x' + '0'.repeat(63) + '2')
assert.equal(owner.address.toLowerCase(), fixture.owner.toLowerCase())
const chain = createPublicClient({transport: http(new URL('/rpc', base).href, {retryCount: 0})})
const withdrawData = encodeFunctionData({abi: parseAbi(['function withdrawCredits()']), functionName: 'withdrawCredits'})
const key = 'zvm.exchange-credits.v1:' + fixture.chainId + ':' + fixture.exchange.toLowerCase() + ':' + fixture.wallet.toLowerCase()
const executablePath = [process.env.CHROME_BIN, '/usr/bin/google-chrome', '/usr/bin/chromium'].find(name => name && existsSync(name))
assert.ok(executablePath, 'preinstalled Chrome is required')
const profile = await mkdtemp(join(tmpdir(), 'zvm-credit-browser-'))
const result = {app: fixture.app, checks: [], pass: false}
const methods = [], failedHTTP = [], exceptions = [], external = [], providerFailures = [], transactions = []
let browser, context, page, stopPromise, rejectWallet = false, walletRejections = 0, requests = 0, actorNonce
let originalJournal, originalHash, savedBeforeRequest = 0
const stop = () => stopPromise ??= (async () => {if (browser) await browser.close()})()
process.once('SIGTERM', () => {void stop()})
const record = check => {
  assert.ok(!result.checks.includes(check))
  result.checks.push(check)
  console.log('CREDIT-BROWSER-EVENT ' + JSON.stringify({app: fixture.app, check, pass: true}))
}

function inject({wallet, chainId, key}) {
  const listeners = new Map()
  const provider = {
    on(name, fn) {const set = listeners.get(name) || new Set(); set.add(fn); listeners.set(name, set); return provider},
    removeListener(name, fn) {listeners.get(name)?.delete(fn); return provider},
    async request({method, params = []}) {
      await window.__creditMethod(method)
      if (method === 'eth_accounts' || method === 'eth_requestAccounts') return [wallet]
      if (method === 'eth_chainId') return '0x' + BigInt(chainId).toString(16)
      if (method === 'wallet_switchEthereumChain') {
        if (BigInt(params[0].chainId) !== BigInt(chainId)) throw new Error('undeclared fixture chain')
        return null
      }
      if (method === 'eth_sendTransaction') {
        const response = await window.__creditWalletSend({params, journal: localStorage.getItem(key)})
        if (response.rejected) throw Object.assign(new Error('User rejected the fixture request.'), {code: 4001})
        return response.hash
      }
      throw new Error('undeclared credit provider method')
    },
  }
  const announce = () => window.dispatchEvent(new CustomEvent('eip6963:announceProvider', {detail: Object.freeze({
    info: {uuid: 'e5b6718a-5392-4b82-9999-000000000002', name: 'Fixture credit wallet', icon: '', rdns: 'test.zvm.credit'}, provider})}))
  window.addEventListener('eip6963:requestProvider', announce)
}

async function until(fn, label, ...args) {
  await page.waitForFunction(fn, {timeout: 12000, polling: 100}, ...args).catch(() => {throw new Error(label)})
}
async function click(text) {
  const action = (value, press) => {
    const button = [...document.querySelectorAll('button')].find(b => b.checkVisibility() && !b.disabled && b.textContent.trim() === value)
    if (press) button?.click()
    return !!button
  }
  await until(action, 'credit action unavailable: ' + text, text, false)
  assert.ok(await page.evaluate(action, text, true))
}
async function connect() {
  if (await page.evaluate(() => [...document.querySelectorAll('header button')].some(b => b.checkVisibility() && b.textContent.trim() === 'Connect wallet'))) {
    await click('Connect wallet'); await click('Fixture credit wallet')
  }
}
async function credit(amount, state = null) {
  await until((amount, state) => {
    const card = document.querySelector('[data-testid="exchange-credits"]')
    const entry = card?.querySelector('[data-testid="credit-withdrawal"]')
    return card?.getAttribute('data-credit-amount') === amount && (!state || entry?.getAttribute('data-state') === state)
  }, 'actual credit amount or recovery state did not converge', amount, state)
}
async function rows() {
  const raw = await page.evaluate(key => localStorage.getItem(key), key)
  if (raw === null) return []
  const value = JSON.parse(raw)
  assert.equal(value.schema, 1)
  assert.ok(Array.isArray(value.records))
  return value.records
}
async function disabled() {
  return page.evaluate(() => document.querySelector('[data-testid="withdraw-exchange-credit"]')?.disabled)
}
async function refresh() {await click('Refresh credit')}
async function phase(name) {
  const response = await fetch(new URL('/__fixture/credit?name=' + name, base), {method: 'POST',
    headers: {'X-ZVM-Fixture': fixture.token}, signal: AbortSignal.timeout(20000)})
  assert.equal(response.status, 200)
  const value = await response.json()
  assert.equal(value.phase, name)
  assert.ok(value.actualCompiledContractAndBalances && value.sourcePrefixPreserved)
  return value
}

try {
  browser = await puppeteer.launch({executablePath, headless: true, userDataDir: profile,
    args: ['--no-sandbox', '--disable-gpu', '--disable-background-networking', '--disable-component-update',
      '--disable-sync', '--no-first-run', '--proxy-server=http://127.0.0.1:9', '--proxy-bypass-list=127.0.0.1;localhost']})
  context = await browser.createBrowserContext()
  page = await context.newPage()
  await page.setViewport({width: 1360, height: 1000})
  await page.exposeFunction('__creditMethod', method => {assert.ok(methods.length < 64); methods.push(method)})
  await page.exposeFunction('__creditWalletSend', async ({params, journal}) => {
    try {
      assert.equal(params.length, 1)
      const request = params[0], envelope = JSON.parse(journal)
      assert.deepEqual(Object.keys(envelope).sort(), ['records', 'schema'])
      assert.equal(envelope.schema, 1)
      const saved = envelope.records
      assert.equal(saved.length, 1)
      const row = saved[0]
      assert.equal(row.hash, null)
      assert.equal(row.source, 'wallet')
      assert.equal(row.owner, fixture.wallet.toLowerCase())
      assert.equal(row.exchange, fixture.exchange.toLowerCase())
      assert.equal(row.chainId, fixture.chainId)
      assert.equal(row.data, withdrawData)
      assert.equal(row.value, '0')
      assert.equal(row.endpointHash, keccak256(stringToHex(new URL('/rpc', base).href)))
      const head = await chain.getBlock(), genesis = await chain.getBlock({blockNumber: 0n})
      assert.equal(row.head, head.number.toString())
      assert.equal(row.headHash.toLowerCase(), head.hash.toLowerCase())
      assert.equal(row.genesis.toLowerCase(), genesis.hash.toLowerCase())
      assert.equal(BigInt(row.nonce), BigInt(await chain.getTransactionCount({address: fixture.wallet})))
      savedBeforeRequest++
      requests++
      assert.equal(request.from.toLowerCase(), fixture.wallet.toLowerCase())
      assert.equal(request.to.toLowerCase(), fixture.exchange.toLowerCase())
      assert.equal(request.data, withdrawData)
      assert.equal(BigInt(request.value ?? '0x0'), 0n)
      if (request.chainId !== undefined) assert.equal(BigInt(request.chainId), BigInt(fixture.chainId))
      if (rejectWallet) {rejectWallet = false; walletRejections++; return {rejected: true}}
      assert.equal(transactions.length, 0)
      const data = encodeFunctionData({abi: fixture.walletABI, functionName: 'execute', args: [fixture.exchange, withdrawData]})
      const nonce = await chain.getTransactionCount({address: owner.address, blockTag: 'pending'})
      assert.equal(nonce, actorNonce)
      const gas = await chain.estimateGas({account: owner.address, to: fixture.wallet, data, value: 0n})
      const raw = await owner.signTransaction({type: 'eip1559', chainId: fixture.chainId, nonce,
        to: fixture.wallet, data, value: 0n, gas: gas * 13n / 10n,
        maxFeePerGas: BigInt(request.maxFeePerGas), maxPriorityFeePerGas: BigInt(request.maxPriorityFeePerGas)})
      const hash = await chain.sendRawTransaction({serializedTransaction: raw})
      transactions.push({hash, rawBytes: (raw.length - 2) / 2, nonce})
      return {hash}
    } catch (error) {providerFailures.push(String(error.message).slice(0, 220)); throw error}
  })
  await page.evaluateOnNewDocument(inject, {wallet: fixture.wallet, chainId: fixture.chainId, key})
  await page.setRequestInterception(true)
  page.on('request', request => {
    const target = new URL(request.url())
    if (target.protocol === 'data:' || target.origin === base.origin) void request.continue()
    else {external.push(target.protocol); void request.abort('blockedbyclient')}
  })
  page.on('pageerror', () => exceptions.push('page-exception'))
  page.on('response', response => {
    if (response.status() >= 400 && failedHTTP.length < 32) failedHTTP.push({path: new URL(response.url()).pathname, status: response.status()})
  })
  const url = new URL('/zvm-' + fixture.app + '/account/' + fixture.wallet, base)
  url.searchParams.set('rpc', new URL('/rpc', base).href)
  await page.goto(url.href, {waitUntil: 'networkidle0', timeout: 20000})
  await connect()
  await credit(fixture.expectedCredit)
  assert.equal(await disabled(), false)
  assert.deepEqual(await rows(), [])
  record('actual account page reads positive backed exchange credit for the compiled contract wallet')

  await click('Withdraw available ZNN')
  await until(() => document.querySelector('[data-testid="exchange-credits"]')?.textContent.includes('The wallet request ended.'), 'rejecting receiver estimate did not settle')
  await credit(fixture.expectedCredit)
  assert.equal(requests, 0)
  assert.equal(transactions.length, 0)
  assert.deepEqual(await rows(), [])
  await phase('refused')
  record('real rejecting receiver execution refuses estimation before wallet dispatch and preserves credit')

  const allowed = await phase('allow')
  actorNonce = allowed.actorNonce
  assert.ok(Number.isSafeInteger(actorNonce) && actorNonce > 0)
  assert.equal(allowed.state.rejectsNative, false)
  await refresh(); await credit(fixture.expectedCredit)
  assert.equal(await disabled(), false)
  record('declared executed owner call enables the recipient without changing its credit')

  rejectWallet = true
  await click('Withdraw available ZNN')
  await until(() => document.querySelector('[data-testid="exchange-credits"]')?.textContent.includes('The wallet request ended.'), 'explicit wallet rejection did not settle')
  await credit(fixture.expectedCredit)
  assert.equal(walletRejections, 1)
  assert.equal(requests, 1)
  assert.equal(transactions.length, 0)
  assert.deepEqual(await rows(), [])
  record('explicit wallet rejection retains credit and removes only its never-submitted recovery record')

  await click('Withdraw available ZNN')
  await until(() => /^0x[0-9a-fA-F]{64}$/.test(document.querySelector('[data-testid="credit-withdrawal-hash"]')?.textContent.trim() ?? ''), 'original withdrawal hash was not shown before mining')
  assert.equal(transactions.length, 1)
  assert.equal(requests, 2)
  assert.equal(savedBeforeRequest, 2)
  originalHash = transactions[0].hash
  originalJournal = await page.evaluate(key => localStorage.getItem(key), key)
  assert.equal(JSON.parse(originalJournal).records[0].hash, originalHash)
  assert.equal(await page.$eval('[data-testid="credit-withdrawal-hash"]', node => node.textContent.trim()), originalHash)
  assert.equal(await disabled(), true)
  record('original call context is durable before both wallet requests and returned hash is durable before receipt waiting')
  record('owner EOA signs one exact outer account-contract transaction through the real relayer')

  await page.reload({waitUntil: 'networkidle0', timeout: 20000}); await connect()
  await credit(fixture.expectedCredit, 'pending')
  assert.equal(await page.evaluate(key => localStorage.getItem(key), key), originalJournal)
  assert.equal(await disabled(), true)
  await page.$eval('[data-testid="withdraw-exchange-credit"]', button => button.click())
  assert.equal(requests, 2)
  record('page reload restores the exact pending hash and blocks a duplicate wallet request')

  await phase('ordered')
  await refresh(); await credit('0', 'executed')
  assert.equal(await disabled(), true)
  const mined = await phase('mined')
  assert.equal(mined.hash, originalHash)
  assert.equal(mined.state.credit, '0')
  assert.equal(mined.state.recipientNativeBalance, fixture.expectedCredit)
  assert.equal(mined.state.exchangeNativeBalance, '0')
  record('canonical executed withdrawal clears credit and pays the actual contract recipient with receipt-fee accounting')
  const transaction = await chain.getTransaction({hash: originalHash})
  assert.equal(transaction.from.toLowerCase(), owner.address.toLowerCase())
  assert.equal(transaction.to.toLowerCase(), fixture.wallet.toLowerCase())
  assert.notEqual(transaction.from.toLowerCase(), JSON.parse(originalJournal).records[0].owner)
  record('recovery accepts a wallet-associated outer hash without fabricating a contract transaction sender')

  const replaced = await phase('replacement')
  assert.equal(replaced.state.credit, fixture.expectedCredit)
  assert.equal(replaced.state.recipientNativeBalance, '0')
  await refresh(); await credit(fixture.expectedCredit, 'pending')
  assert.equal(await disabled(), true)
  assert.equal(await page.evaluate(key => localStorage.getItem(key), key), originalJournal)
  record('canonical removal restores backed credit and keeps the original withdrawal pending')
  await page.reload({waitUntil: 'networkidle0', timeout: 20000}); await connect()
  await credit(fixture.expectedCredit, 'pending')
  assert.equal(await disabled(), true)
  assert.equal(await page.evaluate(key => localStorage.getItem(key), key), originalJournal)
  assert.equal(requests, 2)
  record('reload after removal retains the same hash and still blocks another request')

  const restored = await phase('restored')
  assert.equal(restored.hash, originalHash)
  assert.deepEqual(restored.state, mined.state)
  await refresh(); await credit('0', 'executed')
  assert.equal(await page.evaluate(key => localStorage.getItem(key), key), originalJournal)
  assert.equal(await page.$eval('[data-testid="credit-withdrawal-hash"]', node => node.textContent.trim()), originalHash)
  assert.equal(await disabled(), true)
  assert.equal(requests, 2)
  assert.equal(transactions.length, 1)
  record('restoration recovers the same executed hash balances and immutable journal without re-signing')
  assert.ok(methods.every(method => ['eth_accounts', 'eth_requestAccounts', 'eth_chainId', 'wallet_switchEthereumChain', 'eth_sendTransaction'].includes(method)))
  assert.equal(methods.filter(method => method === 'eth_sendTransaction').length, 2)
  assert.deepEqual(providerFailures, [])
  assert.deepEqual(failedHTTP, [])
  assert.deepEqual(external, [])
  assert.deepEqual(exceptions, [])
  record('only declared provider methods execute with one explicit rejection one broadcast and no external or failed page requests')
  assert.equal(result.checks.length, 13)
  result.pass = true
} catch (error) {
  result.failure = String(error.message).split('\n')[0].slice(0, 240)
  if (page && !page.isClosed()) result.page = await page.evaluate(() => ({path: location.pathname,
    message: document.querySelector('[data-testid="exchange-credits"]')?.textContent.slice(-900),
    buttons: [...document.querySelectorAll('button')].filter(button => button.checkVisibility()).slice(0, 20).map(button => button.textContent.trim().slice(0, 80))})).catch(() => null)
  process.exitCode = 1
} finally {
  if (context) await context.close()
  await stop()
  await rm(profile, {recursive: true, force: false})
  console.log('EXECUTED-CREDIT-BROWSER-QUALIFICATION ' + JSON.stringify({result, pins, SDKVersion: sdk.version,
    actualProductionPage: true, knownContractAccountAdapter: true, requests, walletRejections, savedBeforeRequest,
    providerMethods: methods, providerFailures, actualProviderEVMTransactions: transactions.length,
    signedRawBytes: transactions.map(value => value.rawBytes), originalHash,
    externalRequests: external.length, failedHTTP, pageExceptions: exceptions.length,
    browserJoined: !!browser?.process() && browser.process().exitCode !== null,
    genericSafeUserOperationOrPhysicalWalletQualified: false, independentNoMOrFullDaemonQualified: false,
    sourceSaleIsDeclaredSetup: true}))
}
