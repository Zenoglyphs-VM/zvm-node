// Compile one self-contained test wallet in the hosted browser job, once.
import assert from 'node:assert/strict'
import {open, mkdir} from 'node:fs/promises'
import {createHash} from 'node:crypto'
import {createRequire} from 'node:module'
import {fileURLToPath} from 'node:url'
import path from 'node:path'

const require = createRequire(import.meta.url)
const {SOLC, readRegular, loadCompiler, sha256} = require('./market-credit-build-support.cjs')
const directory = process.argv[2]
assert.ok(directory && path.isAbsolute(directory), 'an explicit fresh compiler directory is required')
await mkdir(directory, {mode: 0o700})
const compilerPath = path.join(directory, 'soljson.js')
const response = await fetch('https://binaries.soliditylang.org/wasm/soljson-v0.8.30+commit.73712a01.js',
  {redirect: 'error', signal: AbortSignal.timeout(60000)})
assert.ok(response.ok && response.body, 'the pinned compiler response is unavailable')
const length = response.headers.get('content-length')
assert.ok(length === null || (/^\d+$/.test(length) && Number(length) <= SOLC.bytes), 'compiler response byte bound refused')
const fd = await open(compilerPath, 'wx', 0o600)
const reader = response.body.getReader()
const hash = createHash('sha256')
let bytes = 0, complete = false
try {
  for (;;) {
    const chunk = await reader.read()
    if (chunk.done) {complete = true; break}
    assert.ok(chunk.value.byteLength <= SOLC.bytes - bytes, 'compiler response exceeds its exact byte count')
    bytes += chunk.value.byteLength
    hash.update(chunk.value)
    let offset = 0
    while (offset < chunk.value.byteLength) {
      const written = await fd.write(chunk.value, offset, chunk.value.byteLength - offset)
      assert.ok(written.bytesWritten > 0, 'compiler stream write made no progress')
      offset += written.bytesWritten
    }
  }
  assert.equal(bytes, SOLC.bytes, 'compiler response was incomplete')
  assert.equal(hash.digest('hex'), SOLC.sha256, 'compiler response digest differs')
  await fd.sync()
} finally {
  if (!complete) void reader.cancel().catch(() => {})
  reader.releaseLock()
  await fd.close()
}

const source = readRegular(fileURLToPath(new URL('../orderbook/testdata/CreditFixtureWallet.sol', import.meta.url)), 16 << 10)
const royaltySource = readRegular(fileURLToPath(new URL('../orderbook/testdata/RoyaltyFixtureCollection.sol', import.meta.url)), 16 << 10)
const signatureSource = readRegular(fileURLToPath(new URL('../orderbook/testdata/ContractSignatureFixtureWallet.sol', import.meta.url)), 16 << 10)
const compiler = loadCompiler(compilerPath, SOLC)
const result = compiler.compile({language: 'Solidity', sources: {'CreditFixtureWallet.sol': {content: source.toString('utf8')},
  'RoyaltyFixtureCollection.sol': {content: royaltySource.toString('utf8')},
  'ContractSignatureFixtureWallet.sol': {content: signatureSource.toString('utf8')}},
  settings: {optimizer: {enabled: true, runs: 200}, evmVersion: 'prague', viaIR: false,
    outputSelection: {'*': {'*': ['abi', 'evm.bytecode.object', 'evm.deployedBytecode.object']}}}})
compiler.verify()
const wallet = result.output.contracts['CreditFixtureWallet.sol'].CreditFixtureWallet
assert.ok(wallet && wallet.evm.bytecode.object.length > 0 && wallet.evm.bytecode.object.length <= 16 << 10)
assert.ok(wallet.evm.deployedBytecode.object.length > 0 && wallet.evm.deployedBytecode.object.length <= 16 << 10)
const artifact = {schema: 1, sourceName: 'CreditFixtureWallet.sol', sourceSha256: sha256(source),
  compilerVersion: SOLC.version, compilerSha256: SOLC.sha256,
  supportSha256: sha256(readRegular(fileURLToPath(new URL('./market-credit-build-support.cjs', import.meta.url)), 64 << 10)),
  abi: wallet.abi, bytecode: '0x' + wallet.evm.bytecode.object, deployedBytecode: '0x' + wallet.evm.deployedBytecode.object}
