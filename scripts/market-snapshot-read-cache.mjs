// A small cache for immutable, hash-selected observations in one test fixture.
// Rejections are never retained and eviction never reinserts late results.
export function snapshotReadCache(limit = 2) {
  if (!Number.isInteger(limit) || limit < 1 || limit > 4) throw new Error('Invalid snapshot cache bound')
  const entries = new Map()
  return (key, load) => {
    if (typeof key !== 'string' || !key || typeof load !== 'function') throw new Error('Invalid snapshot read')
    if (entries.has(key)) {
      const pending = entries.get(key)
      entries.delete(key); entries.set(key, pending)
      return pending
    }
    const pending = Promise.resolve().then(load)
    entries.set(key, pending)
    while (entries.size > limit) entries.delete(entries.keys().next().value)
    void pending.catch(() => {if (entries.get(key) === pending) entries.delete(key)})
    return pending
  }
}
