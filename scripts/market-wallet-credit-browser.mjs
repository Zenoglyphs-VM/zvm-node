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
assert.equal(typeof fixture.minedRevert, 'boolean')
assert.equal(fixture.walletCalls, true)
assert.equal(fixture.minedRevert, false)
const minedRevert = fixture.minedRevert
const finalAmount = minedRevert ? fixture.expectedCredit : '0'
const finalState = minedRevert ? 'reverted' : 'executed'
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
const legacyKey = 'zvm.exchange-credits.v1:' + fixture.chainId + ':' + fixture.exchange.toLowerCase() + ':' + fixture.wallet.toLowerCase()
const key = 'zvm.exchange-credit-calls.v1:' + fixture.chainId + ':' + fixture.exchange.toLowerCase() + ':' + fixture.wallet.toLowerCase()
const executablePath = [process.env.CHROME_BIN, '/usr/bin/google-chrome', '/usr/bin/chromium'].find(name => name && existsSync(name))
assert.ok(executablePath, 'preinstalled Chrome is required')
const profile = await mkdtemp(join(tmpdir(), 'zvm-credit-browser-'))
const result = {app: fixture.app, minedRevert, walletCalls: true, checks: [], pass: false}
const methods = [], failedHTTP = [], exceptions = [], external = [], providerFailures = [], transactions = []
let browser, context, page, stopPromise, rejectWallet = false, walletRejections = 0, requests = 0, actorNonce
let originalJournal, originalHash, savedBeforeRequest = 0
let preparationHeader, finalReceipt, originalCallId
let recoverStatus = false, lostResponses = 0, receiverProbeRefusals = 0
const callBatches = new Map(), statusQueries = []
const stop = () => stopPromise ??= (async () => {if (browser) await browser.close()})()
process.once('SIGTERM', () => {void stop()})
const record = check => {
  assert.ok(!result.checks.includes(check))
  result.checks.push(check)
  console.log('CREDIT-BROWSER-EVENT ' + JSON.stringify({app: fixture.app, check, pass: true}))
}

