// CI-only bounded transport. Local qualification supplies retained read-only files.
import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {appendFileSync, mkdtempSync, readFileSync, realpathSync, writeFileSync} from 'node:fs'
import path from 'node:path'
import {COMPILER, OPENZEPPELIN} from './pins.mjs'
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'local qualification must supply retained pinned inputs')
assert.ok(process.env.RUNNER_TEMP && process.env.GITHUB_ENV)
const root = path.resolve(process.env.RUNNER_TEMP)
assert.equal(realpathSync(root), root)
const directory = mkdtempSync(path.join(root, 'zvm-contract-inputs-'))
const output = {}
for (const [key, filename, pin] of [['ZVM_TEST_COMPILER_FILE', 'soljson.js', COMPILER], ['ZVM_TEST_OPENZEPPELIN_TGZ', 'openzeppelin.tgz', OPENZEPPELIN]]) {
  const response = await fetch(pin.url, {headers: {'accept-encoding': 'identity'}, signal: AbortSignal.timeout(120_000)})
  assert.equal(response.status, 200)
  if (response.headers.has('content-length') && (!response.headers.has('content-encoding') || response.headers.get('content-encoding') === 'identity'))
    assert.equal(Number(response.headers.get('content-length')), pin.bytes)
  const chunks = []; let count = 0
  for await (const chunk of response.body) {count += chunk.length; assert.ok(count <= pin.bytes); chunks.push(chunk)}
  assert.equal(count, pin.bytes)
  const bytes = Buffer.concat(chunks), digest = createHash('sha256').update(bytes).digest('hex')
  assert.equal(digest, pin.sha256)
  const target = path.join(directory, filename)
  writeFileSync(target, bytes, {flag: 'wx', mode: 0o600})
  assert.equal(createHash('sha256').update(readFileSync(target)).digest('hex'), pin.sha256)
  appendFileSync(process.env.GITHUB_ENV, key + '=' + target + '\n')
  output[key] = {bytes: count, sha256: digest}
}
console.log(JSON.stringify({verified: output, networkRequests: 2, archivesExtractedToDisk: false}))
