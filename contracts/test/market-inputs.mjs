import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {lstatSync, readFileSync, realpathSync} from 'node:fs'
import {createRequire} from 'node:module'
import path from 'node:path'
import {fileURLToPath} from 'node:url'

export const manifest = JSON.parse(readFileSync(new URL('./market-inputs.lock.json', import.meta.url)))
assert.equal(manifest.schema, 1)
assert.deepEqual(Object.keys(manifest.frontends).sort(), ['zvm-karum', 'zvm-nft'])
export const sha256 = (bytes) => createHash('sha256').update(bytes).digest('hex')
const canonical = (value) => JSON.stringify((function sort(v) {
  if (Array.isArray(v)) return v.map(sort)
  if (v && typeof v === 'object') return Object.fromEntries(Object.keys(v).sort().map((k) => [k, sort(v[k])]))
  return v
})(value))

export function entries() {
  return [...Object.entries(manifest.frontends), ['zvm-core', manifest.core]].flatMap(([name, row]) => {
    const repository = row.sourceRepository || row.repository
    assert.match(repository, /^(?:edgepillar|Zenoglyphs-VM)\/zvm-(?:nft|karum|core)$/)
    assert.match(row.revision, /^[a-f0-9]{40}$/)
    return Object.values(row.files).map((file) => {
      assert.ok(['src/core/orders.ts', 'package-lock.json', 'vectors/vectors.json'].includes(file.path))
      assert.ok(Number.isSafeInteger(file.bytes) && file.bytes > 0 && file.bytes <= manifest.maxFileBytes)
      assert.match(file.sha256, /^[a-f0-9]{64}$/)
      return {name, repository, revision: row.revision, ...file}
    })
  })
}

export function pinnedFile(root, entry) {
  assert.equal(realpathSync(root), root)
  const target = path.join(root, entry.name, entry.path)
  assert.equal(realpathSync(target), target)
  assert.ok(lstatSync(target).isFile() && !lstatSync(target).isSymbolicLink())
  const bytes = readFileSync(target)
  assert.equal(bytes.length, entry.bytes)
  assert.equal(sha256(bytes), entry.sha256)
  return bytes
}

export function runtimeGraph(packages) {
  function resolve(parent, name) {
    for (;;) {
      const key = (parent ? parent + '/' : '') + 'node_modules/' + name
      if (packages[key]) return key
      if (!parent) return null
      parent = path.posix.dirname(parent)
      if (parent === '.') parent = ''
      if (parent.endsWith('node_modules')) parent = path.posix.dirname(parent)
      if (parent === '.') parent = ''
    }
  }
  const dependencies = (row) => [...new Set([...Object.keys(row.dependencies || {}),
    ...Object.keys(row.optionalDependencies || {}), ...Object.keys(row.peerDependencies || {})])]
  const todo = ['node_modules/viem'], seen = new Set(), semantic = {}
  const keys = ['version', 'resolved', 'integrity', 'dependencies', 'optionalDependencies', 'peerDependencies',
    'peerDependenciesMeta', 'bin', 'engines', 'os', 'cpu', 'hasInstallScript']
  const identity = (key) => key.split('node_modules/').at(-1) + '@' + packages[key].version
  while (todo.length) {
    const key = todo.pop()
    if (seen.has(key)) continue
    seen.add(key)
    const row = packages[key]
    for (const name of dependencies(row)) {
      const target = resolve(key, name)
      assert.ok(target || name in (row.optionalDependencies || {}) || row.peerDependenciesMeta?.[name]?.optional)
      if (target) todo.push(target)
    }
  }
  for (const key of seen) {
    const row = packages[key], value = Object.fromEntries(keys.filter((k) => k in row).map((k) => [k, row[k]]))
    value.resolvedDependencies = Object.fromEntries(dependencies(row).map((name) => [name, resolve(key, name)]).filter(([, v]) => v).map(([name, target]) => [name, identity(target)]))
    const name = identity(key)
    if (semantic[name]) assert.equal(canonical(semantic[name]), canonical(value))
    semantic[name] = value
  }
  return {keys: [...seen].sort(), digest: sha256(canonical(semantic))}
}

export function loadMarketInputs() {
  const root = process.env.ZVM_MARKET_INPUT_ROOT
  assert.ok(root && path.isAbsolute(root), 'provide the exact verified public market input directory')
  const files = new Map()
  for (const entry of entries()) files.set(entry.name + '/' + entry.path, pinnedFile(root, entry))
  const frontends = {}
  for (const [name, row] of Object.entries(manifest.frontends)) {
    const original = JSON.parse(files.get(name + '/package-lock.json'))
    assert.equal(runtimeGraph(original.packages).digest, row.sdkGraphSha256)
    const directory = fileURLToPath(new URL('./' + row.runtimeDirectory + '/', import.meta.url))
    const runtime = JSON.parse(readFileSync(path.join(directory, 'package-lock.json')))
    const graph = runtimeGraph(runtime.packages)
    assert.equal(graph.digest, row.sdkGraphSha256)
    for (const key of graph.keys) {
      const installed = JSON.parse(readFileSync(path.join(directory, key, 'package.json')))
      assert.equal(installed.version, runtime.packages[key].version, name + ': installed dependency differs from its source lock')
    }
    const require = createRequire(path.join(directory, 'package.json'))
    assert.equal(JSON.parse(readFileSync(path.join(directory, 'node_modules/viem/package.json'))).version, manifest.runtimeVersion)
    frontends[name] = {source: files.get(name + '/src/core/orders.ts').toString('utf8'),
      sdk: require('viem'), account: require('viem/accounts').privateKeyToAccount,
      sdkGraphSha256: graph.digest, sourceSha256: row.files['src/core/orders.ts'].sha256}
  }
  return {frontends, vectors: JSON.parse(files.get('zvm-core/vectors/vectors.json')).exchange}
}