const raw = Buffer.from(JSON.stringify(artifact) + '\n')
assert.ok(raw.length <= 64 << 10, 'compiled wallet artifact exceeds its bound')
const output = await open(path.join(directory, 'wallet.json'), 'wx', 0o600)
try {await output.writeFile(raw); await output.sync()} finally {await output.close()}
console.log('CREDIT-WALLET-COMPILER ' + JSON.stringify({sourceSha256: artifact.sourceSha256,
  compilerVersion: SOLC.version, compilerSha256: SOLC.sha256, compilerBytes: bytes,
  supportSha256: artifact.supportSha256, artifactSha256: sha256(raw), artifactBytes: raw.length,
  dependencyOrBrowserInstallations: 0, compiledTargets: 1}))

// A second independent target shares the exact same bounded compiler transport.
const collection = result.output.contracts['RoyaltyFixtureCollection.sol'].RoyaltyFixtureCollection
assert.ok(collection && collection.evm.bytecode.object.length > 0 && collection.evm.bytecode.object.length <= 16 << 10)
assert.ok(collection.evm.deployedBytecode.object.length > 0 && collection.evm.deployedBytecode.object.length <= 16 << 10)
const royaltyArtifact = {schema: 1, sourceName: 'RoyaltyFixtureCollection.sol', sourceSha256: sha256(royaltySource),
  compilerVersion: SOLC.version, compilerSha256: SOLC.sha256, supportSha256: artifact.supportSha256,
  abi: collection.abi, bytecode: '0x' + collection.evm.bytecode.object, deployedBytecode: '0x' + collection.evm.deployedBytecode.object}
const royaltyRaw = Buffer.from(JSON.stringify(royaltyArtifact) + '\n')
assert.ok(royaltyRaw.length <= 64 << 10, 'compiled external royalty artifact exceeds its bound')
const royaltyOutput = await open(path.join(directory, 'royalty.json'), 'wx', 0o600)
try {await royaltyOutput.writeFile(royaltyRaw); await royaltyOutput.sync()} finally {await royaltyOutput.close()}
console.log('EXTERNAL-ROYALTY-COMPILER ' + JSON.stringify({sourceSha256: royaltyArtifact.sourceSha256,
  compilerVersion: SOLC.version, compilerSha256: SOLC.sha256, supportSha256: royaltyArtifact.supportSha256,
  artifactSha256: sha256(royaltyRaw), artifactBytes: royaltyRaw.length, compilerTransports: 1, totalTargets: 3,
  dependencyOrBrowserInstallations: 0}))


const signatureWallet = result.output.contracts['ContractSignatureFixtureWallet.sol'].ContractSignatureFixtureWallet
assert.ok(signatureWallet && signatureWallet.evm.bytecode.object.length > 0 && signatureWallet.evm.bytecode.object.length <= 16 << 10)
assert.ok(signatureWallet.evm.deployedBytecode.object.length > 0 && signatureWallet.evm.deployedBytecode.object.length <= 16 << 10)
const signatureArtifact = {schema: 1, sourceName: 'ContractSignatureFixtureWallet.sol', sourceSha256: sha256(signatureSource),
  compilerVersion: SOLC.version, compilerSha256: SOLC.sha256, supportSha256: artifact.supportSha256,
  abi: signatureWallet.abi, bytecode: '0x' + signatureWallet.evm.bytecode.object, deployedBytecode: '0x' + signatureWallet.evm.deployedBytecode.object}
const signatureRaw = Buffer.from(JSON.stringify(signatureArtifact) + '\n')
assert.ok(signatureRaw.length <= 64 << 10, 'compiled contract-signature artifact exceeds its bound')
const signatureOutput = await open(path.join(directory, 'signature-wallet.json'), 'wx', 0o600)
try {await signatureOutput.writeFile(signatureRaw); await signatureOutput.sync()} finally {await signatureOutput.close()}
console.log('ERC1271-WALLET-COMPILER ' + JSON.stringify({sourceSha256: signatureArtifact.sourceSha256,
  compilerVersion: SOLC.version, compilerSha256: SOLC.sha256, supportSha256: signatureArtifact.supportSha256,
  artifactSha256: sha256(signatureRaw), artifactBytes: signatureRaw.length, compilerTransports: 1, totalTargets: 3,
  dependencyOrBrowserInstallations: 0}))
