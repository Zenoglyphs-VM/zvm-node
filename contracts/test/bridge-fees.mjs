import assert from 'node:assert/strict'
import test from 'node:test'
import {BRIDGE, BURN, errorName, INITIAL, MAX, QSR, QSR_ZTS, random, SCALE, SYSTEM, world, ZNN_ZTS, ZERO} from './evm.mjs'
const hash = (n) => '0x' + BigInt(n).toString(16).padStart(64, '0')
const event = (f, result, name, at) => f.events(result, at).find((e) => e.eventName === name)?.args
const fee = (amount, floor) => amount * 25n / 10_000n > floor ? amount * 25n / 10_000n : floor

test('explicit bridge genesis initializes only the declared parameter slots and enforces system authority', async () => {
  const f = await world(); await f.installBridge()
  assert.equal(await f.read(BRIDGE, 'feeBps'), 25n)
  assert.equal(await f.read(BRIDGE, 'minFeeZNN'), 5n)
  assert.equal(await f.read(BRIDGE, 'minFeeQSR'), 3n)
  assert.equal(await f.read(QSR, 'decimals'), 8)
  for (const [to, method, args] of [[QSR, 'bridgeMint', [f.alice, 1n]], [QSR, 'bridgeBurn', [f.alice, 1n]],
    [BRIDGE, 'depositQSR', [hash(1), f.alice, 1n]], [BRIDGE, 'markPaid', [1n, hash(2), f.alice, ZNN_ZTS, 1n]],
    [BRIDGE, 'markFused', [hash(3), f.alice, 1n]]])
    await f.write(f.outsider, to, method, args, 0n, {error: to === QSR ? 'OnlyBridge' : 'OnlySystem'})
  await f.conserved([QSR])
})

test('real bridge calls mint QSR, honor finite/infinite allowances and burn the full gross withdrawal', async () => {
  const f = await world(); await f.installBridge()
  await f.write(SYSTEM, BRIDGE, 'depositQSR', [hash(1), f.alice, 10_000n])
  assert.equal(await f.read(BRIDGE, 'depositedQSR'), 10_000n)
  await f.write(f.alice, QSR, 'approve', [f.bob, 100n])
  await f.write(f.bob, QSR, 'transferFrom', [f.alice, f.carol, 101n], 0n, {error: 'InsufficientAllowance'})
  await f.write(f.bob, QSR, 'transferFrom', [f.alice, f.carol, 100n])
  assert.equal(await f.read(QSR, 'allowance', [f.alice, f.bob]), 0n)
  await f.write(f.alice, QSR, 'approve', [f.bob, MAX])
  await f.write(f.bob, QSR, 'transferFrom', [f.alice, f.carol, 1n])
  assert.equal(await f.read(QSR, 'allowance', [f.alice, f.bob]), MAX)
  const result = await f.write(f.alice, BRIDGE, 'withdrawQSR', [f.bob, 1000n])
  assert.deepEqual(event(f, result, 'WithdrawalInitiated', BRIDGE), {id: 1n, from: f.alice, to: f.bob, zts: QSR_ZTS, amount: 997n})
  assert.equal(await f.read(QSR, 'totalSupply'), 9000n)
  assert.equal(await f.read(BRIDGE, 'pendingQSR'), 997n)
  assert.equal(await f.read(BRIDGE, 'feesQSR'), 3n)
  const withdrawal = await f.read(BRIDGE, 'withdrawal', [1n])
  assert.equal(withdrawal.fee, 3n); assert.equal(withdrawal.l2Block, f.number)
  await f.conserved([QSR])
})

test('native withdrawals enforce scale, uint64 bounds, net-positive fees and NoM user recipients atomically', async () => {
  const f = await world(); await f.installBridge()
  for (const value of [0n, SCALE - 1n, 5n * SCALE, (1n << 64n) * SCALE])
    await f.write(f.alice, BRIDGE, 'withdrawZNN', [f.bob], value, {error: 'BadAmount'})
  const wrongType = '0x01' + '00'.repeat(19)
  for (const to of [ZERO, wrongType]) await f.write(f.alice, BRIDGE, 'withdrawZNN', [to], 1000n * SCALE, {error: 'NotAUserAddress'})
  const result = await f.write(f.alice, BRIDGE, 'withdrawZNN', [f.bob], 1000n * SCALE)
  assert.equal(event(f, result, 'WithdrawalInitiated', BRIDGE).amount, 995n)
  assert.equal(await f.balance(BRIDGE), 1000n * SCALE)
  assert.equal(await f.read(BRIDGE, 'pendingZNN'), 995n)
  assert.equal(await f.read(BRIDGE, 'feesZNN'), 5n)
  await f.conserved([QSR])
})

