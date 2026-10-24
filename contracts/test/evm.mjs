import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {createBlock} from '@ethereumjs/block'
import {createCustomCommon, Hardfork, Mainnet} from '@ethereumjs/common'
import {createAccount, createAddressFromString, bytesToHex, hexToBytes} from '@ethereumjs/util'
import {createVM} from '@ethereumjs/vm'
import {privateKeyToAccount} from 'viem/accounts'
import {decodeErrorResult, decodeEventLog, decodeFunctionResult, encodeDeployData, encodeFunctionData, getAddress} from 'viem'
import {artifacts, compiled} from './compile.mjs'

export {artifacts}
export const ZERO = '0x0000000000000000000000000000000000000000', BURN = '0x000000000000000000000000000000000000dEaD'
export const BRIDGE = '0x4200000000000000000000000000000000000100', QSR = '0x4200000000000000000000000000000000000101'
export const SYSTEM = '0xDeAddEAddeADdeADdEaDdEaddeadDeADdEAD0002'
export const ZNN_ZTS = '0x14e66318c6318c6318c6', QSR_ZTS = '0x04066318c6318c6318c6'
export const MAX = (1n << 256n) - 1n, SCALE = 10n ** 10n, INITIAL = 10n ** 30n, START = 1_900_000_000n
// Public scalar one is exclusively an offline typed-order fixture. No wallet
// files, network client, canonical transaction signing or real funds are used.
export const signer = privateKeyToAccount('0x' + '00'.repeat(31) + '01')
const errors = [...new Map([...compiled.values()].flatMap((c) => c.abi.filter((a) => a.type === 'error')).map((e) => [JSON.stringify(e), e])).values()]
export function errorName(bytes) {return decodeErrorResult({abi: errors, data: bytes}).errorName}
export function random(seed) {let s = seed >>> 0; return () => {s ^= s << 13; s ^= s >>> 17; s ^= s << 5; return s >>> 0}}

