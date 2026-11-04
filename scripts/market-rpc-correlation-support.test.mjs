// Standard-library controls: no SDK, browser, socket or real HTTP request.
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {performance} from 'node:perf_hooks'
import {createOwnedRPCCorrelation,ownedRPCHeader,ownedRPCQueryHeader,ownedRPCLimits} from './market-rpc-correlation-support.mjs'
import {createNavigationRequestTracker} from './market-navigation-support.mjs'
const origin='http://127.0.0.1:1', token='a'.repeat(64), nonce='1'.repeat(32), second='2'.repeat(32)
const controls=[]
async function check(name,fn) {assert.ok(!controls.includes(name));await fn();controls.push(name)}
function request({url=origin+'/rpc',method='POST',headers={Authorization:'private-header'},operation=()=>undefined}={}) {
  const value={calls:[],url:()=>url,method:()=>method,headers:()=>headers,
    continue(...args){this.calls.push(args);return operation(...args)}}
  return value
}
function fixture(options={}) {return createOwnedRPCCorrelation(origin,{nonceFactory:()=>nonce,...options})}
function tagged(helper=fixture()) {const req=request();helper.continue(req);return {helper,req}}
function echo(helper,entry={},id='one',value=nonce,name=ownedRPCHeader) {
  helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:id,headers:{[name]:value}},entry)
  return entry
}
function selection(helper=fixture()) {const {req}=tagged(helper);const entry={url:origin+'/rpc',method:'POST'};echo(helper,entry);return {helper,req,entry,selected:helper.select([entry])}}
const match=(index=0,status='MATCHED')=>({index,status,returned:status!=='UNKNOWN',returnedNormally:status!=='UNKNOWN',contextCanceled:false})
const snapshot=(matches=[match()])=>({version:1,scope:'owned-loopback-rpc-request-correlation',maximumRecords:256,maximumActive:32,
  maximumQuery:8,overflow:false,matches,HTTPStatusBodyOrDeliveryObserved:false,protocolSessionIdentityEstablished:false,qualifiesNavigation:false})
function response(value=snapshot(),{status=200,type='application/json',bytes,chunks}={}) {
  const raw=bytes??new TextEncoder().encode(JSON.stringify(value))
  return {status,headers:{get:name=>name==='content-type'?type:null},body:new ReadableStream({start(controller){
    for(const part of chunks??[raw]) controller.enqueue(part)
    controller.close()
  }})}
}
const read=(helper,selected,fetchImpl,extra={})=>helper.read({base:origin,token},selected,{fetchImpl,...extra})

