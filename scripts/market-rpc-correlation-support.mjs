import {captureRPCRequestContext,observeRPCRequestContext,bindRPCRequestContext,describeRPCRequestContext,
  summarizeRPCRequestContext,unavailableRPCRequestContext} from './market-rpc-request-context.mjs'
// Owned-fixture HTTP correlation only. Nonces and existing header values never
// enter diagnostics. A matching echo/handler record does not prove delivery.
import {randomBytes} from 'node:crypto'
import {readOwnedHTTP1Connections,unavailableConnectionObservation} from './market-rpc-connection-support.mjs'
export const ownedRPCHeader = 'X-ZVM-Fixture-Request'
export const ownedRPCQueryHeader = 'X-ZVM-Fixture-Requests'
export const ownedRPCLimits = Object.freeze({records: 256, early: 256, scopes: 4, bindings: 512, query: 8, body: 16384, chunks: 64, timeout: 750})
const scope = 'owned-loopback-rpc-request-correlation'
const nonceValid = value => typeof value === 'string' && /^[a-f0-9]{32}$/.test(value)
const idValid = value => typeof value === 'string' && value.length > 0 && value.length <= 512 && !/[^\x21-\x7e]/.test(value)
const exact = (value, keys) => value && typeof value === 'object' && !Array.isArray(value) &&
  Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key))
const baseFacts = {scope, qualifiesNavigation: false, responseDeliveryEstablished: false,
  protocolSessionIdentityEstablished: false, nonceOrRequestValuesReported: false,
  completionGateChanged: false, additionalProtocolCommands: 0, fixtureCorrelationIsAuthentication: false}
const unavailable = reason => ({...baseFacts, status: 'UNAVAILABLE', reason})

function loopback(value) {
  if (typeof value !== 'string' || value.length > 2048) throw new Error('invalid owned fixture origin')
  const url = new URL(value)
  if (url.protocol !== 'http:' || url.hostname !== '127.0.0.1' || !url.port || url.username || url.password ||
    url.pathname !== '/' || url.search || url.hash) throw new Error('invalid owned fixture origin')
  return url
}
function echoHeader(headers) {
  if (!headers || typeof headers !== 'object' || Array.isArray(headers)) return null
  let selected, count = 0
  for (const name in headers) {
    if (!Object.hasOwn(headers,name)) continue
    if (++count > 128) return null
    if (name.toLowerCase() === ownedRPCHeader.toLowerCase()) {
      if (selected !== undefined) return null
      selected = name
    }
  }
  const value = selected === undefined ? null : headers[selected]
  return nonceValid(value) ? value : null
}
function validateSnapshot(value, count) {
  const keys = ['version','scope','maximumRecords','maximumActive','maximumQuery','overflow','matches',
    'HTTPStatusBodyOrDeliveryObserved','protocolSessionIdentityEstablished','qualifiesNavigation']
  if (!exact(value,keys) || value.version !== 1 || value.scope !== scope || value.maximumRecords !== 256 ||
    value.maximumActive !== 32 || value.maximumQuery !== 8 || typeof value.overflow !== 'boolean' ||
    value.HTTPStatusBodyOrDeliveryObserved !== false || value.protocolSessionIdentityEstablished !== false ||
    value.qualifiesNavigation !== false || !Array.isArray(value.matches) || value.matches.length !== count) throw new Error('invalid fixture correlation snapshot')
  const matchKeys = ['index','status','returned','returnedNormally','contextCanceled']
  for (const [index,row] of value.matches.entries()) {
    if (!exact(row,matchKeys) || row.index !== index || !['MATCHED','UNKNOWN','AMBIGUOUS'].includes(row.status) ||
      typeof row.returned !== 'boolean' || typeof row.returnedNormally !== 'boolean' || typeof row.contextCanceled !== 'boolean' ||
      (row.returnedNormally && !row.returned) || (row.status === 'UNKNOWN' && (row.returned || row.returnedNormally || row.contextCanceled))) throw new Error('invalid fixture correlation match')
  }
  return value
}
async function snapshotBody(response, count) {
  const reader = response.body?.getReader()
  if (!reader) throw new Error('missing fixture reader')
  let size = 0, reads = 0
  const chunks = []
  try {
    for (;;) {
      const {done,value} = await reader.read()
      if (done) break
      if (!(value instanceof Uint8Array) || ++reads > ownedRPCLimits.chunks || size + value.byteLength > ownedRPCLimits.body) {
        void reader.cancel().catch(() => {}); throw new Error('bounded fixture body required')
      }
      size += value.byteLength; chunks.push(value)
    }
  } finally {reader.releaseLock()}
  const raw = new Uint8Array(size)
  let offset = 0
  for (const chunk of chunks) {raw.set(chunk,offset); offset += chunk.byteLength}
  return validateSnapshot(JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(raw)),count)
}

