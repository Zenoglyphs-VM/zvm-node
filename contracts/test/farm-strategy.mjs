import assert from 'node:assert/strict'
import test from 'node:test'
import {errorName, MAX, QSR, random, SYSTEM, world, ZERO} from './evm.mjs'
const events = (f, r, name, at) => f.events(r, at).filter((e) => e.eventName === name).map((e) => e.args)
const sum = (values) => values.reduce((a, b) => a + b, 0n)

async function farmWorld(rate = 10n) {
  const f = await world(), reward = await f.deploy('BenchERC20', ['Fixture reward', 'RWD', 10n ** 12n])
  const stake = await f.deploy('BenchERC20', ['Fixture stake', 'STK', 10n ** 12n]), farm = await f.deploy('ZVMFarm', [reward, rate, f.owner])
  await f.write(f.owner, farm, 'addPool', [stake, 100n])
  for (const user of [f.alice, f.bob, f.carol]) {
    await f.write(f.owner, stake, 'transfer', [user, 10_000n]); await f.write(user, stake, 'approve', [farm, MAX])
  }
  await f.write(f.owner, reward, 'approve', [farm, MAX])
  return Object.assign(f, {reward, stake, farm})
}

test('farm constructor/owner checks, duplicate pools and the full 64-pool limit execute actual state', async () => {
  const f = await farmWorld()
  await f.deploy('ZVMFarm', [f.reward, 0n, f.owner], {error: 'ZeroRate'})
  await f.deploy('ZVMFarm', [f.reward, 1n, ZERO], {error: 'OwnableInvalidOwner'})
  for (const [name, args] of [['addPool', [f.reward, 1n]], ['setPool', [0n, 1n]], ['setRewardPerSecond', [1n]]])
    await f.write(f.outsider, f.farm, name, args, 0n, {error: 'OwnableUnauthorizedAccount'})
  await f.write(f.owner, f.farm, 'addPool', [f.stake, 1n], 0n, {error: 'DuplicatePool'})
  await f.write(f.owner, f.farm, 'setPool', [1n, 1n], 0n, {error: 'UnknownPool'})
  await f.write(f.owner, f.farm, 'setRewardPerSecond', [0n], 0n, {error: 'ZeroRate'})
  for (let i = 1; i <= 64; i++) {
    const token = await f.deploy('BenchERC20', ['Cap fixture', 'CAP', 0n])
    await f.write(f.owner, f.farm, 'addPool', [token, 1n], 0n, i === 64 ? {error: 'TooManyPools'} : {})
  }
  assert.equal(await f.read(f.farm, 'poolLength'), 64n)
  await f.conserved([f.reward, f.stake])
})

test('time accrual, signed debt, harvest and rate changes obey an independent hand-calculated example', async () => {
  const f = await farmWorld()
  await f.write(f.owner, f.farm, 'fund', [1003n])
  assert.equal(await f.read(f.farm, 'unscheduled'), 3n)
  f.mine(5n); await f.write(f.alice, f.farm, 'deposit', [0n, 100n, f.alice])
  f.mine(10n); await f.write(f.bob, f.farm, 'deposit', [0n, 100n, f.bob])
  f.mine(10n)
  assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.alice]), 150n)
  assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.bob]), 50n)
  await f.write(f.alice, f.farm, 'withdraw', [0n, 40n, f.alice])
  assert.equal((await f.read(f.farm, 'positions', [0n, f.alice]))[1], -60n)
  f.mine(5n)
  assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.alice]), 168n)
  assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.bob]), 81n)
  await f.write(f.alice, f.farm, 'harvest', [0n, f.alice]); await f.write(f.bob, f.farm, 'harvestMany', [[0n], f.bob])
  assert.equal(await f.read(f.reward, 'balanceOf', [f.alice]), 168n)
  assert.equal(await f.read(f.reward, 'balanceOf', [f.bob]), 81n)
  await f.write(f.owner, f.farm, 'setRewardPerSecond', [7n])
  assert.equal(await f.read(f.farm, 'remainingRewards'), 703n)
  assert.equal(await f.read(f.farm, 'endTime'), f.now + 100n)
  assert.equal(await f.read(f.farm, 'unscheduled'), 3n)
  await f.conserved([f.reward, f.stake])
})

