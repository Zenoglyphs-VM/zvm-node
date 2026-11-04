// Run one exact prior-source redundant-wait failure and current pure controls.
import assert from 'node:assert/strict'
import {readFileSync, lstatSync} from 'node:fs'
import {createHash} from 'node:crypto'
import {spawnSync} from 'node:child_process'
import {fileURLToPath} from 'node:url'
const sha = value => createHash('sha256').update(value).digest('hex')
const prior = fileURLToPath(new URL('./market-navigation-before-quiet-reuse.fixture.mjs', import.meta.url))
const probe = fileURLToPath(new URL('./market-navigation-quiet-reuse.test.mjs', import.meta.url))
const current = fileURLToPath(new URL('./market-navigation-support.mjs', import.meta.url))
assert.ok(lstatSync(prior).isFile() && !lstatSync(prior).isSymbolicLink())
const before = readFileSync(prior), after = readFileSync(current)
assert.equal(sha(before), '4eb738d63b0d2f6eecf93c4beef2c329c301e48b8d5464b3b1366c4d4b9188c6')
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
assert.ok(original.stderr.includes('ERR_ASSERTION') && original.stderr.includes('an already observed complete quiet interval must not restart before navigation'))
assert.deepEqual(original.stdout.split('\n').filter(line => line.startsWith('NAVIGATION-QUIET-REUSE-REGRESSION ')),
  ['NAVIGATION-QUIET-REUSE-REGRESSION '+JSON.stringify({quietMillisecondsAlreadyObserved: 200,
    documentStartedBeforeClockAdvance: false, preQuietTimers: 1, expectedPreQuietTimers: 0})])
console.log('NAVIGATION-QUIET-REUSE-ORIGINAL-NEGATIVE '+JSON.stringify({source: '3fa79576a58563bbe0a8e9b173fe7eae87c4fa52',
  sourceSHA256: sha(before), actualExit: original.status, outputComplete: true, exactRedundantFreshWindowFailureRequired: true,
  originalRuntimeModuleRewritten: false, sameSourceRetry: false, SDKOrBrowserExecutions: 0, networkRequests: 0}))
const corrected = run([])
assert.equal(corrected.status, 0); assert.equal(corrected.stderr, '')
const marker = 'NAVIGATION-QUIET-REUSE-CONTROLS '
const records = corrected.stdout.split('\n').filter(line => line.startsWith(marker)).map(line => JSON.parse(line.slice(marker.length)))
assert.equal(records.length, 1); assert.equal(records[0].controls, 11); assert.equal(records[0].pass, true)
console.log('NAVIGATION-QUIET-REUSE-CURRENT-POSITIVE '+JSON.stringify({sourceSHA256: sha(after), actualExit: 0,
  outputComplete: true, controls: 11, originalNativeOrBrowserTestsRepeated: 0, SDKOrBrowserExecutions: 0, networkRequests: 0}))
