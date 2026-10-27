// CI-only bounded acquisition of already public, immutable interface inputs.
// Local qualification must supply retained verified inputs through its manager.
import assert from 'node:assert/strict'
import {appendFileSync, mkdirSync, mkdtempSync, realpathSync, writeFileSync} from 'node:fs'
import path from 'node:path'
import {entries, manifest, pinnedFile, sha256} from './market-inputs.mjs'

assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.ok(process.env.RUNNER_TEMP && process.env.GITHUB_ENV)
const temporary = path.resolve(process.env.RUNNER_TEMP)
assert.equal(realpathSync(temporary), temporary)
const root = mkdtempSync(path.join(temporary, 'zvm-market-inputs-'))
const summary = []
let total = 0
for (const entry of entries()) {
  const url = 'https://raw.githubusercontent.com/' + entry.repository + '/' + entry.revision + '/' + entry.path
  const response = await fetch(url, {headers: {'accept-encoding': 'identity'}, signal: AbortSignal.timeout(30_000)})
  assert.equal(response.status, 200)
  if (response.headers.has('content-length') && (!response.headers.has('content-encoding') || response.headers.get('content-encoding') === 'identity'))
    assert.equal(Number(response.headers.get('content-length')), entry.bytes)
  const chunks = []
  let count = 0
  for await (const chunk of response.body) {
    count += chunk.length
    assert.ok(count <= entry.bytes && total + count <= manifest.maxTotalBytes)
    chunks.push(chunk)
  }
  const bytes = Buffer.concat(chunks)
  assert.equal(count, entry.bytes)
  assert.equal(sha256(bytes), entry.sha256)
  const target = path.join(root, entry.name, entry.path)
  mkdirSync(path.dirname(target), {recursive: true, mode: 0o700})
  writeFileSync(target, bytes, {flag: 'wx', mode: 0o600})
  pinnedFile(root, entry)
  total += count
  summary.push({name: entry.name, path: entry.path, bytes: count, sha256: entry.sha256})
}
appendFileSync(process.env.GITHUB_ENV, 'ZVM_MARKET_INPUT_ROOT=' + root + '\n')
console.log(JSON.stringify({verified: summary, sourceBytes: total, maxSourceBytes: manifest.maxTotalBytes,
  sourceRequests: summary.length, fullFrontendCheckoutsOrBuilds: 0}))