test('weighted pools checkpoint the old allocation before a change and zero allocation emits no reward', async () => {
  const f = await farmWorld(100n), second = await f.deploy('BenchERC20', ['Second stake', 'TWO', 1000n])
  await f.write(f.owner, f.farm, 'setPool', [0n, 1n]); await f.write(f.owner, f.farm, 'addPool', [second, 3n])
  await f.write(f.owner, second, 'transfer', [f.bob, 100n]); await f.write(f.bob, second, 'approve', [f.farm, MAX])
  await f.write(f.owner, f.farm, 'fund', [10_000n])
  await f.write(f.alice, f.farm, 'deposit', [0n, 100n, f.alice]); await f.write(f.bob, f.farm, 'deposit', [1n, 100n, f.bob])
  f.mine(4n)
  assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.alice]), 100n)
  assert.equal(await f.read(f.farm, 'pendingReward', [1n, f.bob]), 300n)
  await f.write(f.owner, f.farm, 'setPool', [0n, 3n]); f.mine(2n)
  assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.alice]), 200n)
  assert.equal(await f.read(f.farm, 'pendingReward', [1n, f.bob]), 400n)
  await f.write(f.owner, f.farm, 'setPool', [0n, 0n]); await f.write(f.owner, f.farm, 'setPool', [1n, 0n]); f.mine(5n)
  assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.alice]), 200n)
  await f.conserved([f.reward, f.stake, second])
})

test('funding after an expired schedule does not pay retroactively for an unfunded gap', async () => {
  const f = await farmWorld()
  await f.write(f.owner, f.farm, 'fund', [100n]); await f.write(f.alice, f.farm, 'deposit', [0n, 100n, f.alice])
  f.mine(1000n); assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.alice]), 100n)
  await f.write(f.owner, f.farm, 'fund', [100n]); assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.alice]), 100n)
  f.mine(1n); assert.equal(await f.read(f.farm, 'pendingReward', [0n, f.alice]), 110n)
  await f.conserved([f.reward, f.stake])
})

test('delegated deposits, combined withdrawal and emergency exit preserve the actual stake liabilities', async () => {
  const f = await farmWorld()
  await f.write(f.owner, f.farm, 'fund', [1000n])
  await f.write(f.alice, f.farm, 'deposit', [0n, 100n, f.bob])
  await f.write(f.alice, f.farm, 'withdraw', [0n, 1n, f.alice], 0n, {error: 'InsufficientStake'})
  f.mine(5n)
  await f.write(f.bob, f.farm, 'withdrawAndHarvest', [0n, 40n, f.carol])
  assert.equal(await f.read(f.reward, 'balanceOf', [f.carol]), 50n)
  assert.equal(await f.read(f.stake, 'balanceOf', [f.carol]), 10_040n)
  await f.write(f.bob, f.farm, 'emergencyWithdraw', [0n, f.bob])
  assert.deepEqual(await f.read(f.farm, 'positions', [0n, f.bob]), [0n, 0n])
  assert.equal((await f.read(f.farm, 'poolInfo', [0n])).totalStaked, 0n)
  await f.write(f.alice, f.farm, 'deposit', [0n, 0n, f.alice], 0n, {error: 'ZeroAmount'})
  await f.write(f.owner, f.farm, 'fund', [0n], 0n, {error: 'ZeroAmount'})
  await f.conserved([f.reward, f.stake])
})

