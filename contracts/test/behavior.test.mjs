import assert from 'node:assert/strict'
import test from 'node:test'
import {compilationEvidence, verifyExports} from './compile.mjs'
import {artifacts, INITIAL, MAX, world, ZERO} from './evm.mjs'
import './bridge-fees.mjs'
import './collection-exchange.mjs'
import './farm-strategy.mjs'
import './market-interfaces.mjs'

test('fresh Solidity 0.8.30 compilation matches all 79 original exports with metadata retained', (t) => {
  assert.equal(verifyExports(), 79)
  assert.equal(compilationEvidence.compilerVersion, '0.8.30+commit.73712a01.Emscripten.clang')
  assert.deepEqual(compilationEvidence.metadataOrExecutableByteAllowances, [])
  assert.equal(Object.keys(artifacts).length, 18)
  t.diagnostic(JSON.stringify(compilationEvidence))
})

test('fresh WZNN construction, wrap/unwrap and finite/infinite approvals conserve collateral', async () => {
  const f = await world(), wznn = await f.deploy('WZNN')
  assert.equal(await f.read(wznn, 'name'), 'Wrapped ZNN')
  assert.equal(await f.read(wznn, 'symbol'), 'WZNN')
  await f.write(f.alice, wznn, 'deposit', [], 1000n)
  await f.write(f.alice, wznn, 'approve', [f.bob, 99n])
  await f.write(f.bob, wznn, 'transferFrom', [f.alice, f.carol, 100n], 0n, {error: {name: 'Error', args: ['WZNN: insufficient allowance']}})
  await f.write(f.bob, wznn, 'transferFrom', [f.alice, f.carol, 99n])
  await f.write(f.alice, wznn, 'approve', [f.bob, MAX])
  await f.write(f.bob, wznn, 'transferFrom', [f.alice, f.carol, 1n])
  await f.write(f.carol, wznn, 'withdraw', [100n])
  assert.equal(await f.read(wznn, 'allowance', [f.alice, f.bob]), MAX)
  assert.equal(await f.balance(f.carol), INITIAL + 100n)
  assert.equal(await f.balance(wznn), 900n)
  assert.equal(await f.read(wznn, 'totalSupply'), 900n)
  await f.conserved([wznn])
})

test('original benchmark ERC20/721/1155 constructors, minting and transfers execute unchanged', async () => {
  const f = await world(), token = await f.deploy('BenchERC20', ['Benchmark fixture', 'FIX', 1000n])
  await f.write(f.owner, token, 'airdrop', [[f.alice, f.bob], 100n])
  assert.equal(await f.read(token, 'totalSupply'), 1200n)
  const nft = await f.deploy('BenchERC721'), multi = await f.deploy('BenchERC1155')
  await f.write(f.owner, nft, 'mint', [f.alice]); await f.write(f.alice, nft, 'transferFrom', [f.alice, f.bob, 1n])
  assert.equal(await f.read(nft, 'ownerOf', [1n]), f.bob)
  await f.write(f.owner, multi, 'mint', [f.alice, 7n, 100n])
  await f.write(f.alice, multi, 'safeTransferFrom', [f.alice, f.bob, 7n, 40n, '0x'])
  assert.equal(await f.read(multi, 'balanceOf', [f.alice, 7n]), 60n)
  assert.equal(await f.read(multi, 'balanceOf', [f.bob, 7n]), 40n)
  await f.conserved([token])
})

test('original storage churn writes actual block-derived slots and failed bounded execution restores them', async () => {
  const f = await world(), at = await f.deploy('StorageChurn')
  await f.write(f.owner, at, 'churn', [3n])
  for (let i = 0n; i < 3n; i++) assert.equal(await f.read(at, 'slots', [i]), f.number + i + 1n)
  f.mine(1n); await f.write(f.owner, at, 'churn', [2n])
  assert.equal(await f.read(at, 'slots', [3n]), f.number + 1n)
  assert.equal(await f.read(at, 'counter'), 5n)
  await f.write(f.owner, at, 'churn', [1000n], 0n, {gas: 100_000n, error: true})
  assert.equal(await f.read(at, 'counter'), 5n)
  await f.conserved()
})

test('original disperse pays exact shares, refunds excess and rolls earlier payments back on failure', async () => {
  const f = await world(), at = await f.deploy('Disperse'), wallet = await f.deploy('PayoutWallet')
  await f.write(f.owner, wallet, 'configure', [1n, ZERO, '0x'])
  await f.write(f.owner, at, 'disperse', [[f.alice, wallet], 25n], 100n, {error: {name: 'Error', args: ['send failed']}})
  await f.write(f.owner, at, 'disperse', [[f.alice, f.bob], 25n], 49n, {error: {name: 'Error', args: ['send failed']}})
  await f.write(f.owner, at, 'disperse', [[f.alice, f.bob], 25n], 100n)
  assert.equal(await f.balance(f.owner), INITIAL - 50n)
  assert.equal(await f.balance(f.alice), INITIAL + 25n)
  assert.equal(await f.balance(f.bob), INITIAL + 25n)
  assert.equal(await f.balance(at), 0n)
  await f.conserved()
})
