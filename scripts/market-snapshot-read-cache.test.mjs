import test from 'node:test'
import assert from 'node:assert/strict'
import {snapshotReadCache} from './market-snapshot-read-cache.mjs'

const deferred = () => {
  let resolve, reject
  const promise = new Promise((yes, no) => {resolve = yes; reject = no})
  return {promise, resolve, reject}
}

test('concurrent reads share the original in-flight snapshot', async () => {
  const read = snapshotReadCache(), pending = deferred(); let calls = 0
  const one = read('hash-a', () => {calls++; return pending.promise})
  const two = read('hash-a', () => {calls++; throw new Error('Duplicate load')})
  assert.equal(one, two); pending.resolve(7n)
  assert.equal(await one, 7n); assert.equal(await two, 7n); assert.equal(calls, 1)
})
test('a successful snapshot is reused without another loader call', async () => {
  const read = snapshotReadCache(); let calls = 0
  const load = () => {calls++; return Object.freeze({feeBps: 200n, capBps: 1000n})}
  const first = await read('hash-a', load), second = await read('hash-a', load)
  assert.equal(first, second); assert.equal(calls, 1); assert.equal(second.feeBps, 200n)
})
test('different canonical hashes receive independent observations', async () => {
  const read = snapshotReadCache()
  assert.equal(await read('hash-a', () => 200n), 200n)
  assert.equal(await read('hash-b', () => 250n), 250n)
})
test('the least recently used snapshot is evicted at the declared bound', async () => {
  const read = snapshotReadCache(2); let bLoads = 0
  await read('a', () => 1); await read('b', () => {bLoads++; return 2})
  await read('a', () => {throw new Error('Lost a')}); await read('c', () => 3)
  assert.equal(await read('b', () => {bLoads++; return 4}), 4); assert.equal(bLoads, 2)
})
test('a late evicted success cannot replace a newer same-hash observation', async () => {
  const read = snapshotReadCache(1), old = deferred()
  const first = read('a', () => old.promise)
  await read('b', () => 2); assert.equal(await read('a', () => 3), 3)
  old.resolve(1); assert.equal(await first, 1)
  assert.equal(await read('a', () => {throw new Error('Lost newer value')}), 3)
})
test('a late evicted rejection cannot remove the newer same-hash observation', async () => {
  const read = snapshotReadCache(1), old = deferred()
  const first = read('a', () => old.promise), rejected = assert.rejects(first, /Old read failed/)
  await read('b', () => 2); await read('a', () => 3)
  old.reject(new Error('Old read failed')); await rejected
  assert.equal(await read('a', () => {throw new Error('Lost newer value')}), 3)
})
test('a failed RPC observation is removed and the next attempt loads again', async () => {
  const read = snapshotReadCache(); let calls = 0
  await assert.rejects(read('a', () => {calls++; return Promise.reject(new Error('RPC unavailable'))}), /RPC unavailable/)
  assert.equal(await read('a', () => {calls++; return 9n}), 9n); assert.equal(calls, 2)
})
test('synchronous loader failures follow the same retry rule', async () => {
  const read = snapshotReadCache()
  await assert.rejects(read('a', () => {throw new Error('Decode failed')}), /Decode failed/)
  assert.equal(await read('a', () => 7), 7)
})
test('zero and false results remain successful observations', async () => {
  const read = snapshotReadCache()
  for (const value of [0n, false]) {
    const key = String(value); await read(key, () => value)
    assert.equal(await read(key, () => {throw new Error('Falsy value was lost')}), value)
  }
})
test('unbounded or malformed cache settings and requests are refused', () => {
  for (const size of [0, -1, 1.5, NaN, Infinity, 5]) assert.throws(() => snapshotReadCache(size), /bound/)
  const read = snapshotReadCache()
  for (const key of ['', null, 1]) assert.throws(() => read(key, () => 1), /read/)
  assert.throws(() => read('a', null), /read/)
})