test('fee-on-transfer stake and funding credit only the actual received amounts', async () => {
  const f = await world(), reward = await f.deploy('ControlledToken'), stake = await f.deploy('ControlledToken')
  const farm = await f.deploy('ZVMFarm', [reward, 10n, f.owner])
  await f.write(f.owner, farm, 'addPool', [stake, 1n])
  for (const [token, who, tax] of [[reward, f.owner, 1000n], [stake, f.alice, 2500n]]) {
    await f.write(f.owner, token, 'mint', [who, 1000n]); await f.write(who, token, 'approve', [farm, MAX])
    await f.write(f.owner, token, 'configure', [tax, 0n, ZERO, '0x'])
  }
  await f.write(f.owner, farm, 'fund', [1000n]); await f.write(f.alice, farm, 'deposit', [0n, 1000n, f.alice])
  assert.equal(await f.read(farm, 'totalFunded'), 900n)
  assert.equal((await f.read(farm, 'positions', [0n, f.alice]))[0], 750n)
  assert.equal((await f.read(farm, 'poolInfo', [0n])).totalStaked, 750n)
  await f.write(f.alice, farm, 'withdraw', [0n, 750n, f.alice])
  assert.equal(await f.read(stake, 'balanceOf', [f.alice]), 563n)
  await f.conserved([reward, stake])
})

test('failed reward transfers retain pending state and emergency stake recovery needs no reward transfer', async () => {
  const f = await world(), reward = await f.deploy('ControlledToken'), stake = await f.deploy('BenchERC20', ['Stake', 'STK', 1000n])
  const farm = await f.deploy('ZVMFarm', [reward, 10n, f.owner])
  await f.write(f.owner, farm, 'addPool', [stake, 1n]); await f.write(f.owner, reward, 'mint', [f.owner, 1000n])
  for (const token of [reward, stake]) await f.write(f.owner, token, 'approve', [farm, MAX])
  await f.write(f.owner, farm, 'fund', [1000n]); await f.write(f.owner, farm, 'deposit', [0n, 100n, f.owner]); f.mine(5n)
  await f.write(f.owner, reward, 'configure', [0n, 1n, ZERO, '0x'])
  await f.write(f.owner, farm, 'harvest', [0n, f.owner], 0n, {error: 'SafeERC20FailedOperation'})
  assert.equal(await f.read(farm, 'pendingReward', [0n, f.owner]), 50n)
  await f.write(f.owner, farm, 'emergencyWithdraw', [0n, f.owner])
  assert.equal(await f.read(stake, 'balanceOf', [f.owner]), 1000n)
  await f.conserved([reward, stake])
})

test('an actual token callback reaches the farm reentrancy guard and a later false reply rolls back credit', async () => {
  const f = await world(), reward = await f.deploy('ControlledToken'), stake = await f.deploy('ControlledToken')
  const farm = await f.deploy('ZVMFarm', [reward, 10n, f.owner])
  await f.write(f.owner, farm, 'addPool', [stake, 1n]); await f.write(f.owner, stake, 'mint', [f.alice, 100n])
  await f.write(f.alice, stake, 'approve', [farm, MAX])
  await f.write(f.owner, stake, 'configure', [0n, 0n, farm, f.calldata(farm, 'fund', [1n])])
  const result = await f.write(f.alice, farm, 'deposit', [0n, 40n, f.alice]), hook = events(f, result, 'Hook', stake)[0]
  assert.equal(hook.ok, false); assert.equal(errorName(hook.result), 'ReentrancyGuardReentrantCall')
  await f.write(f.owner, stake, 'configure', [0n, 1n, ZERO, '0x'])
  await f.write(f.alice, farm, 'deposit', [0n, 10n, f.alice], 0n, {error: 'SafeERC20FailedOperation'})
  assert.equal((await f.read(farm, 'positions', [0n, f.alice]))[0], 40n)
  await f.conserved([reward, stake])
})

