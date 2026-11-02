// Pure deterministic DOM/state controls. No SDK, browser or real wallet.
import assert from 'node:assert/strict'
import {walletConnectionStep, connectFixtureWallet} from './market-wallet-connection-support.mjs'

const seller = '0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf'
const buyer = '0x2B5AD5c4795c026514f8317c7a215E218DcCD6cF'
const allowed = [seller,buyer], initial = () => ({connectClicked:false,walletClicked:false,ready:false})
const button = (text, action = () => {}) => ({textContent:text,disabled:false,visible:true,clicks:0,
  checkVisibility() {return this.visible}, click() {this.clicks++; action()}})
let header = [], picker = [], controls = 0
const before = globalThis.document
globalThis.document = {querySelectorAll: selector => selector === 'header button' ? header : [...header,...picker]}
const reset = () => {header=[];picker=[]}
try {
  {
    reset(); const connect=button('Connect wallet');header=[connect]
    const staleNeeded=header.some(value => value.textContent === 'Connect wallet');assert.equal(staleNeeded,true)
    header=[button('0x7E5F45\u2026395Bdf')]
    assert.equal(header.some(value => value.textContent === 'Connect wallet'),false,'the old second click cannot find its stale label')
    const value=walletConnectionStep(seller,allowed,initial())
    assert.equal(value.ready,true);assert.equal(value.connectClicked,false);assert.equal(connect.clicks,0);controls++
  }
  {
    reset();const wallet=button('Fixture wallet',() => {header=[button('0x7E5F\u20265Bdf')];picker=[]})
    const connect=button('Connect wallet',() => {picker=[wallet]});header=[connect]
    let value=walletConnectionStep(seller,allowed,initial());assert.equal(connect.clicks,1)
    value=walletConnectionStep(seller,allowed,value);assert.equal(wallet.clicks,1)
    value=walletConnectionStep(seller,allowed,value);assert.equal(value.ready,true)
    walletConnectionStep(seller,allowed,value);assert.equal(connect.clicks,1);assert.equal(wallet.clicks,1);controls++
  }
  {
    reset();header=[button('0x2B5AD5\u2026cCD6cF')]
    assert.throws(() => walletConnectionStep(seller,allowed,initial()),/connected header differs/)
    assert.equal(header[0].clicks,0);controls++
  }
  {
    reset();const connect=button('Connect wallet');connect.disabled=true;header=[connect]
    let value=walletConnectionStep(seller,allowed,initial());assert.equal(connect.clicks,0)
    connect.disabled=false;value=walletConnectionStep(seller,allowed,value)
    walletConnectionStep(seller,allowed,value);assert.equal(connect.clicks,1);controls++
  }
  {
    reset();header=[button('0x7E5F45\u2026395Bdf')];const wallet=button('Fixture wallet');picker=[wallet]
    const value=walletConnectionStep(seller,allowed,{...initial(),connectClicked:true})
    assert.equal(value.ready,true);assert.equal(value.walletClicked,false);assert.equal(wallet.clicks,0);controls++
  }
  {
    reset();const wallet=button('Fixture wallet');picker=[wallet]
    let value=walletConnectionStep(seller,allowed,initial());assert.equal(wallet.clicks,1)
    value=walletConnectionStep(seller,allowed,value);assert.equal(value.ready,false);assert.equal(wallet.clicks,1);controls++
  }
  {
    reset();const first='0x1111'+'0'.repeat(32)+'2222',second='0x1111'+'f'.repeat(32)+'2222'
    header=[button('0x1111\u20262222')]
    assert.throws(() => walletConnectionStep(first,[first,second],initial()),/connected header differs/);controls++
  }
  {
    reset();const connect=button('Connect wallet');header=[connect];let now=0,evaluations=0
    const page={async evaluate(fn,...args) {evaluations++;return fn(...args)}}
    const time={now:()=>now,pause:async ms=>{now+=ms;header=[button('0x7E5F45\u2026395Bdf')]}}
    const value=await connectFixtureWallet(page,seller,allowed,time)
    assert.equal(value.ready,true);assert.equal(value.connectClicked,true);assert.equal(value.walletClicked,false)
    assert.equal(connect.clicks,1);assert.equal(evaluations,2);assert.equal(now,100);controls++
  }
  {
    reset();let now=0,evaluations=0
    const page={async evaluate(fn,...args) {evaluations++;return fn(...args)}}
    await assert.rejects(connectFixtureWallet(page,seller,allowed,{now:()=>now,pause:async ms=>{now+=ms}}),/original connection budget/)
    assert.equal(now,12000);assert.equal(evaluations,120);controls++
  }
  {
    reset();let now=0
    const page={async evaluate() {now=12001;return {...initial(),ready:true}}}
    await assert.rejects(connectFixtureWallet(page,seller,allowed,{now:()=>now,pause:async()=>{}}),/original connection budget/)
    controls++
  }
  assert.equal(controls,10)
  console.log('WALLET-CONNECTION-CONTROLS '+JSON.stringify({controls,pass:true,SDKOrBrowserExecutions:0,
    oldSnapshotThenClickRaceReproduced:true,restoredConnectionSkipsBothActions:true,wrongOrAmbiguousIdentityRefused:true,
    oneConnectionBudgetMilliseconds:12000,maximumConnectClicksPerCall:1,maximumWalletChoicesPerCall:1,
    navigationBudgetOrGatingChanged:false,productOrProviderRepliesReplaced:0}))
} finally {if (before === undefined) delete globalThis.document;else globalThis.document=before}