export async function world() {
  const common = createCustomCommon({chainId: 7340469}, Mainnet, {hardfork: Hardfork.Osaka}), vm = await createVM({common})
  const actors = [...Array.from({length: 8}, (_, i) => getAddress('0x' + (0x10000n + BigInt(i)).toString(16).padStart(40, '0'))), signer.address]
  const [owner, alice, bob, carol, dan, royalty, revenue, outsider] = actors
  const address = createAddressFromString, abis = new Map(), inventory = new Set([...actors, ZERO, BURN, SYSTEM].map((a) => a.toLowerCase()))
  for (const at of actors) await vm.stateManager.putAccount(address(at), createAccount({balance: INITIAL}))
  await vm.stateManager.putAccount(address(SYSTEM), createAccount({nonce: 1n}))
  let now = START, number = 100n, genesisInstalled = false
  const root = async () => bytesToHex(await vm.stateManager.getStateRoot())
  const balance = async (at) => (await vm.stateManager.getAccount(address(at)))?.balance ?? 0n
  function mine(seconds = 1n) {assert.ok(seconds >= 0n); now += seconds; number++}
  async function frame({from = owner, to, data = '0x', value = 0n, gas = 50_000_000n, isStatic = false, error}) {
    vm.stateManager.originalStorageCache.clear(); vm.evm.journal.cleanJournal()
    const before = to ? await root() : undefined
    const block = createBlock({header: {number, timestamp: now, gasLimit: 60_000_000n}}, {common})
    const result = await vm.evm.runCall({caller: address(from), ...(to ? {to: address(to)} : {}), data: hexToBytes(data),
      value, gasLimit: gas, block, isStatic, skipNonceIncrement: Boolean(to)})
    const returned = bytesToHex(result.execResult.returnValue), failed = Boolean(result.execResult.exceptionError)
    if (error !== undefined) {
      assert.ok(failed, 'expected actual EVM failure')
      if (typeof error === 'string') assert.equal(errorName(returned), error)
      else if (typeof error === 'object') {
        const decoded = decodeErrorResult({abi: errors, data: returned})
        assert.equal(decoded.errorName, error.name); if (error.args) assert.deepEqual(decoded.args, error.args)
      }
      if (to) assert.equal(await root(), before, 'failed frames restore the entire state root, including balances')
      assert.equal(result.execResult.logs?.length ?? 0, 0, 'failed frames retain no logs')
    } else assert.equal(result.execResult.exceptionError, undefined, `EVM frame failed: ${returned}`)
    return {...result, returned, failed}
  }
  function register(at, abi) {abis.set(at.toLowerCase(), abi); inventory.add(at.toLowerCase()); return getAddress(at)}
  async function deploy(code, args = [], options = {}) {
    if (typeof code === 'string') code = artifacts[code]
    const bytecode = code.bytecode.startsWith('0x') ? code.bytecode : '0x' + code.bytecode
    const result = await frame({data: encodeDeployData({abi: code.abi, bytecode, args}), ...options})
    if (result.failed) return result
    const at = getAddress(result.createdAddress.toString()); assert.ok((await vm.stateManager.getCode(address(at))).length > 0)
    register(at, code.abi); return at
  }
  const calldata = (to, name, args = []) => encodeFunctionData({abi: abis.get(to.toLowerCase()), functionName: name, args})
  const write = (from, to, name, args = [], value = 0n, options = {}) => frame({from, to, data: calldata(to, name, args), value, ...options})
  async function read(to, name, args = []) {
    const result = await write(owner, to, name, args, 0n, {isStatic: true})
    return decodeFunctionResult({abi: abis.get(to.toLowerCase()), functionName: name, data: result.returned})
  }
  function events(result, only) {
    return (result.execResult.logs ?? []).filter(([at]) => !only || bytesToHex(at).toLowerCase() === only.toLowerCase()).map(([at, topics, data]) => {
      const location = bytesToHex(at), abi = abis.get(location.toLowerCase()); assert.ok(abi, 'event emitter ABI must be registered')
      return {address: location, ...decodeEventLog({abi, topics: topics.map(bytesToHex), data: bytesToHex(data)})}
    })
  }
  const forward = (wallet, to, name, args = [], value = 0n, options = {}) => write(outsider, wallet, 'execute', [to, calldata(to, name, args)], value, options)
  async function token(name = 'ControlledToken', args = []) {return deploy(name, args)}
  async function installBridge({feeBps = 25n, minFeeZNN = 5n, minFeeQSR = 3n} = {}) {
    assert.equal(genesisInstalled, false); genesisInstalled = true
    for (const [at, name] of [[BRIDGE, 'ZVMBridge'], [QSR, 'WrappedQSR']]) {
      assert.equal(Object.keys(artifacts[name].immutableReferences).length, 0)
      await vm.stateManager.putAccount(address(at), createAccount({nonce: 1n}))
      await vm.stateManager.putCode(address(at), hexToBytes(artifacts[name].deployedBytecode)); register(at, artifacts[name].abi)
    }
    // Exactly three explicit genesis parameter writes, before any execution.
    // Synthetic SYSTEM calls assume authority; no NoM derivation is established.
    for (const [name, slot, value] of [['feeBps', 10n, feeBps], ['minFeeZNN', 11n, minFeeZNN], ['minFeeQSR', 12n, minFeeQSR]]) {
      const field = artifacts.ZVMBridge.storageLayout.storage.find((x) => x.label === name)
      assert.equal(BigInt(field.slot), slot); assert.equal(field.offset, 0)
      const word = (x) => hexToBytes('0x' + x.toString(16).padStart(64, '0'))
      await vm.stateManager.putStorage(address(BRIDGE), word(slot), word(value))
    }
    return {bridge: BRIDGE, qsr: QSR}
  }
  async function collection({creator = alice, bps = 500, name = 'Fixture collection'} = {}) {
    const implementation = await deploy('ZVMCollection'), factory = await deploy('ZVMCollectionFactory', [implementation])
    const result = await write(creator, factory, 'createCollection', [name, 'FIX', 'ipfs://fixture-contract', 'ipfs://fixture/', bps])
    const at = events(result, factory).find((e) => e.eventName === 'CollectionCreated').args.collection
    register(at, artifacts.ZVMCollection.abi); return {collection: at, implementation, factory, result}
  }
  async function dex() {
    const asset = (name) => JSON.parse(readFileSync(new URL('../../bench/artifacts/' + name + '.json', import.meta.url)))
    const wznn = await deploy('WZNN'), factory = await deploy(asset('UniswapV2Factory'), [owner])
    const router = await deploy(asset('UniswapV2Router02'), [factory, wznn]), pairArtifact = asset('UniswapV2Pair')
    async function addPair(a, b, amountA, amountB) {
      for (const [t, amount] of [[a, amountA], [b, amountB]]) await write(owner, t, 'approve', [router, amount])
      await write(owner, router, 'addLiquidity', [a, b, amountA, amountB, 0n, 0n, owner, now])
      const pair = await read(factory, 'getPair', [a, b]); register(pair, pairArtifact.abi)
      assert.equal(bytesToHex(await vm.stateManager.getCode(address(pair))), '0x' + pairArtifact.evm.deployedBytecode.object)
      return pair
    }
    return {wznn, factory, router, addPair}
  }
  async function conserved(tokens = []) {
    let native = 0n
    for (const at of inventory) native += await balance(at)
    assert.equal(native, BigInt(actors.length) * INITIAL, 'all known synthetic native value is conserved')
    for (const t of tokens) {
      let sum = 0n
      for (const at of inventory) sum += await read(t, 'balanceOf', [at])
      assert.equal(sum, await read(t, 'totalSupply'), 'known ERC20 inventory equals actual supply')
    }
  }
  return {vm, actors, owner, alice, bob, carol, dan, royalty, revenue, outsider, signer: signer.address, root, balance, mine, frame,
    register, deploy, calldata, write, read, events, forward, token, installBridge, collection, dex, conserved,
    get now() {return now}, get number() {return number}}
}
