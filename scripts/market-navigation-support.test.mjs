// SDK/browser-free event and deadline controls; actual pages require hosted CI.
import assert from 'node:assert/strict'
import {performance} from 'node:perf_hooks'
import {EventEmitter} from 'node:events'
import {observeNavigationRequests, boundedNavigation} from './market-navigation-support.mjs'

class Page extends EventEmitter {
  signal(type, event) {this.emit('*', type, event)}
  async goto(href, options) {this.options = options; await this.loading?.(); return {href}}
}
const request = (id = 'same', path = '/rpc?private=fixture-secret') => ({id,
  url: () => 'http://127.0.0.1:1'+path, method: () => 'POST'})
const delay = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds))
let controls = 0

{
  const page = new Page(), tracker = observeNavigationRequests(page), first = request(), second = request()
  page.signal('request', first); page.signal('request', first); page.signal('request', second)
  page.signal('response', {request: () => first}); page.signal('requestfinished', first)
  assert.equal(tracker.diagnostic().activeRequests, 1)
  await assert.rejects(tracker.wait(performance.now()+20), /original navigation deadline elapsed/)
  page.signal('requestfailed', second)
  await tracker.wait(performance.now()+500)
  assert.equal(tracker.diagnostic().completed, 2); controls++
}
{
  const page = new Page(), tracker = observeNavigationRequests(page), pending = request()
  page.signal('request', pending)
  let message = ''
  try {await tracker.wait(performance.now()+20)} catch (error) {message = error.message}
  assert.match(message, /activeRequests":1/); assert.match(message, /"path":"\/rpc"/)
  assert.ok(!message.includes('fixture-secret') && !message.includes('?private=')); controls++
}
{
  const page = new Page(), tracker = observeNavigationRequests(page), pending = request()
  const began = performance.now(), waiting = tracker.wait(began+700)
  setTimeout(() => page.signal('request', pending), 40)
  setTimeout(() => page.signal('response', {request: () => pending}), 70)
  await waiting
  assert.ok(performance.now()-began >= 165); controls++
}
{
  const page = new Page(), tracker = observeNavigationRequests(page), done = request()
  page.signal('request', done); page.signal('response', {request: () => done})
  const began = performance.now()
  const noise = setInterval(() => page.signal('requestfinished', done), 10)
  try {await tracker.wait(began+500)} finally {clearInterval(noise)}
  assert.ok(performance.now()-began >= 95 && tracker.diagnostic().unmatchedCompletionSignals > 0); controls++
}
{
  const first = new Page(), second = new Page(), a = observeNavigationRequests(first), b = observeNavigationRequests(second)
  first.signal('request', request())
  await b.wait(performance.now()+500)
  assert.equal(a.diagnostic().activeRequests, 1); assert.equal(b.diagnostic().activeRequests, 0)
  const waiting = a.wait(performance.now()+500); first.signal('close')
  await assert.rejects(waiting, /page closed/); controls++
}
{
  const page = new Page(), tracker = observeNavigationRequests(page)
  for (let i=0; i<257; i++) page.signal('request', request(String(i)))
  assert.equal(tracker.diagnostic().activeRequests, 256)
  await assert.rejects(tracker.wait(performance.now()+500), /tracking limit exceeded/); controls++
}
{
  const page = new Page(), navigation = boundedNavigation('http://127.0.0.1:1')
  page.loading = () => delay(25)
  const began = performance.now(); await navigation.goto(page, 'http://127.0.0.1:1/account/test')
  assert.ok(performance.now()-began >= 120)
  assert.deepEqual(page.options, {waitUntil: 'domcontentloaded', timeout: 20000})
  const value = navigation.summary()
  assert.equal(value.attempts, 1); assert.equal(value.completed, 1); assert.equal(value.failed, 0)
  assert.equal(value.quietMilliseconds, 100); assert.equal(value.maximumInflightRequests, 0)
  await assert.rejects(navigation.goto(page, 'https://example.invalid/'), /Expected values/); controls++
}
{
  const page = new Page(), tracker = observeNavigationRequests(page)
  const began = performance.now(); await delay(25)
  await assert.rejects(tracker.wait(began+40), /original navigation deadline elapsed/)
  assert.ok(performance.now()-began < 400); controls++
}
assert.equal(controls, 8)
console.log('NAVIGATION-IDENTITY-CONTROLS '+JSON.stringify({controls, pass: true, SDKOrBrowserExecutions: 0,
  overallNavigationDeadlineMilliseconds: 20000, quietMilliseconds: 100, maximumInflightRequests: 0,
  duplicateAndRedirectIdentityControls: true, originalBudgetPreserved: true, diagnosticQueryValuesOmitted: true}))