await check('strict-owned-loopback-configuration',()=>{
  for(const bad of ['https://127.0.0.1:1','http://localhost:1','http://127.0.0.1','http://u:p@127.0.0.1:1','http://127.0.0.1:1/path']) assert.throws(()=>createOwnedRPCCorrelation(bad))
})
await check('unowned-request-operation-and-fields-preserved',()=>{
  for(const data of [{url:origin+'/other'},{url:'data:text/plain,owned'},{url:'http://127.0.0.1:2/rpc'},{method:'GET'}]) {
    const req=request(data);req.headers=()=>{throw new Error('must-not-read')};fixture().continue(req);assert.deepEqual(req.calls,[[]])
  }
})
await check('one-owned-header-and-promise-identity',()=>{
  const promise=Promise.resolve('original-result'),req=request({operation:()=>promise}),helper=fixture()
  assert.equal(helper.continue(req),promise);assert.deepEqual(req.calls,[[{headers:{Authorization:'private-header',[ownedRPCHeader]:nonce}}]])
})
await check('original-header-object-remains-independent',()=>{
  const headers={Accept:'application/json'},req=request({headers});fixture().continue(req)
  assert.deepEqual(headers,{Accept:'application/json'});req.calls[0][0].headers.Accept='changed';assert.equal(headers.Accept,'application/json')
})
await check('owned-synchronous-throw-propagates-once',()=>{
  const error=new Error('private-error'),req=request({operation:()=>{throw error}})
  assert.throws(()=>fixture().continue(req),value=>value===error);assert.equal(req.calls.length,1)
})
await check('unowned-synchronous-throw-propagates-once',()=>{
  const error=new Error('private-error'),req=request({url:origin+'/other',operation:()=>{throw error}})
  assert.throws(()=>fixture().continue(req),value=>value===error);assert.deepEqual(req.calls,[[]])
})
await check('rejection-value-and-promise-identity-preserved',async()=>{
  const error=new Error('private-error'),promise=Promise.reject(error),req=request({operation:()=>promise})
  const result=fixture().continue(req);assert.equal(result,promise);await assert.rejects(result,value=>value===error);assert.equal(req.calls.length,1)
})
await check('reserved-header-refuses-observation-with-original-call',()=>{
  const req=request({headers:{'x-zvm-fixture-request':'private-existing'}}),helper=fixture();helper.continue(req)
  assert.deepEqual(req.calls,[[]]);assert.equal(helper.summary().taggedRequests,0)
})
await check('throwing-header-reader-does-not-leak-or-retry',()=>{
  const req=request(),helper=fixture();req.headers=()=>{throw new Error('private-secret')};helper.continue(req)
  assert.deepEqual(req.calls,[[]]);assert.ok(!JSON.stringify(helper.summary()).includes('private-secret'))
})
await check('bounded-header-view-refusals-keep-operation',()=>{
  for(const headers of [null,[],{a:1},{a:'\r\nprivate'},Object.fromEntries(Array.from({length:65},(_,i)=>['h'+i,'v'])),{a:'v'.repeat(8193)}]) {
    const req=request({headers});fixture().continue(req);assert.deepEqual(req.calls,[[]])
  }
})
await check('invalid-or-colliding-nonce-is-unavailable',()=>{
  const bad=request();fixture({nonceFactory:()=>null}).continue(bad);assert.deepEqual(bad.calls,[[]])
  const helper=fixture(),first=request(),next=request();helper.continue(first);helper.continue(next);assert.deepEqual(next.calls,[[]]);assert.equal(helper.summary().records,1)
})
await check('same-request-reuses-its-local-tag',()=>{
  let generated=0;const helper=fixture({nonceFactory:()=>{generated++;return nonce}}),req=request()
  helper.continue(req);helper.continue(req);assert.equal(generated,1);assert.equal(req.calls.length,2);assert.deepEqual(req.calls[0],req.calls[1])
})
await check('record-limit-preserves-original-operation',()=>{
  let sequence=0;const helper=fixture({nonceFactory:()=>(++sequence).toString(16).padStart(32,'0')})
  for(let i=0;i<256;i++) helper.continue(request())
  const req=request();req.headers=()=>{throw new Error('must-not-read')};helper.continue(req)
  assert.deepEqual(req.calls,[[]]);assert.equal(helper.summary().records,256);assert.equal(helper.summary().truncated,true)
})
await check('canonical-header-case-and-early-extra-info',()=>{
  const {helper}=tagged();const entry={url:origin+'/rpc',method:'POST'}
  helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'one',headers:{'X-Zvm-Fixture-Request':nonce}},undefined)
  assert.equal(helper.diagnostic(entry).status,'UNAVAILABLE');helper.bind('one',entry);assert.equal(helper.diagnostic(entry).status,'OBSERVED')
})
await check('only-the-owned-response-header-value-is-read',()=>{
  const {helper}=tagged();const headers={[ownedRPCHeader]:nonce};Object.defineProperty(headers,'Authorization',{enumerable:true,get(){throw new Error('private-secret')}})
  const entry={};helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'one',headers},entry)
  assert.equal(helper.diagnostic(entry).status,'OBSERVED');assert.ok(!JSON.stringify(helper.diagnostic(entry)).includes(nonce))
})
await check('conflicting-early-echo-is-ambiguous',()=>{
  let id=0;const helper=fixture({nonceFactory:()=>[nonce,second][id++]});helper.continue(request());helper.continue(request())
  for(const value of [nonce,second]) helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'one',headers:{[ownedRPCHeader]:value}},undefined)
  const entry={};helper.bind('one',entry);assert.equal(helper.diagnostic(entry).status,'AMBIGUOUS')
})
await check('multiple-header-spellings-and-unknown-nonce-refused',()=>{
  const {helper}=tagged();const a={},b={}
  helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'one',headers:{[ownedRPCHeader]:nonce,[ownedRPCHeader.toLowerCase()]:nonce}},a)
  echo(helper,b,'two',second);assert.equal(helper.diagnostic(a).status,'UNAVAILABLE');assert.equal(helper.diagnostic(b).status,'UNAVAILABLE')
})
await check('duplicate-selected-nonce-cannot-establish-identity',async()=>{
  const {helper}=tagged(),a={url:origin+'/rpc',method:'POST'},b={...a};echo(helper,a);echo(helper,b,'two')
  let calls=0;const result=await read(helper,helper.select([a,b]),()=>{calls++;throw new Error('must-not-fetch')})
  assert.equal(calls,0);assert.equal(result.status,'UNAVAILABLE');assert.ok(result.targets.every(row=>row.ambiguousSelection))
})
await check('selection-is-opaque-and-forgery-is-unavailable',async()=>{
  const {helper,selected}=selection();assert.ok(!JSON.stringify(selected).includes(nonce))
  assert.equal((await read(helper,{...selected},()=>{throw new Error('must-not-fetch')})).reason,'SELECTION')
})
await check('exact-lookup-is-one-bounded-read-only-request',async()=>{
  const {helper,selected}=selection();let calls=0
  const value=await read(helper,selected,(url,options)=>{calls++;assert.equal(url.pathname,'/__fixture/rpc-correlations');assert.equal(options.method,'GET');assert.equal(options.headers[ownedRPCQueryHeader],nonce);return response()})
  assert.equal(calls,1);assert.equal(value.targets[0].requestToServerCorrelationEstablished,true)
  for(const key of ['qualifiesNavigation','responseDeliveryEstablished','protocolSessionIdentityEstablished']) assert.equal(value.targets[0][key],false)
  assert.ok(!JSON.stringify(value).includes(nonce));assert.ok(!JSON.stringify(value).includes(token))
})
await check('unknown-server-record-is-not-correlation',async()=>{
  const {helper,selected}=selection();const value=await read(helper,selected,()=>response(snapshot([match(0,'UNKNOWN')])))
  assert.equal(value.targets[0].requestToServerCorrelationEstablished,false)
})
await check('ambiguous-server-record-is-not-correlation',async()=>{
  const {helper,selected}=selection();const value=await read(helper,selected,()=>response(snapshot([match(0,'AMBIGUOUS')])))
  assert.equal(value.targets[0].requestToServerCorrelationEstablished,false)
})
await check('lookup-configuration-cannot-leave-owned-origin',async()=>{
  const {helper,selected}=selection();let calls=0;const fetchImpl=()=>{calls++;throw new Error('must-not-fetch')}
  for(const config of [{base:'http://127.0.0.1:2',token},{base:origin,token:'private-invalid'}]) assert.equal((await helper.read(config,selected,{fetchImpl})).reason,'CONFIGURATION')
  assert.equal(calls,0)
})
await check('HTTP-status-and-content-type-refusals',async()=>{
  for(const options of [{status:403},{type:'text/plain'}]) {const {helper,selected}=selection();assert.equal((await read(helper,selected,()=>response(snapshot(),options))).reason,'HTTP_RESPONSE')}
})
await check('schema-refusals-omit-untrusted-values',async()=>{
  const bad=[{...snapshot(),nonce:'private-secret'},snapshot([{...match(),status:'private-secret'}]),snapshot([{...match(),index:1}]),{...snapshot(),qualifiesNavigation:true},snapshot([{...match(),returned:false}])]
  for(const value of bad){const {helper,selected}=selection();const result=await read(helper,selected,()=>response(value));assert.equal(result.reason,'TRANSPORT_OR_SCHEMA');assert.ok(!JSON.stringify(result).includes('private-secret'))}
})
await check('body-and-chunk-count-bounds',async()=>{
  for(const options of [{bytes:new Uint8Array(ownedRPCLimits.body+1)},{chunks:Array.from({length:65},()=>new Uint8Array())}]) {
    const {helper,selected}=selection();assert.equal((await read(helper,selected,()=>response(snapshot(),options))).reason,'TRANSPORT_OR_SCHEMA')
  }
})
await check('diagnostic-timeout-aborts-without-completion',async()=>{
  const {helper,selected}=selection();let signal
  const value=await read(helper,selected,(_,options)=>{signal=options.signal;return new Promise(()=>{})},{timeoutMilliseconds:10})
  assert.equal(value.reason,'DEADLINE');assert.equal(signal.aborted,true);assert.equal(value.qualifiesNavigation,false)
})
await check('selection-and-result-mutations-cannot-change-private-state',async()=>{
  const {helper,selected}=selection();selected.targets[0].index=7
  const first=await read(helper,selected,()=>response());assert.equal(first.targets[0].index,0);first.targets[0].index=9
  const next=await read(helper,selected,()=>response());assert.equal(next.targets[0].index,0)
  assert.equal(helper.select([]).selectionScope,'RECENT_HEADER_ECHO_SAMPLE')
})
await check('correlation-does-not-complete-navigation',async()=>{
  class Session extends EventEmitter {detached=false;signal(type,event){this.emit('*',type,event)}}
  const session=new Session(),helper=fixture(),tracker=createNavigationRequestTracker(session,undefined,helper)
  helper.continue(request());session.signal('Network.responseReceivedExtraInfo',{requestId:'one',headers:{'X-Zvm-Fixture-Request':nonce}})
  session.signal('Network.requestWillBeSent',{requestId:'one',timestamp:1,request:{url:origin+'/rpc',method:'POST'}})
  assert.equal(tracker.diagnostic().requests[0].ownedRPCCorrelation.status,'OBSERVED')
  await assert.rejects(tracker.wait(performance.now()+10),/original navigation deadline elapsed/)
  assert.equal(tracker.diagnostic().activeRequests,1)
  session.signal('Network.loadingFinished',{requestId:'one',timestamp:2});await tracker.wait(performance.now()+500,1);tracker.close()
})
await check('early-observation-bound-is-honest',()=>{
  const {helper}=tagged();for(let i=0;i<257;i++) helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'id'+i,headers:{[ownedRPCHeader]:nonce}},undefined)
  assert.equal(helper.summary().early,256);assert.equal(helper.summary().truncated,true)
})
await check('throwing-hook-does-not-change-original-navigation',async()=>{
  class Session extends EventEmitter {detached=false;signal(type,event){this.emit('*',type,event)}}
  const session=new Session(),bad={observeSignal(){throw new Error('private-secret')},bind(){throw new Error('private-secret')},diagnostic(){throw new Error('private-secret')}}
  const tracker=createNavigationRequestTracker(session,undefined,bad)
  session.signal('Network.requestWillBeSent',{requestId:'one',timestamp:1,request:{url:origin+'/rpc',method:'POST'}})
  session.signal('Network.loadingFinished',{requestId:'one',timestamp:2});await tracker.wait(performance.now()+500,1)
  assert.equal(tracker.diagnostic().completed,1);tracker.close()
})