function inject({wallet, chainId, key, legacyKey}) {
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
      if (method === 'wallet_getCapabilities') {
        if (params[0].toLowerCase() !== wallet.toLowerCase() || params.length !== 2 ||
          params[1].length !== 1 || BigInt(params[1][0]) !== BigInt(chainId)) throw new Error('unscoped capability discovery')
        return {['0x' + BigInt(chainId).toString(16)]: {atomic: {status: 'unsupported'}}}
      }
      if (method === 'wallet_sendCalls') {
        const response = await window.__creditWalletSend({params, journal: localStorage.getItem(key), legacyJournal: localStorage.getItem(legacyKey)})
        if (response.rejected) throw Object.assign(new Error('User rejected the fixture wallet call.'), {code: 4001})
        if (response.lost) throw new Error('The accepted wallet response was intentionally lost.')
        return {id: response.id}
      }
      if (method === 'wallet_getCallsStatus') {
        const response = await window.__creditWalletStatus(params)
        if (response.unknown) throw Object.assign(new Error('Fixture wallet status is temporarily unavailable.'), {code: 5730})
        return response
      }
      throw new Error('undeclared wallet-call provider method')
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
    const entries = card?.querySelectorAll('[data-testid="credit-withdrawal"]')
    const entry = entries?.[entries.length - 1]
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
  await page.exposeFunction('__creditMethod', method => {assert.ok(methods.length < 128); methods.push(method)})
  await page.exposeFunction('__creditWalletSend', async ({params, journal, legacyJournal}) => {
    try {
      assert.equal(params.length, 1)
      assert.equal(legacyJournal, null, 'the old withdrawal journal must not be rewritten')
      const request = params[0], envelope = JSON.parse(journal)
      assert.deepEqual(Object.keys(envelope).sort(), ['records', 'schema'])
      assert.equal(envelope.schema, 1)
      const saved = envelope.records
      assert.equal(saved.length, requests + 1)
      const row = saved[saved.length - 1]
      assert.equal(row.hash, null); assert.equal(row.source, 'wallet-calls')
      assert.equal(row.owner, fixture.wallet.toLowerCase()); assert.equal(row.exchange, fixture.exchange.toLowerCase())
      assert.equal(row.chainId, fixture.chainId); assert.equal(row.data, withdrawData); assert.equal(row.value, '0')
      assert.equal(row.endpointHash, keccak256(stringToHex(new URL('/rpc', base).href)))
      assert.deepEqual(row.wallet.request, request); assert.deepEqual(row.wallet.results, [])
      assert.match(request.id, /^0x[0-9a-f]{128}$/); assert.ok(!callBatches.has(request.id))
      assert.equal(request.version, '2.0.0'); assert.equal(request.from, fixture.wallet.toLowerCase())
      assert.equal(BigInt(request.chainId), BigInt(fixture.chainId)); assert.equal(request.atomicRequired, false)
      assert.deepEqual(request.calls, [{to: fixture.exchange.toLowerCase(), data: withdrawData, value: '0x0'}])
      const head = await chain.getBlock(), genesis = await chain.getBlock({blockNumber: 0n})
      assert.equal(row.head, head.number.toString()); assert.equal(row.headHash.toLowerCase(), head.hash.toLowerCase())
      assert.equal(row.genesis.toLowerCase(), genesis.hash.toLowerCase())
      assert.equal(BigInt(row.nonce), BigInt(await chain.getTransactionCount({address: fixture.wallet})))
      savedBeforeRequest++; requests++
      const batch = {request: JSON.stringify(request), outcome: 'preparing', hash: null}
      callBatches.set(request.id, batch)
      // This is an actual read-only call from the credit owner in the pinned
      // engine. The subsequent signed transaction still comes from its EOA.
      try {await chain.estimateGas({account: fixture.wallet, to: fixture.exchange, data: withdrawData, value: 0n})}
      catch (error) {
        assert.equal(requests, 1, 'only the initial rejecting receiver probe may fail')
        receiverProbeRefusals++; batch.outcome = 'offchain-failed'
        return {id: request.id}
      }
      if (rejectWallet) {
        rejectWallet = false; walletRejections++; batch.outcome = 'offchain-failed'
        return {id: request.id, rejected: true}
      }
      assert.equal(requests, 3); assert.equal(transactions.length, 0)
      preparationHeader = {number: head.number.toString(), hash: head.hash}
      const data = encodeFunctionData({abi: fixture.walletABI, functionName: 'execute', args: [fixture.exchange, withdrawData]})
      const nonce = await chain.getTransactionCount({address: owner.address, blockTag: 'pending'})
      assert.equal(nonce, actorNonce)
      const gas = await chain.estimateGas({account: owner.address, to: fixture.wallet, data, value: 0n})
      assert.equal(typeof head.baseFeePerGas, 'bigint')
      const raw = await owner.signTransaction({type: 'eip1559', chainId: fixture.chainId, nonce,
        to: fixture.wallet, data, value: 0n, gas: gas * 13n / 10n,
        maxFeePerGas: head.baseFeePerGas * 2n + 1n, maxPriorityFeePerGas: 1n})
      const hash = await chain.sendRawTransaction({serializedTransaction: raw})
      transactions.push({hash, rawBytes: (raw.length - 2) / 2, nonce})
      batch.hash = hash; batch.outcome = 'submitted'; lostResponses++
      // The fixture wallet retains its real hash under the exact caller id,
      // while the application loses the original wallet_sendCalls response.
      return {id: request.id, lost: true}
    } catch (error) {providerFailures.push(String(error.message).slice(0, 220)); throw error}
  })
  await page.exposeFunction('__creditWalletStatus', async params => {
    try {
      assert.equal(params.length, 1); assert.equal(typeof params[0], 'string')
      const id = params[0], batch = callBatches.get(id)
      assert.ok(batch, 'only a previously dispatched exact id may be queried')
      statusQueries.push(id); assert.ok(statusQueries.length <= 96)
      const response = {version: '2.0.0', id, chainId: '0x' + BigInt(fixture.chainId).toString(16), atomic: false, status: 100, receipts: []}
      if (batch.outcome === 'offchain-failed') return {...response, status: 400}
      assert.equal(batch.outcome, 'submitted'); assert.ok(batch.hash)
      if (!recoverStatus) return {unknown: true}
      const receipt = await chain.request({method: 'eth_getTransactionReceipt', params: [batch.hash]})
      if (receipt === null) return response
      assert.equal(receipt.transactionHash.toLowerCase(), batch.hash.toLowerCase())
      assert.equal(receipt.status, '0x1')
      return {...response, status: 200, receipts: [{transactionHash: receipt.transactionHash,
        blockHash: receipt.blockHash, blockNumber: receipt.blockNumber, gasUsed: receipt.gasUsed, status: receipt.status,
        logs: receipt.logs.map(log => ({address: log.address, data: log.data, topics: log.topics}))}]}
    } catch (error) {providerFailures.push(String(error.message).slice(0, 220)); throw error}
  })
  await page.evaluateOnNewDocument(inject, {wallet: fixture.wallet, chainId: fixture.chainId, key, legacyKey})
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
  await page.goto(url.href, {waitUntil: 'networkidle0', timeout: 20000}); await connect()
  await credit(fixture.expectedCredit)
  assert.equal(await disabled(), false); assert.deepEqual(await rows(), [])
  assert.equal(methods.filter(method => method === 'wallet_getCapabilities').length, 0)
  record('actual account page reads backed contract credit without unsolicited wallet capability discovery')

  await click('Withdraw available ZNN'); await credit(fixture.expectedCredit, 'not-submitted')
  assert.equal(receiverProbeRefusals, 1); assert.equal(requests, 1); assert.equal(transactions.length, 0)
  assert.equal((await rows())[0].wallet.results[0].status, 400)
  await phase('refused')
  record('real PayoutFailed estimation produces an offchain wallet refusal while preserving the original request')

  const allowed = await phase('allow'); actorNonce = allowed.actorNonce
  assert.ok(Number.isSafeInteger(actorNonce) && actorNonce > 0); assert.equal(allowed.state.rejectsNative, false)
  await refresh(); await credit(fixture.expectedCredit, 'not-submitted'); assert.equal(await disabled(), false)
  record('executed owner setup enables the receiver and pins its actual funded EOA nonce')

  rejectWallet = true
  await click('Withdraw available ZNN'); await credit(fixture.expectedCredit, 'not-submitted')
  await until(() => document.querySelectorAll('[data-testid="credit-withdrawal"]').length === 2, 'rejected wallet intent not retained')
  await credit(fixture.expectedCredit, 'not-submitted')
  assert.equal(walletRejections, 1); assert.equal(requests, 2); assert.equal(transactions.length, 0)
  const refused = await rows(); assert.equal(refused.length, 2)
  assert.ok(refused.every(row => row.wallet.results.length === 1 && row.wallet.results[0].status === 400))
  const refusedBytes = JSON.stringify(refused)
  record('explicit rejection retains both refused request ids without signing or dropping history')

  await click('Withdraw available ZNN'); await credit(fixture.expectedCredit, 'unknown')
  await until(() => document.querySelector('[data-testid="withdraw-exchange-credit"]')?.textContent.trim() === 'Withdraw available ZNN', 'the lost send response did not settle')
  await credit(fixture.expectedCredit, 'unknown')
  assert.equal(requests, 3); assert.equal(savedBeforeRequest, 3); assert.equal(lostResponses, 1)
  assert.equal(transactions.length, 1); originalHash = transactions[0].hash
  originalJournal = await page.evaluate(key => localStorage.getItem(key), key)
  const active = (await rows())[2]; originalCallId = active.wallet.request.id
  const originalRequest = JSON.stringify(active.wallet.request)
  assert.equal(active.hash, null); assert.deepEqual(active.wallet.results, [])
  assert.equal(JSON.stringify((await rows()).slice(0, 2)), refusedBytes)
  assert.equal(await page.evaluate(key => localStorage.getItem(key), legacyKey), null)
  assert.equal(await disabled(), true)
  record('all exact caller ids and payloads are durable before wallet dispatch and the legacy journal stays untouched')
  record('one actual owner-signed outer transaction is submitted through the official relayer before its wallet response is lost')
  assert.equal(await page.$$eval('[data-testid="credit-withdrawal-hash"]', nodes => nodes.length), 0)
  record('lost response exposes only the original saved request id and prevents a replacement send')

  await page.reload({waitUntil: 'networkidle0', timeout: 20000}); await connect()
  await credit(fixture.expectedCredit, 'unknown')
  assert.equal(await page.evaluate(key => localStorage.getItem(key), key), originalJournal)
  await page.$eval('[data-testid="withdraw-exchange-credit"]', button => button.click())
  assert.equal(requests, 3); assert.equal(await disabled(), true)
  record('reload with unavailable wallet status restores the same unresolved id and still blocks another request')

  recoverStatus = true
  await refresh(); await credit(fixture.expectedCredit, 'pending')
  assert.equal(JSON.stringify((await rows())[2].wallet.request), originalRequest)
  assert.equal((await rows())[2].wallet.results[0].status, 100)
  assert.equal(transactions.length, 1); assert.equal(requests, 3)
  record('recovery queries only the saved id and observes the actual missing canonical receipt without resending')

  await phase('ordered'); await refresh(); await credit('0', 'executed')
  const mined = await phase('mined')
  assert.equal(mined.hash, originalHash); assert.equal(mined.state.credit, '0')
  assert.equal(mined.state.recipientNativeBalance, fixture.expectedCredit); assert.equal(mined.state.exchangeNativeBalance, '0')
  const transaction = await chain.getTransaction({hash: originalHash}), receipt = await chain.getTransactionReceipt({hash: originalHash})
  assert.equal(receipt.status, 'success'); assert.ok(receipt.gasUsed > 21000n && receipt.gasUsed < transaction.gas)
  assert.ok(receipt.blockNumber > BigInt(preparationHeader.number))
  finalReceipt = {status: receipt.status, blockNumber: receipt.blockNumber.toString(), blockHash: receipt.blockHash,
    gasUsed: receipt.gasUsed.toString(), gasLimit: transaction.gas.toString()}
  const included = (await rows())[2]
  assert.equal(included.wallet.results.at(-1).status, 200)
  assert.equal(included.wallet.results.at(-1).receipts[0].transactionHash.toLowerCase(), originalHash.toLowerCase())
  record('wallet-reported completion and actual canonical receipt agree with independently executed payout credit and owner fees')
  assert.equal(transaction.from.toLowerCase(), owner.address.toLowerCase()); assert.equal(transaction.to.toLowerCase(), fixture.wallet.toLowerCase())
  assert.notEqual(transaction.from.toLowerCase(), included.owner)
  record('wallet id maps to the real outer transaction without fabricating the contract-account sender')

  const replaced = await phase('replacement')
  assert.equal(replaced.state.credit, fixture.expectedCredit); assert.equal(replaced.state.recipientNativeBalance, '0')
  await refresh(); await credit(fixture.expectedCredit, 'unknown')
  assert.equal(await disabled(), true)
  assert.equal(JSON.stringify((await rows())[2].wallet.request), originalRequest)
  assert.ok((await rows())[2].wallet.results.some(value => value.receipts.some(r => r.transactionHash.toLowerCase() === originalHash.toLowerCase())))
  record('canonical removal restores backed credit and preserves all prior receipt hashes while the wallet group is unresolved')

  await page.reload({waitUntil: 'networkidle0', timeout: 20000}); await connect()
  await credit(fixture.expectedCredit, 'unknown'); assert.equal(await disabled(), true)
  assert.equal(JSON.stringify((await rows())[2].wallet.request), originalRequest); assert.equal(requests, 3)
  record('reload after removal retains the exact request id and complete prior receipt history without another signature')

  const restored = await phase('restored'); assert.equal(restored.hash, originalHash); assert.deepEqual(restored.state, mined.state)
  await refresh(); await credit('0', 'executed')
  const finalRows = await rows(); assert.equal(finalRows.length, 3)
  assert.equal(JSON.stringify(finalRows.slice(0, 2)), refusedBytes)
  assert.equal(JSON.stringify(finalRows[2].wallet.request), originalRequest)
  assert.equal(finalRows[2].wallet.request.id, originalCallId)
  assert.equal(finalRows[2].wallet.results.at(-1).receipts[0].transactionHash.toLowerCase(), originalHash.toLowerCase())
  assert.equal(await disabled(), true); assert.equal(requests, 3); assert.equal(transactions.length, 1)
  assert.equal(await page.evaluate(key => localStorage.getItem(key), legacyKey), null)
  record('restoration recovers the same actual payout receipt nonce fee and signed bytes using only the original caller id')

  const ids = new Set([...callBatches.keys()]); assert.equal(ids.size, 3)
  assert.ok(statusQueries.length > 3 && statusQueries.every(id => ids.has(id)))
  assert.equal(methods.filter(method => method === 'wallet_getCapabilities').length, 3)
  assert.equal(methods.filter(method => method === 'wallet_sendCalls').length, 3)
  assert.equal(methods.filter(method => method === 'eth_sendTransaction').length, 0)
  assert.ok(methods.every(method => ['eth_accounts', 'eth_requestAccounts', 'eth_chainId', 'wallet_switchEthereumChain',
    'wallet_getCapabilities', 'wallet_sendCalls', 'wallet_getCallsStatus'].includes(method)))
  record('only connected-account discovery and exact known-id status queries execute without legacy fallback or duplicate broadcasts')
  assert.deepEqual(providerFailures, []); assert.deepEqual(failedHTTP, []); assert.deepEqual(external, []); assert.deepEqual(exceptions, [])
  record('actual production pages finish without external network requests failed HTTP responses or uncaught exceptions')
  assert.equal(result.checks.length, 16); result.pass = true
} catch (error) {
  result.failure = String(error.message).split('\n')[0].slice(0, 240)
  if (page && !page.isClosed()) result.page = await page.evaluate(() => ({path: location.pathname,
    message: document.querySelector('[data-testid="exchange-credits"]')?.textContent.slice(-900),
    buttons: [...document.querySelectorAll('button')].filter(button => button.checkVisibility()).slice(0, 20).map(button => button.textContent.trim().slice(0, 80))})).catch(() => null)
  process.exitCode = 1
} finally {
  if (context) await context.close()
  await stop(); await rm(profile, {recursive: true, force: false})
  console.log('EXECUTED-WALLET-CREDIT-BROWSER-QUALIFICATION ' + JSON.stringify({result, pins, SDKVersion: sdk.version,
    actualProductionPage: true, knownContractAccountAdapter: true, requests, walletRejections, receiverProbeRefusals, savedBeforeRequest,
    providerMethods: methods, providerFailures, actualProviderEVMTransactions: transactions.length,
    signedRawBytes: transactions.map(value => value.rawBytes), originalHash, originalCallId, lostResponses,
    exactSavedIdStatusQueries: statusQueries, walletCallsRetained: callBatches.size, preparationHeader, finalReceipt,
    externalRequests: external.length, failedHTTP, pageExceptions: exceptions.length,
    browserJoined: !!browser?.process() && browser.process().exitCode !== null,
    genericSafeUserOperationOrPhysicalWalletQualified: false, independentNoMOrFullDaemonQualified: false,
    sourceSaleIsDeclaredSetup: true}))
}
