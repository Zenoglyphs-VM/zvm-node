// Run an exact prior-source ordering failure and the current pure controls once.
import assert from 'node:assert/strict'
import {readFileSync, lstatSync} from 'node:fs'
import {createHash} from 'node:crypto'
import {spawnSync} from 'node:child_process'
import {fileURLToPath} from 'node:url'
const sha = value => createHash('sha256').update(value).digest('hex')
const prior = fileURLToPath(new URL('./market-navigation-before-transition.fixture.mjs', import.meta.url))
const probe = fileURLToPath(new URL('./market-navigation-transition.test.mjs', import.meta.url))
const current = fileURLToPath(new URL('./market-navigation-support.mjs', import.meta.url))
assert.ok(lstatSync(prior).isFile() && !lstatSync(prior).isSymbolicLink())
const before = readFileSync(prior), after = readFileSync(current)
assert.equal(sha(before), '6c15cfa935348568505e1a0b6a87901ff2a9f1a711deac8db00a203daa967c34')
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
assert.ok(original.stderr.includes('ERR_ASSERTION') && original.stderr.includes('known earlier requests must finish before page.goto'))
assert.deepEqual(original.stdout.split('\n').filter(line => line.startsWith('NAVIGATION-TRANSITION-REGRESSION ')),
  ['NAVIGATION-TRANSITION-REGRESSION '+JSON.stringify({activeBeforeNavigation: 1, expectedActive: 0})])
console.log('NAVIGATION-TRANSITION-ORIGINAL-NEGATIVE '+JSON.stringify({source: 'b6bd518adfd9ae541ddd9cb7d133af8b1f750f38',
  sourceSHA256: sha(before), actualExit: original.status, outputComplete: true, exactEarlierRequestOrderingFailureRequired: true,
  originalRuntimeModuleRewritten: false, sameSourceRetry: false, SDKOrBrowserExecutions: 0, networkRequests: 0}))
const corrected = run([])
assert.equal(corrected.status, 0); assert.equal(corrected.stderr, '')
const marker = 'NAVIGATION-TRANSITION-CONTROLS '
const records = corrected.stdout.split('\n').filter(line => line.startsWith(marker)).map(line => JSON.parse(line.slice(marker.length)))
assert.equal(records.length, 1); assert.equal(records[0].controls, 9); assert.equal(records[0].pass, true)
console.log('NAVIGATION-TRANSITION-CURRENT-POSITIVE '+JSON.stringify({sourceSHA256: sha(after), actualExit: 0,
  outputComplete: true, controls: 9, originalNativeOrBrowserTestsRepeated: 0, SDKOrBrowserExecutions: 0, networkRequests: 0}))