test('a late invalid QSR recipient reverses the actual token burn and every bridge record', async () => {
  const f = await world(); await f.installBridge()
  await f.write(SYSTEM, BRIDGE, 'depositQSR', [hash(1), f.alice, 1000n])
  await f.write(f.alice, BRIDGE, 'withdrawQSR', [ZERO, 100n], 0n, {error: 'NotAUserAddress'})
  await f.write(f.alice, BRIDGE, 'withdrawQSR', [f.bob, 1001n], 0n, {error: 'InsufficientBalance'})
  assert.equal(await f.read(QSR, 'totalSupply'), 1000n)
  assert.equal(await f.read(QSR, 'balanceOf', [f.alice]), 1000n)
  assert.equal(await f.read(BRIDGE, 'withdrawalCount'), 0n)
  await f.conserved([QSR])
})

test('payout/fusion receipts separate unknown, mismatched, duplicate and already-paid facts', async () => {
  const f = await world(); await f.installBridge()
  await f.write(f.alice, BRIDGE, 'withdrawZNN', [f.bob], 1000n * SCALE)
  const pay = (id, n, to = f.bob, zts = ZNN_ZTS, amount = 995n) => f.write(SYSTEM, BRIDGE, 'markPaid', [id, hash(n), to, zts, amount])
  assert.equal(event(f, await pay(9n, 1), 'PayoutAnomaly', BRIDGE).reason, 1)
  assert.equal(event(f, await pay(1n, 2, f.carol), 'PayoutAnomaly', BRIDGE).reason, 3)
  assert.equal(f.events(await pay(1n, 2), BRIDGE).length, 0)
  assert.deepEqual(event(f, await pay(1n, 3), 'WithdrawalPaid', BRIDGE), {id: 1n, nomBlock: hash(3)})
  assert.equal(await f.read(BRIDGE, 'pendingZNN'), 0n)
  assert.equal(await f.read(BRIDGE, 'paidZNN'), 995n)
  assert.equal(event(f, await pay(1n, 4), 'PayoutAnomaly', BRIDGE).reason, 2)
  await f.write(SYSTEM, BRIDGE, 'markFused', [hash(5), f.alice, 7n])
  await f.write(SYSTEM, BRIDGE, 'markFused', [hash(5), f.alice, 7n])
  assert.equal(await f.read(BRIDGE, 'fusedQSR'), 7n)
  assert.equal(await f.read(BRIDGE, 'fundableQSR'), 0n)
  await f.conserved([QSR])
})

