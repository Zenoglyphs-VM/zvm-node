// Reuse the reviewed bounded source/archive/compiler helpers from the parent.
import assert from 'node:assert/strict'
import {createRequire} from 'node:module'
import {fileURLToPath} from 'node:url'
import path from 'node:path'
import {COMPILER, OPENZEPPELIN} from './pins.mjs'
const require = createRequire(import.meta.url)
const {readRegular, dependencySources, sourceClosure, loadCompiler, sha256, canonical} = require('../build-support.cjs')
const directory = fileURLToPath(new URL('../', import.meta.url))
const compilerFile = process.env.ZVM_TEST_COMPILER_FILE, dependencyFile = process.env.ZVM_TEST_OPENZEPPELIN_TGZ
assert.ok(compilerFile && dependencyFile, 'explicit pinned compiler and dependency files are required; see README')
const {url: compilerUrl, ...compilerPin} = COMPILER
const compiler = loadCompiler(compilerFile, compilerPin)
const {url: dependencyUrl, ...dependencyPin} = OPENZEPPELIN
const dependency = dependencySources(readRegular(dependencyFile, 1 << 20), dependencyPin, {maxUnpackedBytes: 16 << 20})
export const settings = Object.freeze({optimizer: {enabled: true, runs: 200}, evmVersion: 'prague', viaIR: false,
  metadata: {bytecodeHash: 'ipfs', appendCBOR: true, useLiteralContent: false}, libraries: {}, remappings: [],
  outputSelection: {'*': {'*': ['abi', 'metadata', 'storageLayout', 'evm.bytecode.object', 'evm.bytecode.linkReferences',
    'evm.deployedBytecode.object', 'evm.deployedBytecode.linkReferences', 'evm.deployedBytecode.immutableReferences']}}})
export const groups = {}, compiled = new Map(), sourcePins = new Map()
const roots = {
  apps: ['apps/ZVMCollection.sol', 'apps/ZVMCollectionFactory.sol', 'apps/ZVMExchange.sol', 'apps/ZVMFarm.sol', 'apps/ZVMFarmFeeStrategy.sol'],
  bench: ['bench/BenchTokens.sol'], wznn: ['bench/WZNN.sol'],
  system: ['src/WrappedQSR.sol', 'src/ZVMBridge.sol', 'src/ZVMFees.sol'], fixtures: ['test/Fixtures.sol'],
}
for (const [group, names] of Object.entries(roots)) {
  const sources = sourceClosure(names, (unit) => {
    if (unit.startsWith('@openzeppelin/contracts/')) return dependency.sources.get(unit)
    assert.match(unit, /^(?:apps|bench|src|test)\//)
    const bytes = readRegular(path.join(directory, unit), 1 << 20)
    sourcePins.set(unit, sha256(bytes)); return bytes
  })
  const result = compiler.compile({language: 'Solidity', sources, settings})
  groups[group] = result
  for (const [unit, contracts] of Object.entries(result.output.contracts)) for (const [name, value] of Object.entries(contracts))
    compiled.set(unit + ':' + name, value)
}
compiler.verify()
assert.equal(sha256(readRegular(dependencyFile, 1 << 20)), OPENZEPPELIN.sha256)
const projectUnits = {ZVMCollection: 'apps/ZVMCollection.sol', ZVMCollectionFactory: 'apps/ZVMCollectionFactory.sol',
  ZVMExchange: 'apps/ZVMExchange.sol', ZVMFarm: 'apps/ZVMFarm.sol', ZVMFarmFeeStrategy: 'apps/ZVMFarmFeeStrategy.sol',
  BenchERC20: 'bench/BenchTokens.sol', BenchERC721: 'bench/BenchTokens.sol', BenchERC1155: 'bench/BenchTokens.sol',
  StorageChurn: 'bench/BenchTokens.sol', Disperse: 'bench/BenchTokens.sol', WZNN: 'bench/WZNN.sol',
  ZVMBridge: 'src/ZVMBridge.sol', WrappedQSR: 'src/WrappedQSR.sol', ZVMFees: 'src/ZVMFees.sol',
  ControlledToken: 'test/Fixtures.sol', PayoutWallet: 'test/Fixtures.sol', ContractSigner: 'test/Fixtures.sol', RoyaltyNFT: 'test/Fixtures.sol'}
export const artifacts = Object.fromEntries(Object.entries(projectUnits).map(([name, unit]) => {
  const c = compiled.get(unit + ':' + name); assert.ok(c, 'required compiled subject missing')
  assert.ok(c.evm.deployedBytecode.object.length / 2 <= 24_576)
  return [name, {abi: c.abi, bytecode: '0x' + c.evm.bytecode.object, deployedBytecode: '0x' + c.evm.deployedBytecode.object,
    storageLayout: c.storageLayout, immutableReferences: c.evm.deployedBytecode.immutableReferences ?? {}}]
}))
export function verifyExports() {
  let count = 0
  for (const [group, file] of [['apps', '../../apps/artifacts/apps.json'], ['bench', '../../bench/artifacts/bench.json']]) {
    const saved = JSON.parse(readRegular(fileURLToPath(new URL(file, import.meta.url)), 2 << 20))
    for (const [name, exported] of Object.entries(saved.contracts)) {
      const [unit, contract] = name.split(':'), c = groups[group].output.contracts[unit][contract]
      assert.equal(canonical(c.abi), canonical(exported.abi))
      assert.equal(c.evm.bytecode.object, exported.bin)
      assert.equal(c.evm.deployedBytecode.object, exported['bin-runtime'])
      count++
    }
  }
  const saved = JSON.parse(readRegular(fileURLToPath(new URL('../../bench/artifacts/WZNN.json', import.meta.url)), 1 << 20))
  assert.equal(canonical(artifacts.WZNN.abi), canonical(saved.abi)); assert.equal(artifacts.WZNN.bytecode, saved.bytecode); count++
  return count
}
export const compilationEvidence = {compilerVersion: COMPILER.version, compilerSha256: COMPILER.sha256,
  dependencySha256: OPENZEPPELIN.sha256, metadataOrExecutableByteAllowances: [],
  groups: Object.fromEntries(Object.entries(groups).map(([name, g]) => [name, {sources: Object.keys(g.input.sources).length,
    targets: Object.values(g.output.contracts).reduce((n, cs) => n + Object.keys(cs).length, 0)}]))}