export function createOwnedRPCCorrelation(origin, {nonceFactory = () => randomBytes(16).toString('hex'), requestPurpose = null, requestContext = null} = {}) {
  const base = loopback(origin)
  if (typeof nonceFactory !== 'function') throw new Error('invalid owned nonce factory')
  const records = new Map(), requestTags = new WeakMap(), entryEcho = new WeakMap(), selections = new WeakMap()
  const defaultScope = {}, scopeStates = new WeakMap(), retainedScopes = []
  let tagged = 0, fallbacks = 0, readErrors = 0, truncated = false, primaryEchoes = 0
  const limitHits = {records:0,primaryScopes:0,primaryBindings:0,earlyHeaderEchoes:0}
  let purposeReadErrors = 0
  const unavailablePurpose = reason => ({status:'UNAVAILABLE',phase:null,reason})
  const purposeSnapshot = request => {
    if (requestPurpose === null) return unavailablePurpose('OBSERVER_NOT_CONFIGURED')
    try {
      if (typeof requestPurpose !== 'function') throw new Error('purpose observer unavailable')
      const value = requestPurpose(request)
      if (!exact(value,['status','phase'])) throw new Error('purpose observation shape')
      const status = value.status, phase = value.phase
      if (!['MARKED','NOT_MARKED','AMBIGUOUS','UNAVAILABLE'].includes(status) ||
          (status === 'MARKED' ? !['ACCOUNT_CHANGE_REFUSAL','FORM_EDIT_SNAPSHOT'].includes(phase) : phase !== null))
        throw new Error('purpose observation value')
      return Object.freeze({status,phase,reason:status === 'UNAVAILABLE' ? 'OBSERVER_UNAVAILABLE' : null})
    } catch {
      purposeReadErrors = Math.min(Number.MAX_SAFE_INTEGER,purposeReadErrors+1)
      return unavailablePurpose('OBSERVER_REFUSED')
    }
  }
  const limit = kind => {truncated=true;limitHits[kind]=Math.min(Number.MAX_SAFE_INTEGER,limitHits[kind]+1)}
  const scopeState = session => {
    const key = session === undefined ? defaultScope : session
    if (!key || typeof key !== 'object') {readErrors++; return null}
    if (scopeStates.has(key)) return scopeStates.get(key)
    if (retainedScopes.length === ownedRPCLimits.scopes) {limit('primaryScopes'); return null}
    const value = {early:new Map(),bindings:new Map()}
    scopeStates.set(key,value);retainedScopes.push(value);return value
  }
  const register = (state,id,entry) => {
    if (!entry || typeof entry !== 'object' || !idValid(id)) return false
    const previous = state.bindings.get(id)
    if (previous) {
      if (previous.entry !== entry || previous.reused) {
        previous.reused=true;entryEcho.set(previous.entry,null);entryEcho.set(entry,null)
        state.early.delete(id);return false
      }
      return true
    }
    if (state.bindings.size === ownedRPCLimits.bindings) {limit('primaryBindings'); return false}
    state.bindings.set(id,{entry,reused:false});return true
  }
  const rpc = (url,method) => {
    try {const value = new URL(url); return value.origin === base.origin && value.pathname === '/rpc' && method === 'POST'} catch {return false}
  }
  const bindEcho = (entry,nonce) => {
    if (!entry || typeof entry !== 'object') return
    if (entryEcho.has(entry) && entryEcho.get(entry) !== nonce) {entryEcho.set(entry,null); return}
    if (!entryEcho.has(entry)) {primaryEchoes++; records.get(nonce).echoed = true}
    entryEcho.set(entry,nonce)
  }
  const descriptor = entry => {
    const nonce = entryEcho.get(entry), row = nonce && records.get(nonce)
    return {...baseFacts, status: entryEcho.has(entry) && nonce === null ? 'AMBIGUOUS' : row ? 'OBSERVED' : 'UNAVAILABLE',
      reason: row ? null : 'NO_UNIQUE_OWNED_HEADER_ECHO', requestObjectTaggedLocally: !!row,
      primaryResponseExtraInfoEchoObserved: !!row, requestToServerCorrelationEstablished: false}
  }
  function select(entries) {
    const pending = entries.filter(entry => rpc(entry.url,entry.method))
    let targets = pending.slice(0,ownedRPCLimits.query).map(entry => ({entry,nonce:entryEcho.get(entry) ?? null}))
    const selectionScope = pending.length ? 'PENDING_NETWORK' : 'RECENT_HEADER_ECHO_SAMPLE'
    if (!pending.length) {
      const recent = [...records.entries()].filter(([,row]) => row.echoed).slice(-1)
      targets = recent.map(([nonce]) => ({entry:null,nonce}))
    }
    const counts = new Map()
    for (const {nonce} of targets) if (nonce) counts.set(nonce,(counts.get(nonce) ?? 0)+1)
    const publicTargets = targets.map(({entry,nonce},index) => ({index,
      requestObjectTaggedLocally: !!nonce && records.has(nonce),
      primaryResponseExtraInfoEchoObserved: !!nonce && records.has(nonce),
      ambiguousSelection: !!nonce && counts.get(nonce) !== 1,
      ...(!nonce && entry ? {observation:descriptor(entry)} : {})}))
    const requests = targets.flatMap(({nonce},targetIndex) => nonce && counts.get(nonce) === 1 && records.has(nonce) ? [{nonce,targetIndex}] : [])
    const result = Object.freeze({selectionScope,targets:structuredClone(publicTargets),targetsTruncated:pending.length > ownedRPCLimits.query})
    selections.set(result,{requests,publicTargets,selectionScope,truncated:result.targetsTruncated,contextEntries:targets.map(row=>row.entry)})
    return result
  }
  function requestPurposes(selection) {
    const facts = {scope:'owned-delayed-approval-request-purpose',phase:'AFTER_ORIGINAL_CHECKS_BEFORE_OWNED_BROWSER_CLOSE',
      observation:'EXACT_REQUEST_OBJECT_BEFORE_FIRST_OWNED_CONTINUE',rawIdentifiersOrHeadersOrBodyValuesReported:false,
      additionalProtocolCommands:0,additionalHTTPRequests:0,completionGateChanged:false,responseDeliveryEstablished:false,
      protocolSessionIdentityEstablished:false,serverRequestCorrelationEstablished:false,qualifiesNavigation:false,
      maximumQuery:ownedRPCLimits.query,observerReadErrors:purposeReadErrors}
    const selected=selections.get(selection)
    if (!selected) return {...facts,status:'UNAVAILABLE',reason:'SELECTION',selectionScope:'UNAVAILABLE',targetsTruncated:false,targets:[]}
    const byTarget=new Map(selected.requests.map(row=>[row.targetIndex,row]))
    return {...facts,status:'AVAILABLE',reason:null,selectionScope:selected.selectionScope,targetsTruncated:selected.truncated,
      targets:selected.publicTargets.map(target=>{
        const chosen=byTarget.get(target.index),record=chosen && records.get(chosen.nonce)
        const matched=!!record && !target.ambiguousSelection
        return {index:target.index,requestObjectTagStatus:target.ambiguousSelection ? 'AMBIGUOUS' : matched ? 'MATCHED' : 'UNAVAILABLE',
          sameUniqueOwnedHeaderRecordAsSelectedTarget:matched,
          purpose:matched ? {...record.purpose} : unavailablePurpose('NO_UNIQUE_OWNED_HEADER_RECORD')}
      })}
  }
  function requestContexts(selection) {
    const facts={scope:'owned-loopback-rpc-request-context',phase:'AFTER_ORIGINAL_CHECKS_BEFORE_OWNED_BROWSER_CLOSE',
      rawIdentifiersAddressesHeadersOrBodyValuesReported:false,additionalProtocolCommands:0,additionalHTTPRequests:0,
      completionGateChanged:false,qualifiesNavigation:false,responseDeliveryEstablished:false,protocolSessionIdentityEstablished:false,
      rendererDocumentCommitEstablished:false,maximumQuery:ownedRPCLimits.query,observer:summarizeRPCRequestContext(requestContext)}
    const chosen=selections.get(selection)
    if (!chosen) return {...facts,status:'UNAVAILABLE',reason:'SELECTION',selectionScope:'UNAVAILABLE',targetsTruncated:false,targets:[]}
    const byTarget=new Map(chosen.requests.map(row=>[row.targetIndex,row]))
    return {...facts,status:'AVAILABLE',reason:null,selectionScope:chosen.selectionScope,targetsTruncated:chosen.truncated,
      targets:chosen.publicTargets.map(target=>{
        const selected=byTarget.get(target.index),record=selected && records.get(selected.nonce),matched=!!record&&!target.ambiguousSelection
        return {index:target.index,requestObjectTagStatus:target.ambiguousSelection?'AMBIGUOUS':matched?'MATCHED':'UNAVAILABLE',
          sameUniqueOwnedHeaderRecordAsSelectedTarget:matched,
          context:matched?describeRPCRequestContext(requestContext,record.contextReference,chosen.contextEntries[target.index]):unavailableRPCRequestContext()}
      })}
  }
  async function read(fixture,selection,{fetchImpl = globalThis.fetch,timeoutMilliseconds = ownedRPCLimits.timeout} = {}) {
    const selected = selections.get(selection)
    if (!selected) return unavailable('SELECTION')
    const facts = {...baseFacts, selectionScope:selected.selectionScope, targets:structuredClone(selected.publicTargets),
      targetsTruncated:selected.truncated, maximumQuery:ownedRPCLimits.query}
    if (!selected.requests.length) return {...facts,status:'UNAVAILABLE',reason:'NO_UNIQUE_OWNED_HEADER_ECHO'}
    try {
      if (loopback(fixture.base).origin !== base.origin || typeof fixture.token !== 'string' || !/^[a-f0-9]{64}$/.test(fixture.token) ||
        typeof fetchImpl !== 'function' || !Number.isSafeInteger(timeoutMilliseconds) || timeoutMilliseconds < 1 || timeoutMilliseconds > ownedRPCLimits.timeout)
        return {...facts,status:'UNAVAILABLE',reason:'CONFIGURATION'}
    } catch {return {...facts,status:'UNAVAILABLE',reason:'CONFIGURATION'}}
    const controller = new AbortController()
    let timer
    try {
      const deadline = new Promise(resolve => {timer = setTimeout(() => {controller.abort();resolve({...facts,status:'UNAVAILABLE',reason:'DEADLINE'})},timeoutMilliseconds)})
      const observation = (async () => {
        try {
          const response = await fetchImpl(new URL('/__fixture/rpc-correlations',base),{method:'GET',redirect:'error',
            headers:{'X-ZVM-Fixture':fixture.token,[ownedRPCQueryHeader]:selected.requests.map(row => row.nonce).join(',')},signal:controller.signal})
          if (response.status !== 200 || !response.headers.get('content-type')?.toLowerCase().startsWith('application/json'))
            return {...facts,status:'UNAVAILABLE',reason:'HTTP_RESPONSE'}
          const snapshot = await snapshotBody(response,selected.requests.length)
          const matches = new Map(snapshot.matches.map((row,index) => [selected.requests[index].targetIndex,row]))
          return {...facts,status:'AVAILABLE',reason:null,serverTrackingOverflow:snapshot.overflow,localTrackingTruncated:truncated,
            targets:selected.publicTargets.map(row => {
              const match = matches.get(row.index)
              return {...structuredClone(row),serverMatchStatus:match?.status ?? 'UNAVAILABLE',
                handlerReturned:match?.returned ?? null,handlerReturnedNormally:match?.returnedNormally ?? null,
                handlerContextCanceled:match?.contextCanceled ?? null,
                requestToServerCorrelationEstablished:match?.status === 'MATCHED' && !row.ambiguousSelection,
                responseDeliveryEstablished:false,protocolSessionIdentityEstablished:false,qualifiesNavigation:false}
            })}
        } catch {return {...facts,status:'UNAVAILABLE',reason:controller.signal.aborted ? 'DEADLINE' : 'TRANSPORT_OR_SCHEMA'}}
      })()
      return await Promise.race([observation,deadline])
    } finally {clearTimeout(timer)}
  }
  async function readConnections(fixture,selection,options={}) {
    const selected=selections.get(selection)
    if(!selected)return unavailableConnectionObservation('SELECTION')
    const selectionFacts={selectionScope:selected.selectionScope,targetsTruncated:selected.truncated,maximumQuery:ownedRPCLimits.query}
    if(!selected.requests.length)return {...unavailableConnectionObservation('NO_UNIQUE_OWNED_HEADER_ECHO'),...selectionFacts,targets:structuredClone(selected.publicTargets)}
    const value=await readOwnedHTTP1Connections(fixture,selected.requests.map(row=>row.nonce),options)
    const {matches,...summary}=value
    const byTarget=new Map((matches??[]).map((row,index)=>[selected.requests[index].targetIndex,row]))
    return {...summary,...selectionFacts,targets:selected.publicTargets.map(target=>{
      const match=byTarget.get(target.index)
      return {...structuredClone(target),serverConnectionStatus:match?.status??'UNAVAILABLE',connectionReason:match?.reason??value.reason,
        HTTP1CycleBound:match?.HTTP1CycleBound??false,handlerReturned:match?.handlerReturned??null,
        handlerReturnedNormally:match?.handlerReturnedNormally??null,serverIdleAfterHandler:match?.serverIdleAfterHandler??false,
        connectionClosedAfterHandler:match?.connectionClosedAfterHandler??false,connectionHijackedAfterHandler:match?.connectionHijackedAfterHandler??false,
        requestToServerHTTP1CycleCorrelationEstablished:match?.status==='MATCHED'&&!target.ambiguousSelection,
        responseDeliveryEstablished:false,protocolSessionIdentityEstablished:false,qualifiesNavigation:false}
    })}
  }
  return {
    continue(request) {
      let options
      try {
        if (rpc(request.url(),request.method())) {
          let record = requestTags.get(request)
          if (!record && records.size === ownedRPCLimits.records) {limit('records'); fallbacks++}
          else {
            const headers = request.headers()
            if (!headers || typeof headers !== 'object' || Array.isArray(headers)) throw new Error('header view unavailable')
            const fields = Object.entries(headers)
            if (fields.length > 64 || fields.some(([name,value]) => name.toLowerCase() === ownedRPCHeader.toLowerCase() ||
              name.length > 128 || typeof value !== 'string' || value.length > 8192 || /[\r\n]/.test(value)) ||
              fields.reduce((size,[name,value]) => size+name.length+value.length,0) > 65536) throw new Error('bounded original headers required')
            const nonce = record?.nonce ?? nonceFactory()
            if (!nonceValid(nonce) || (!record && records.has(nonce))) throw new Error('unique owned nonce required')
            if (!record) {record={nonce,echoed:false,purpose:purposeSnapshot(request),contextReference:captureRPCRequestContext(requestContext,request)};records.set(nonce,record);requestTags.set(request,record);tagged++}
            options = {headers:Object.fromEntries([...fields,[ownedRPCHeader,nonce]])}
          }
        }
      } catch {readErrors++; fallbacks++}
      // The original operation stays outside the observation catch: a sync
      // throw or rejection is propagated once rather than attempted again.
      return options ? request.continue(options) : request.continue()
    },
    observeSignal(type,event,entry,session) {
      try {observeRPCRequestContext(requestContext,type,event,session)} catch {}
      try {
        if (type !== 'Network.responseReceivedExtraInfo') return
        const nonce = echoHeader(event?.headers)
        if (!nonce || !records.has(nonce) || !idValid(event.requestId)) return
        const state=scopeState(session)
        if (!state) return
        if (entry && !register(state,event.requestId,entry)) return
        const known=state.bindings.get(event.requestId)
        if (known) {
          if (!known.reused) bindEcho(known.entry,nonce)
          return
        }
        if (!state.early.has(event.requestId) && state.early.size === ownedRPCLimits.early) {limit('earlyHeaderEchoes'); return}
        if (state.early.has(event.requestId) && state.early.get(event.requestId) !== nonce) state.early.set(event.requestId,null)
        else state.early.set(event.requestId,nonce)
      } catch {readErrors++}
    },
    bind(id,entry,session) {
      try {bindRPCRequestContext(requestContext,id,entry,session)} catch {}
      const state=scopeState(session)
      if (!state || !register(state,id,entry) || !state.early.has(id)) return
      const nonce=state.early.get(id);state.early.delete(id)
      if (nonce === null) entryEcho.set(entry,null)
      else bindEcho(entry,nonce)
    },
    diagnostic:descriptor,
    requestPurposes,
    requestContexts,
    select,
    read,
    readConnections,
    async reportConnections(fixture,selection) {
      try {console.log('OWNED-MARKET-RPC-CONNECTION '+JSON.stringify({phase:'AFTER_ORIGINAL_CHECKS_BEFORE_OWNED_BROWSER_CLOSE',...await readConnections(fixture,selection)}))}catch{}
    },
    summary() {return {...baseFacts,taggedRequests:tagged,fallbacks,readErrors,primaryEchoes,records:records.size,
      early:retainedScopes.reduce((total,state)=>total+state.early.size,0),scopes:retainedScopes.length,
      bindings:retainedScopes.reduce((total,state)=>total+state.bindings.size,0),
      limitHits:{...limitHits},primaryScopeUsage:retainedScopes.map((state,index)=>({index,bindings:state.bindings.size,early:state.early.size})),
      truncated,maximumRecords:ownedRPCLimits.records,maximumEarlyPerScope:ownedRPCLimits.early,
      maximumBindingsPerScope:ownedRPCLimits.bindings,maximumScopes:ownedRPCLimits.scopes,
      reusedOrUntrackedPrimaryRequestIdsRefused:true,lateHeaderEchoSupported:true,primaryObservationScopesIsolated:true,
      existingHeaderValuesReadLocally:true,HTTPHeaderAddedOnlyToOwnedRPC:true}},
    async report(fixture,selection) {
      try {const value=await read(fixture,selection);console.log('OWNED-MARKET-RPC-CORRELATION '+JSON.stringify({
        phase:'AFTER_ORIGINAL_CHECKS_BEFORE_OWNED_BROWSER_CLOSE',...value,local:this.summary()}))} catch {}
    },
  }
}