// Rational entitlement model uses time/weights/stake fractions, not the farm's
// accumulator or signed-debt algorithm. Conservative rounding loss is bounded
// by two base units per state-changing checkpoint for each position.
const gcd = (a, b) => {while (b) [a, b] = [b, a % b]; return a}
function plus(a, n, d) {const numerator = a[0] * d + n * a[1], denominator = a[1] * d, g = gcd(numerator, denominator); return [numerator / g, denominator / g]}
test('seeded farms obey independent rational entitlements, stake liabilities and funded reward bounds', async (t) => {
  for (const seed of [3, 17, 0x12345678, 0x9e3779b9]) await t.test('seed ' + seed, async () => {
    const f = await farmWorld(101n), next = random(seed), users = [f.alice, f.bob, f.carol], amounts = new Map(users.map((a) => [a, 0n]))
    const ideals = new Map(users.map((a) => [a, [0n, 1n]])), paid = new Map(users.map((a) => [a, 0n]))
    let rate = 101n, end = f.now, carry = 0n, funded = 0n, checkpoints = 0n
    const schedule = async (amount, newRate = rate) => {
      const budget = (end > f.now ? end - f.now : 0n) * rate + carry + amount
      if (amount) {await f.write(f.owner, f.farm, 'fund', [amount]); funded += amount}
      else await f.write(f.owner, f.farm, 'setRewardPerSecond', [newRate])
      rate = newRate; end = f.now + budget / rate; carry = budget % rate
    }
    await schedule(100_000n)
    for (let step = 0; step < 64; step++) {
      const seconds = step === 60 ? 2000n : BigInt(next() % 6), effective = (f.now + seconds < end ? f.now + seconds : end) - (f.now < end ? f.now : end)
      const total = sum([...amounts.values()])
      if (total && effective > 0n) for (const a of users) ideals.set(a, plus(ideals.get(a), effective * rate * amounts.get(a), total))
      f.mine(seconds)
      const a = users[next() % users.length], amount = 1n + BigInt(next() % 40), kind = next() % 6
      if (kind === 0 || (kind === 1 && !amounts.get(a))) {
        await f.write(a, f.farm, 'deposit', [0n, amount, a]); amounts.set(a, amounts.get(a) + amount)
      } else if (kind === 1) {
        const value = amount < amounts.get(a) ? amount : amounts.get(a)
        await f.write(a, f.farm, 'withdraw', [0n, value, a]); amounts.set(a, amounts.get(a) - value)
      } else if (kind === 2) {
        const before = await f.read(f.reward, 'balanceOf', [a]); await f.write(a, f.farm, 'harvest', [0n, a])
        paid.set(a, paid.get(a) + await f.read(f.reward, 'balanceOf', [a]) - before)
      } else if (kind === 3) await schedule(113n + amount)
      else if (kind === 4) await schedule(0n, 1n + amount)
      else await f.write(f.outsider, f.farm, 'massUpdatePools')
      checkpoints++
      let pending = 0n
      for (const who of users) {
        const pos = await f.read(f.farm, 'positions', [0n, who]), owing = await f.read(f.farm, 'pendingReward', [0n, who]), ideal = ideals.get(who)[0] / ideals.get(who)[1]
        assert.equal(pos[0], amounts.get(who)); assert.equal(await f.read(f.stake, 'balanceOf', [who]), 10_000n - amounts.get(who))
        assert.equal(await f.read(f.reward, 'balanceOf', [who]), paid.get(who))
        assert.ok(paid.get(who) + owing <= ideal, 'rounding never grants more than independent rational entitlement')
        assert.ok(ideal - paid.get(who) - owing <= checkpoints * 2n, 'checkpoint rounding cannot silently erase a material entitlement')
        pending += owing
      }
      assert.equal((await f.read(f.farm, 'poolInfo', [0n])).totalStaked, sum([...amounts.values()]))
      assert.equal(await f.read(f.stake, 'balanceOf', [f.farm]), sum([...amounts.values()]))
      assert.equal(await f.read(f.farm, 'endTime'), end); assert.equal(await f.read(f.farm, 'unscheduled'), carry)
      assert.equal(await f.read(f.farm, 'totalFunded'), funded)
      assert.equal(await f.read(f.reward, 'balanceOf', [f.farm]), funded - sum([...paid.values()]))
      assert.ok(sum([...paid.values()]) + pending <= funded)
      await f.conserved([f.reward, f.stake])
    }
  })
})