test('seeded bridge histories match independent gross/net, token-supply and pending/paid ledgers', async (t) => {
  for (const seed of [3, 17, 0x12345678, 0x9e3779b9]) await t.test('seed ' + seed, async () => {
    const f = await world(); await f.installBridge()
    const next = random(seed), users = [f.alice, f.bob, f.carol], tokens = new Map(users.map((a) => [a, 0n]))
    const native = new Map(users.map((a) => [a, INITIAL])), records = new Map()
    let deposited = 0n, supply = 0n, locked = 0n, pendingZnn = 0n, pendingQsr = 0n, paidZnn = 0n, paidQsr = 0n, feesZnn = 0n, feesQsr = 0n, fused = 0n
    for (let step = 0; step < 64; step++) {
      const actor = users[next() % 3], amount = 6n + BigInt(next() % 2000), kind = step === 0 ? 0 : next() % 5, block = hash(seed + step + 1)
      if (kind === 0) {
        await f.write(SYSTEM, BRIDGE, 'depositQSR', [block, actor, amount]); deposited += amount; supply += amount; tokens.set(actor, tokens.get(actor) + amount)
      } else if (kind === 1 || kind === 2) {
        const nativePayment = kind === 1, charge = fee(amount, nativePayment ? 5n : 3n), to = users[(users.indexOf(actor) + 1) % 3]
        if (!nativePayment && tokens.get(actor) < amount) await f.write(actor, BRIDGE, 'withdrawQSR', [to, amount], 0n, {error: 'InsufficientBalance'})
        else {
          const result = nativePayment ? await f.write(actor, BRIDGE, 'withdrawZNN', [to], amount * SCALE) : await f.write(actor, BRIDGE, 'withdrawQSR', [to, amount])
          const id = BigInt(records.size + 1), net = amount - charge
          assert.equal(event(f, result, 'WithdrawalInitiated', BRIDGE).id, id)
          records.set(id, {from: actor, to, zts: nativePayment ? ZNN_ZTS : QSR_ZTS, net, paid: false})
          if (nativePayment) {native.set(actor, native.get(actor) - amount * SCALE); locked += amount * SCALE; pendingZnn += net; feesZnn += charge}
          else {tokens.set(actor, tokens.get(actor) - amount); supply -= amount; pendingQsr += net; feesQsr += charge}
        }
      } else if (kind === 3 && records.size) {
        const id = BigInt(1 + next() % records.size), r = records.get(id)
        const result = await f.write(SYSTEM, BRIDGE, 'markPaid', [id, block, r.to, r.zts, r.net])
        if (r.paid) assert.equal(event(f, result, 'PayoutAnomaly', BRIDGE).reason, 2)
        else {
          r.paid = true
          if (r.zts === ZNN_ZTS) {pendingZnn -= r.net; paidZnn += r.net} else {pendingQsr -= r.net; paidQsr += r.net}
          assert.equal(event(f, result, 'WithdrawalPaid', BRIDGE).id, id)
        }
      } else {await f.write(SYSTEM, BRIDGE, 'markFused', [block, actor, amount]); fused += amount}
      for (const a of users) {assert.equal(await f.read(QSR, 'balanceOf', [a]), tokens.get(a)); assert.equal(await f.balance(a), native.get(a))}
      for (const [name, value] of Object.entries({withdrawalCount: BigInt(records.size), pendingZNN: pendingZnn, pendingQSR: pendingQsr,
        paidZNN: paidZnn, paidQSR: paidQsr, depositedQSR: deposited, feesZNN: feesZnn, feesQSR: feesQsr, fusedQSR: fused})) assert.equal(await f.read(BRIDGE, name), value)
      assert.equal(await f.read(QSR, 'totalSupply'), supply)
      assert.equal(await f.balance(BRIDGE), locked)
      assert.equal(pendingZnn + paidZnn + feesZnn, locked / SCALE)
      assert.equal(pendingQsr + paidQsr + feesQsr, deposited - supply)
      assert.equal(await f.read(BRIDGE, 'fundableQSR'), feesQsr > fused ? feesQsr - fused : 0n)
      await f.conserved([QSR])
    }
  })
})

test('fees settlement excludes existing liabilities, applies dust and lets relayers pull their shares', async () => {
  const f = await world(), fees = await f.deploy('ZVMFees')
  await f.frame({from: f.owner, to: fees, value: 10n * SCALE})
  await f.write(f.outsider, fees, 'settle', [1n, [f.alice], [1n]], 0n, {error: 'OnlySystem'})
  const before = await f.root(); await f.write(SYSTEM, fees, 'settle', [1n, [f.alice], []]); assert.equal(await f.root(), before)
  await f.write(SYSTEM, fees, 'settle', [1n, [f.alice, f.bob], [1n, 3n]])
  assert.equal(await f.read(fees, 'owed', [f.alice]), 125n * SCALE / 100n)
  assert.equal(await f.read(fees, 'owed', [f.bob]), 375n * SCALE / 100n)
  assert.equal(await f.balance(BURN), 5n * SCALE)
  const recipient = await f.balance(f.carol)
  await f.write(f.alice, fees, 'withdraw', [f.carol])
  assert.equal(await f.balance(f.carol), recipient + 125n * SCALE / 100n)
  const unchanged = await f.root(); await f.write(SYSTEM, fees, 'settle', [2n, [f.alice], [1n]]); assert.equal(await f.root(), unchanged)
  await f.write(f.alice, fees, 'withdraw', [f.carol], 0n, {error: 'NothingOwed'})
  await f.conserved()
})