await check('late-extra-info-retains-the-original-entry',()=>{
  const {helper}=tagged(),session={},entry={url:origin+'/rpc',method:'POST'}
  helper.bind('one',entry,session)
  helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'one',headers:{[ownedRPCHeader]:nonce}},undefined,session)
  assert.equal(helper.diagnostic(entry).status,'OBSERVED')
})
await check('primary-session-identities-never-cross',()=>{
  let number=0;const helper=fixture({nonceFactory:()=>[nonce,second][number++]})
  helper.continue(request());helper.continue(request())
  const a={},b={},first={url:origin+'/rpc',method:'POST'},next={...first}
  helper.bind('one',first,a);helper.bind('one',next,b)
  helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'one',headers:{[ownedRPCHeader]:nonce}},undefined,a)
  assert.equal(helper.diagnostic(first).status,'OBSERVED');assert.equal(helper.diagnostic(next).status,'UNAVAILABLE')
  helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'one',headers:{[ownedRPCHeader]:second}},undefined,b)
  assert.equal(helper.diagnostic(next).status,'OBSERVED')
})
await check('primary-session-bound-is-explicit',()=>{
  const helper=fixture()
  for(let i=0;i<5;i++) helper.bind('one',{}, {})
  assert.equal(helper.summary().scopes,4);assert.equal(helper.summary().truncated,true)
})
await check('reused-primary-identity-refuses-both-entries',()=>{
  const {helper}=tagged(),session={},first={},next={}
  helper.bind('one',first,session)
  helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'one',headers:{[ownedRPCHeader]:nonce}},undefined,session)
  helper.bind('one',next,session)
  helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'one',headers:{[ownedRPCHeader]:nonce}},next,session)
  assert.equal(helper.diagnostic(first).status,'AMBIGUOUS');assert.equal(helper.diagnostic(next).status,'AMBIGUOUS')
})
await check('bounded-history-never-forgets-identity-reuse',()=>{
  const helper=fixture(),session={},entries=[]
  for(let i=0;i<=ownedRPCLimits.bindings;i++) {const entry={};entries.push(entry);helper.bind('id'+i,entry,session)}
  assert.equal(helper.summary().bindings,ownedRPCLimits.bindings);assert.equal(helper.summary().truncated,true)
  helper.bind('id0',{},session);assert.equal(helper.diagnostic(entries[0]).status,'AMBIGUOUS')
})