const sqrt = (n) => {if (n < 2n) return n; let a = n, b = (n + 1n) / 2n; while (b < a) {a = b; b = (a + n / a) / 2n} return a}
async function strategyWorld() {
  const f = await world(); await f.installBridge()
  await f.write(SYSTEM, '0x4200000000000000000000000000000000000100', 'depositQSR', ['0x' + '01'.repeat(32), f.owner, 10n ** 16n])
  const dex = await f.dex(); await f.write(f.owner, dex.wznn, 'deposit', [], 10n ** 20n)
  const farm = await f.deploy('ZVMFarm', [QSR, 1000n, f.owner]), strategy = await f.deploy('ZVMFarmFeeStrategy', [farm, dex.router, dex.factory, dex.wznn])
  await f.write(f.owner, dex.factory, 'setFeeTo', [strategy])
  const pair = await dex.addPair(QSR, dex.wznn, 10n ** 12n, 10n ** 18n)
  const seed = async (at = pair, tokenA = QSR, tokenB = dex.wznn, amountA = 10n ** 7n, amountB = 10n ** 13n) => {
    for (const [token, amount] of [[tokenA, amountA], [tokenB, amountB]]) await f.write(f.owner, token, 'approve', [strategy, amount])
    const first = await f.read(at, 'token0'), amounts = first.toLowerCase() === tokenA.toLowerCase() ? [amountA, amountB] : [amountB, amountA]
    return f.write(f.owner, strategy, 'seed', [at, ...amounts])
  }
  const trade = async (tokenA = dex.wznn, tokenB = QSR, amount = 10n ** 17n) => {
    await f.write(f.owner, tokenA, 'approve', [dex.router, amount])
    return f.write(f.owner, dex.router, 'swapExactTokensForTokens', [amount, 0n, [tokenA, tokenB], f.owner, f.now])
  }
  return Object.assign(f, dex, {farm, strategy, pair, seed, trade})
}

test('strategy requires feeTo and a real seed; a high output floor reverses burn, swaps and farm funding', async () => {
  const f = await strategyWorld()
  await f.write(f.owner, f.factory, 'setFeeTo', [ZERO])
  await f.write(f.bob, f.strategy, 'harvest', [[f.pair], 0n], 0n, {error: 'NotFeeTo'})
  await f.write(f.owner, f.factory, 'setFeeTo', [f.strategy])
  await f.write(f.bob, f.strategy, 'harvest', [[f.pair], 0n], 0n, {error: 'NothingToHarvest'})
  await f.seed(); await f.trade()
  await f.write(f.bob, f.strategy, 'harvest', [[f.pair], MAX], 0n, {error: 'TooLittleReward'})
  assert.equal(await f.read(f.farm, 'totalFunded'), 0n)
  assert.ok(await f.read(f.pair, 'balanceOf', [f.strategy]) > 0n)
  await f.conserved([QSR, f.wznn])
})