test('rejected and reentrant fee withdrawals preserve liabilities and cannot pay a relayer twice', async () => {
  const f = await world(), fees = await f.deploy('ZVMFees'), wallet = await f.deploy('PayoutWallet')
  await f.frame({from: f.owner, to: fees, value: 10n * SCALE})
  await f.write(SYSTEM, fees, 'settle', [1n, [wallet], [1n]])
  await f.write(f.owner, wallet, 'configure', [1n, ZERO, '0x'])
  await f.forward(wallet, fees, 'withdraw', [wallet], 0n, {error: 'TransferFailed'})
  assert.equal(await f.read(fees, 'owed', [wallet]), 5n * SCALE)
  await f.write(f.owner, wallet, 'configure', [2n, fees, f.calldata(fees, 'withdraw', [wallet])])
  const result = await f.forward(wallet, fees, 'withdraw', [wallet]), callback = event(f, result, 'Callback', wallet)
  assert.equal(callback.ok, false); assert.equal(errorName(callback.result), 'NothingOwed')
  assert.equal(await f.balance(wallet), 5n * SCALE)
  assert.equal(await f.read(fees, 'totalPaid'), 5n * SCALE)
  assert.equal(await f.read(fees, 'totalOwed'), 0n)
  await f.conserved()
})

test('burn mirroring records duplicate/excess anomalies and a nonnegative remaining gap', async () => {
  const f = await world(), fees = await f.deploy('ZVMFees')
  await f.frame({from: f.owner, to: fees, value: 20n * SCALE})
  await f.write(SYSTEM, fees, 'settle', [1n, [], []])
  await f.write(f.alice, fees, 'markBurnedOnNoM', [hash(1), SCALE], 0n, {error: 'OnlySystem'})
  await f.write(SYSTEM, fees, 'markBurnedOnNoM', [hash(1), SCALE])
  assert.equal(event(f, await f.write(SYSTEM, fees, 'markBurnedOnNoM', [hash(1), SCALE]), 'BurnAnomaly', fees).reason, 1)
  assert.equal(await f.read(fees, 'unmirrored'), 19n * SCALE)
  assert.equal(event(f, await f.write(SYSTEM, fees, 'markBurnedOnNoM', [hash(2), 30n * SCALE]), 'BurnAnomaly', fees).reason, 2)
  assert.equal(await f.read(fees, 'unmirrored'), 0n)
  await f.conserved()
})

test('seeded fee histories match independent pot, owed, paid and burn accounting', async (t) => {
  for (const seed of [3, 17, 0x12345678, 0x9e3779b9]) await t.test('seed ' + seed, async () => {
    const f = await world(), fees = await f.deploy('ZVMFees'), users = [f.alice, f.bob, f.carol], next = random(seed)
    const owed = new Map(users.map((a) => [a, 0n])), received = new Map(users.map((a) => [a, 0n]))
    let balance = 0n, totalOwed = 0n, collected = 0n, paid = 0n, burned = 0n, funded = 0n
    for (let step = 0; step < 64; step++) {
      if (step === 0 || next() % 3 === 0) {
        const amount = BigInt(1 + next() % 1000) * 10n ** 8n
        await f.frame({from: f.owner, to: fees, value: amount}); balance += amount; funded += amount
      } else if (next() % 2 === 0) {
        const weights = users.map(() => BigInt(next() % 9)), total = weights.reduce((a, b) => a + b, 0n), pot = balance - totalOwed
        const result = await f.write(SYSTEM, fees, 'settle', [BigInt(step), users, weights])
        if (pot !== 0n) {
          let credited = 0n
          for (let i = 0; i < users.length; i++) {
            const share = total ? pot * weights[i] / (2n * total) : 0n, value = share < 10n ** 9n ? 0n : share
            owed.set(users[i], owed.get(users[i]) + value); credited += value
          }
          const burn = pot - credited; totalOwed += credited; burned += burn; balance -= burn; collected += pot
          assert.equal(event(f, result, 'Settled', fees).credited, credited)
        }
      } else {
        const a = users[next() % users.length], value = owed.get(a)
        await f.write(a, fees, 'withdraw', [a], 0n, value ? {} : {error: 'NothingOwed'})
        if (value) {owed.set(a, 0n); received.set(a, received.get(a) + value); balance -= value; totalOwed -= value; paid += value}
      }
      assert.equal(await f.balance(f.owner), INITIAL - funded)
      for (const a of users) {assert.equal(await f.read(fees, 'owed', [a]), owed.get(a)); assert.equal(await f.balance(a), INITIAL + received.get(a))}
      for (const [name, value] of Object.entries({totalOwed, totalCollected: collected, totalPaid: paid, totalBurned: burned})) assert.equal(await f.read(fees, name), value)
      assert.equal(await f.balance(fees), balance); assert.equal(await f.balance(BURN), burned)
      assert.equal(funded, paid + burned + balance)
      assert.equal(collected, paid + totalOwed + burned)
      await f.conserved()
    }
  })
})
