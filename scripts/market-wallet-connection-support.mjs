// Test-fixture UI coordination only. Product/provider replies are unchanged.
import assert from 'node:assert/strict'
import {performance} from 'node:perf_hooks'

// This function is serialized into the page. Read and act in one synchronous
// evaluation; a prior disconnected snapshot cannot authorize a later click.
export function walletConnectionStep(expected, allowed, previous) {
  const selected = String(expected).toLowerCase()
  const identities = [...new Set(allowed.map(value => String(value).toLowerCase()))]
  if (!/^0x[0-9a-f]{40}$/.test(selected) || !identities.includes(selected) ||
      identities.some(value => !/^0x[0-9a-f]{40}$/.test(value))) throw new Error('invalid public fixture identity')
  const state = {connectClicked: previous.connectClicked === true, walletClicked: previous.walletClicked === true, ready: false}
  const visible = button => button.checkVisibility()
  const headers = [...document.querySelectorAll('header button')].filter(visible)
  const labels = headers.map(button => button.textContent.trim().toLowerCase())
  const accounts = labels.filter(label => /^0x[0-9a-f]{40}$/.test(label) || /^0x[0-9a-f]{4,10}\u2026[0-9a-f]{4,10}$/.test(label))
  if (accounts.length) {
    for (const label of accounts) {
      const matches = identities.filter(address => {
        if (!label.includes('\u2026')) return address === label
        const [prefix, suffix] = label.split('\u2026')
        return prefix.length+suffix.length < address.length && address.startsWith(prefix) && address.endsWith(suffix)
      })
      if (matches.length !== 1 || matches[0] !== selected) throw new Error('connected header differs from the selected public fixture account')
    }
    return {...state, ready: true}
  }
  // A picker may already be open. Each action is issued at most once, and a
  // restored account above always wins over a disappearing picker/button.
  const picker = [...document.querySelectorAll('button')].find(button => visible(button) && !button.disabled && button.textContent.trim() === 'Fixture wallet')
  if (picker && !state.walletClicked) {picker.click(); return {...state, walletClicked: true}}
  const connect = headers.find(button => !button.disabled && button.textContent.trim() === 'Connect wallet')
  if (connect && !state.connectClicked && !state.walletClicked) {connect.click(); return {...state, connectClicked: true}}
  return state
}

const realTime = {now: () => performance.now(), pause: ms => new Promise(resolve => setTimeout(resolve, ms))}

// One original twelve-second UI budget, with no reload, retry of an action,
// provider mutation, extra account prompt or change to navigation completion.
export async function connectFixtureWallet(page, expected, allowed, time = realTime) {
  const deadline = time.now()+12000
  let state = {connectClicked: false, walletClicked: false, ready: false}
  while (time.now() < deadline) {
    state = await page.evaluate(walletConnectionStep, expected, allowed, state)
    assert.ok(state && ['connectClicked','walletClicked','ready'].every(key => typeof state[key] === 'boolean'))
    if (time.now() > deadline) break
    if (state.ready) return state
    const remaining = deadline-time.now()
    if (remaining <= 0) break
    await time.pause(Math.min(100,remaining))
  }
  throw new Error('actual public fixture wallet did not become ready within the original connection budget')
}