test('real V2 burn/protocol-fee math predicts funded reward and the surviving next-round LP seed', async () => {
  const f = await strategyWorld(); await f.seed(); await f.trade(); await f.trade(QSR, f.wznn, 10n ** 10n)
  const lp = await f.read(f.pair, 'balanceOf', [f.strategy]), supply = await f.read(f.pair, 'totalSupply'), [r0, r1] = await f.read(f.pair, 'getReserves')
  const oldK = await f.read(f.pair, 'kLast'), root = sqrt(r0 * r1), oldRoot = sqrt(oldK)
  const newLp = supply * (root - oldRoot) / (root * 5n + oldRoot)
  assert.ok(newLp > 0n)
  const amounts = [lp * r0 / (supply + newLp), lp * r1 / (supply + newLp)]
  const rewardFirst = (await f.read(f.pair, 'token0')).toLowerCase() === QSR.toLowerCase(), reward = amounts[rewardFirst ? 0 : 1], native = amounts[rewardFirst ? 1 : 0]
  const remainingReward = (rewardFirst ? r0 : r1) - reward, remainingNative = (rewardFirst ? r1 : r0) - native
  const expected = reward + native * 997n * remainingReward / (remainingNative * 1000n + native * 997n)
  const result = await f.write(f.bob, f.strategy, 'harvest', [[f.pair], expected])
  const realised = events(f, result, 'Realised', f.strategy)[0]
  assert.equal(realised.liquidityBurned, lp); assert.equal(realised.amount0, amounts[0]); assert.equal(realised.amount1, amounts[1])
  assert.equal(await f.read(f.farm, 'totalFunded'), expected)
  assert.equal(await f.read(f.farm, 'rewardPerSecond'), 1000n)
  assert.equal(await f.read(f.farm, 'endTime'), f.now + expected / 1000n)
  assert.equal(await f.read(f.farm, 'unscheduled'), expected % 1000n)
  assert.equal(await f.read(QSR, 'balanceOf', [f.farm]), expected)
  assert.equal(await f.read(f.pair, 'balanceOf', [f.strategy]), newLp)
  assert.equal(await f.read(QSR, 'balanceOf', [f.strategy]), 0n)
  assert.equal(await f.read(f.wznn, 'balanceOf', [f.strategy]), 0n)
  await f.trade(); await f.write(f.carol, f.strategy, 'harvest', [[f.pair], 0n])
  assert.ok(await f.read(f.farm, 'totalFunded') > expected)
  await f.conserved([QSR, f.wznn])
})

test('strategy sells direct and wrapped-hop assets while tokens without a route remain held', async () => {
  const f = await strategyWorld(), a = await f.deploy('BenchERC20', ['Direct asset', 'DIR', 10n ** 24n])
  const b = await f.deploy('BenchERC20', ['Hop asset', 'HOP', 10n ** 24n]), x = await f.deploy('BenchERC20', ['No route X', 'XXX', 10n ** 24n])
  const y = await f.deploy('BenchERC20', ['No route Y', 'YYY', 10n ** 24n])
  const direct = await f.addPair(a, QSR, 10n ** 18n, 10n ** 12n), hop = await f.addPair(b, f.wznn, 10n ** 18n, 10n ** 18n)
  const isolated = await f.addPair(x, y, 10n ** 18n, 10n ** 18n)
  await f.seed(direct, a, QSR, 10n ** 13n, 10n ** 7n)
  await f.seed(hop, b, f.wznn, 10n ** 13n, 10n ** 13n)
  await f.seed(isolated, x, y, 10n ** 13n, 10n ** 13n)
  const result = await f.write(f.outsider, f.strategy, 'harvest', [[direct, hop, isolated], 1n])
  const sold = events(f, result, 'Sold', f.strategy).map((e) => e.token.toLowerCase())
  assert.ok(sold.includes(a.toLowerCase()) && sold.includes(b.toLowerCase()))
  assert.equal(await f.read(a, 'balanceOf', [f.strategy]), 0n)
  assert.equal(await f.read(b, 'balanceOf', [f.strategy]), 0n)
  assert.ok(await f.read(x, 'balanceOf', [f.strategy]) > 0n)
  assert.ok(await f.read(y, 'balanceOf', [f.strategy]) > 0n)
  assert.ok(await f.read(f.farm, 'totalFunded') > 0n)
  await f.conserved([QSR, f.wznn, a, b, x, y])
})
