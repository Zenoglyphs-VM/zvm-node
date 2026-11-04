// Run a source-pinned original failure and the current positive vector once each.
import assert from 'node:assert/strict'
import {readFileSync, lstatSync} from 'node:fs'
import {createHash} from 'node:crypto'
import {spawnSync} from 'node:child_process'
import {fileURLToPath} from 'node:url'
const sha = value => createHash('sha256').update(value).digest('hex')
const prior = fileURLToPath(new URL('./market-navigation-before-terminal-load.fixture.mjs', import.meta.url))
const probe = fileURLToPath(new URL('./market-navigation-terminal-load.test.mjs', import.meta.url))
const current = fileURLToPath(new URL('./market-navigation-support.mjs', import.meta.url))
assert.ok(lstatSync(prior).isFile() && !lstatSync(prior).isSymbolicLink())
const before = readFileSync(prior), after = readFileSync(current)
assert.equal(sha(before), '6030094fef3e7ff9774a03255d301a53effef47e4ab2147178263906fbd971cc')
assert.ok(before.length < 32768 && after.length < 32768)
const run = args => {
  const result = spawnSync(process.execPath, [probe, ...args], {encoding: 'utf8', timeout: 5000, maxBuffer: 32768})
  assert.equal(result.error, undefined); assert.equal(result.signal, null)
  assert.ok(Buffer.byteLength(result.stdout) <= 32768 && Buffer.byteLength(result.stderr) <= 32768)
  process.stdout.write(result.stdout); process.stderr.write(result.stderr)
  return result
}
const original = run([prior])
assert.equal(original.status, 1)
assert.ok(original.stderr.includes('ERR_ASSERTION') && original.stderr.includes('response headers must not complete a tracked load'))
assert.deepEqual(original.stdout.split('\n').filter(line => line.startsWith('NAVIGATION-HEADER-TERMINAL-REGRESSION ')),
  ['NAVIGATION-HEADER-TERMINAL-REGRESSION '+JSON.stringify({activeAfterHeaders: 0, expectedActive: 1})])
console.log('NAVIGATION-TERMINAL-ORIGINAL-NEGATIVE '+JSON.stringify({source: 'fcbab39dbfcb32a5521e3469823fa7757da30a5c',
  sourceSHA256: sha(before), actualExit: original.status, outputComplete: true, exactHeaderOnlyFailureRequired: true,
  originalRuntimeModuleRewritten: false, sameSourceRetry: false, SDKOrBrowserExecutions: 0, networkRequests: 0}))
const corrected = run([])
assert.equal(corrected.status, 0); assert.equal(corrected.stderr, '')
const marker = 'NAVIGATION-TERMINAL-LOAD-CONTROLS '
const records = corrected.stdout.split('\n').filter(line => line.startsWith(marker)).map(line => JSON.parse(line.slice(marker.length)))
assert.equal(records.length, 1); assert.equal(records[0].controls, 8); assert.equal(records[0].pass, true)
console.log('NAVIGATION-TERMINAL-CURRENT-POSITIVE '+JSON.stringify({sourceSHA256: sha(after), actualExit: 0,
  outputComplete: true, controls: 8, originalNativeOrBrowserTestsRepeated: 0, SDKOrBrowserExecutions: 0, networkRequests: 0}))