await check('expanded-primary-binding-window-retains-echo-and-reuse-refusal',()=>{
  const {helper}=tagged(),session={},entries=[]
  for(let i=0;i<ownedRPCLimits.bindings;i++) {const entry={};entries.push(entry);helper.bind('expanded'+i,entry,session)}
  assert.equal(ownedRPCLimits.bindings,512);assert.equal(helper.summary().truncated,false)
  helper.observeSignal('Network.responseReceivedExtraInfo',{requestId:'expanded256',headers:{[ownedRPCHeader]:nonce}},undefined,session)
  assert.equal(helper.diagnostic(entries[256]).status,'OBSERVED')
  helper.bind('expanded512',{},session)
  assert.deepEqual(helper.summary().limitHits,{records:0,primaryScopes:0,primaryBindings:1,earlyHeaderEchoes:0})
  assert.equal(helper.diagnostic(entries[256]).status,'OBSERVED')
  helper.bind('expanded0',{},session);assert.equal(helper.diagnostic(entries[0]).status,'AMBIGUOUS')
  assert.deepEqual(helper.summary().primaryScopeUsage,[{index:0,bindings:512,early:0}])
})
await check('limit-reasons-and-scope-usage-have-independent-private-copies',()=>{
  const empty={records:0,primaryScopes:0,primaryBindings:0,earlyHeaderEchoes:0}
  let sequence=0;const records=fixture({nonceFactory:()=>(++sequence).toString(16).padStart(32,'0')})
  for(let i=0;i<=ownedRPCLimits.records;i++) records.continue(request())
  assert.deepEqual(records.summary().limitHits,{...empty,records:1})
  const scopes=fixture();for(let i=0;i<=ownedRPCLimits.scopes;i++) scopes.bind('one',{}, {})
  assert.deepEqual(scopes.summary().limitHits,{...empty,primaryScopes:1})
  const bindings=fixture(),session={}
  for(let i=0;i<=ownedRPCLimits.bindings;i++) bindings.bind('id'+i,{},session)
  assert.deepEqual(bindings.summary().limitHits,{...empty,primaryBindings:1})
  const {helper:early}=tagged()
  for(let i=0;i<=ownedRPCLimits.early;i++) early.observeSignal('Network.responseReceivedExtraInfo',
    {requestId:'early'+i,headers:{[ownedRPCHeader]:nonce}},undefined)
  assert.deepEqual(early.summary().limitHits,{...empty,earlyHeaderEchoes:1})
  const external=early.summary();external.limitHits.earlyHeaderEchoes=0;external.primaryScopeUsage[0].early=0
  assert.deepEqual(early.summary().limitHits,{...empty,earlyHeaderEchoes:1})
  assert.deepEqual(early.summary().primaryScopeUsage,[{index:0,bindings:0,early:256}])
  for(const helper of [records,scopes,bindings,early]) assert.equal(helper.summary().truncated,true)
  assert.ok(!JSON.stringify(early.summary()).includes(nonce))
})

assert.equal(controls.length,38)
console.log('OWNED-RPC-CORRELATION-FAKE-CONTROLS '+JSON.stringify({controls:controls.length,controlNames:controls,pass:true,
  SDKOrBrowserExecutions:0,realNetworkRequests:0,originalHTTPArgumentsPreservedExceptOwnedNonceHeader:true,
  synchronousThrowAndRejectionPropagatedOnce:true,nonceOrRequestValuesReported:false,completionGateChanged:false,
  maximumPrimaryBindingsPerScope:512,preciseLimitHitCounts:true,scopeUsageContainsOpaqueValues:false,
  lookupMaximumMilliseconds:750,lookupBodyLimit:16384,maximumRecords:256,maximumLookupTargets:8,
  fixtureCorrelationIsAuthentication:false,responseDeliveryEstablished:false,protocolSessionIdentityEstablished:false}))
