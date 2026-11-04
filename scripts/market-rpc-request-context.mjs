// Owned request data stays local. Only fixed classes and document-request relations leave this module.
import {Buffer} from 'node:buffer'

export const rpcRequestContextLimits = Object.freeze({records: 256, scopes: 4, frames: 16, starts: 256, bindings: 512, inputBytes: 16384, batch: 16})
const observers = new WeakMap(), object = value => value !== null && (typeof value === 'object' || typeof value === 'function')
const opaque = value => typeof value === 'string' && value.length > 0 && value.length <= 128 && !/[^\x21-\x7e]/.test(value)
const address = value => typeof value === 'string' && /^0x[0-9a-fA-F]{40}$/.test(value)
const methods = Object.freeze({eth_call: 'ETH_CALL', eth_chainId: 'CHAIN_ID', eth_blockNumber: 'BLOCK_NUMBER',
  eth_getBlockByNumber: 'BLOCK_BY_NUMBER', eth_getBlockByHash: 'BLOCK_BY_HASH', eth_getBalance: 'BALANCE',
  eth_getTransactionCount: 'TRANSACTION_COUNT', eth_getTransactionReceipt: 'RECEIPT', eth_getLogs: 'LOGS',
  eth_getCode: 'CODE', eth_getStorageAt: 'STORAGE', eth_estimateGas: 'ESTIMATE_GAS', eth_gasPrice: 'GAS_PRICE',
  eth_maxPriorityFeePerGas: 'PRIORITY_FEE', eth_feeHistory: 'FEE_HISTORY', eth_sendRawTransaction: 'SEND_RAW_TRANSACTION'})
const callKinds = new Set(['APPROVAL_FOR_ALL', 'ALLOWANCE', 'TOKEN_BALANCE', 'TOKEN_OWNER', 'ROYALTY_INFO', 'FEE_BPS', 'FEE_RECIPIENT'])
const noRequest = reason => ({status: 'UNAVAILABLE', reason, batch: null, entries: null, calls: []})
const noDocument = reason => ({status: 'UNAVAILABLE', reason, documentRequestOrdinalAtStart: null,
  latestSameFrameDocumentRequestOrdinal: null, loaderMatchesDocumentAtStart: null, loaderMatchesLatestDocument: null,
  timestampNotBeforeDocumentStart: null, sameDocumentRequestStillLatest: null})
const copyRequest = value => ({...value, calls: value.calls.map(row => ({...row}))})
export const unavailableRPCRequestContext = () => ({request: noRequest('CONTEXT_UNAVAILABLE'), document: noDocument('CONTEXT_UNAVAILABLE')})

export function createOwnedRPCRequestContext(origin, {targets = {}, selectors = () => ({})} = {}) {
  const base = new URL(origin)
  if (base.protocol !== 'http:' || base.hostname !== '127.0.0.1' || !base.port || base.username || base.password ||
      base.pathname !== '/' || base.search || base.hash) throw new Error('owned loopback origin required')
  const roles = [['COLLECTION', targets.collection], ['EXCHANGE', targets.exchange], ['TOKEN', targets.token]]
    .filter(([, value]) => address(value)).map(([role, value]) => [role, value.toLowerCase()])
  const captures = new WeakMap(), entries = new WeakMap(), sessionStates = new WeakMap(), retainedStates = []
  let captured = 0, readErrors = 0, refused = 0, truncated = false, selectorTable
  const refuse = () => {refused = Math.min(Number.MAX_SAFE_INTEGER, refused + 1); truncated = true}
  const owned = (url, method) => {
    try {const value = new URL(url); return value.origin === base.origin && value.pathname === '/rpc' && method === 'POST'} catch {return false}
  }
  const stateFor = session => {
    if (!object(session)) return null
    if (sessionStates.has(session)) return sessionStates.get(session)
    if (retainedStates.length === rpcRequestContextLimits.scopes) {refuse(); return null}
    const state = {documents: new Map(), starts: new Map(), bindings: new Map(), documentCount: 0}
    sessionStates.set(session, state); retainedStates.push(state); return state
  }
  const selectCallKind = data => {
    if (selectorTable === undefined) {
      selectorTable = new Map()
      try {
        const value = selectors()
        if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).length > callKinds.size) throw new Error('selector shape')
        for (const [kind, selector] of Object.entries(value)) {
          if (!callKinds.has(kind) || typeof selector !== 'string' || !/^0x[0-9a-f]{8}$/.test(selector)) throw new Error('selector value')
          selectorTable.set(selector, selectorTable.has(selector) ? 'AMBIGUOUS_CALL' : kind)
        }
      } catch {selectorTable.clear(); readErrors++}
    }
    if (typeof data !== 'string' || data.length < 10 || data.length > rpcRequestContextLimits.inputBytes || !/^0x[0-9a-fA-F]+$/.test(data)) return 'OTHER_CALL'
    return selectorTable.get(data.slice(0, 10).toLowerCase()) ?? 'OTHER_CALL'
  }
  const classify = request => {
    try {
      if (!owned(request.url(), request.method())) return noRequest('UNOWNED_REQUEST')
      const raw = request.postData()
      if (typeof raw !== 'string') return noRequest('BODY_UNAVAILABLE')
      if (raw.length > rpcRequestContextLimits.inputBytes || Buffer.byteLength(raw, 'utf8') > rpcRequestContextLimits.inputBytes) return noRequest('BODY_LIMIT')
      let value
      try {value = JSON.parse(raw)} catch {return noRequest('BODY_JSON')}
      const batch = Array.isArray(value), rows = batch ? value : [value]
      if (!rows.length || rows.length > rpcRequestContextLimits.batch) return noRequest('BATCH_LIMIT')
      const calls = []
      for (const row of rows) {
        if (!row || typeof row !== 'object' || Array.isArray(row) || typeof row.method !== 'string' || row.method.length > 64) return noRequest('BODY_SHAPE')
        const method = Object.hasOwn(methods, row.method) ? methods[row.method] : 'OTHER_RPC'
        let callKind = 'NOT_CALL', targetRole = 'NOT_CALL'
        if (method === 'ETH_CALL') {
          const call = Array.isArray(row.params) ? row.params[0] : null
          callKind = selectCallKind(call?.data)
          const selected = address(call?.to) ? roles.filter(([, value]) => value === call.to.toLowerCase()) : []
          targetRole = selected.length === 1 ? selected[0][0] : selected.length > 1 ? 'AMBIGUOUS_TARGET' : 'OTHER_TARGET'
        }
        calls.push({method, callKind, targetRole})
      }
      return {status: 'AVAILABLE', reason: null, batch, entries: calls.length, calls}
    } catch {readErrors++; return noRequest('READ_REFUSED')}
  }
  const api = {
    capture(request) {
      if (captured === rpcRequestContextLimits.records) {refuse(); return null}
      const reference = Object.freeze({}); captures.set(reference, classify(request)); captured++; return reference
    },
    observe(type, event, session) {
      if (type !== 'Network.requestWillBeSent') return
      try {
        const state = stateFor(session)
        if (!state || !event || !opaque(event.requestId) || !Number.isFinite(event.timestamp)) return
        if (event.type === 'Document' && opaque(event.frameId) && opaque(event.loaderId)) {
          let ownedDocument = false
          try {ownedDocument = new URL(event.request?.url).origin === base.origin} catch {}
          if (!ownedDocument) {
            state.documentCount = Math.min(Number.MAX_SAFE_INTEGER, state.documentCount + 1)
            state.documents.delete(event.frameId); return
          }
          if (!state.documents.has(event.frameId) && state.documents.size === rpcRequestContextLimits.frames) {refuse(); return}
          state.documentCount = Math.min(Number.MAX_SAFE_INTEGER, state.documentCount + 1)
          state.documents.set(event.frameId, {loader: event.loaderId, timestamp: event.timestamp, ordinal: state.documentCount})
        }
        if (!owned(event.request?.url, event.request?.method)) return
        if (state.bindings.has(event.requestId)) {state.bindings.get(event.requestId).ambiguous = true; return}
        if (state.starts.has(event.requestId)) {state.starts.set(event.requestId, null); return}
        if (state.starts.size === rpcRequestContextLimits.starts) {refuse(); return}
        const document = opaque(event.frameId) ? state.documents.get(event.frameId) : null
        state.starts.set(event.requestId, {frame: event.frameId, loader: event.loaderId, timestamp: event.timestamp,
          document: document ? {...document} : null})
      } catch {readErrors++}
    },
    bind(id, entry, session) {
      try {
        if (!opaque(id) || !object(entry)) return
        const state = stateFor(session)
        if (!state) return
        const previous = state.bindings.get(id)
        if (previous) {
          if (previous.entry !== entry) {previous.ambiguous = true; entries.set(entry, {ambiguous: true})}
          return
        }
        if (state.bindings.size === rpcRequestContextLimits.bindings) {refuse(); return}
        const value = {state, entry, start: state.starts.get(id) ?? null, ambiguous: false}
        state.starts.delete(id); state.bindings.set(id, value); entries.set(entry, value)
      } catch {readErrors++}
    },
    describe(reference, entry) {
      const request = object(reference) ? captures.get(reference) : null
      const result = {request: request ? copyRequest(request) : noRequest('CAPTURE_UNAVAILABLE'), document: noDocument('NO_PENDING_ENTRY')}
      if (!object(entry)) return result
      const bound = entries.get(entry)
      if (!bound?.start || bound.ambiguous) {result.document = noDocument(bound?.ambiguous ? 'AMBIGUOUS_BINDING' : 'NO_BOUND_START'); return result}
      const start = bound.start, before = start.document, latest = bound.state.documents.get(start.frame)
      if (!before || !latest || !opaque(start.loader)) {result.document = noDocument('DOCUMENT_UNAVAILABLE'); return result}
      result.document = {status: 'AVAILABLE', reason: null, documentRequestOrdinalAtStart: before.ordinal,
        latestSameFrameDocumentRequestOrdinal: latest.ordinal, loaderMatchesDocumentAtStart: start.loader === before.loader,
        loaderMatchesLatestDocument: start.loader === latest.loader, timestampNotBeforeDocumentStart: start.timestamp >= before.timestamp,
        sameDocumentRequestStillLatest: before.ordinal === latest.ordinal}
      return result
    },
    summary() {
      return {status: 'AVAILABLE', capturedRequests: captured, readErrors, refusedObservations: refused, truncated,
        scopes: retainedStates.length, documentRequestsObserved: retainedStates.reduce((sum, value) => sum + value.documentCount, 0),
        scopeUsage: retainedStates.map((value, index) => ({index, frames: value.documents.size, starts: value.starts.size, bindings: value.bindings.size})),
        maximumRecords: rpcRequestContextLimits.records, maximumScopes: rpcRequestContextLimits.scopes,
        maximumFramesPerScope: rpcRequestContextLimits.frames, maximumStartsPerScope: rpcRequestContextLimits.starts,
        maximumBindingsPerScope: rpcRequestContextLimits.bindings, maximumInputBytes: rpcRequestContextLimits.inputBytes,
        maximumBatch: rpcRequestContextLimits.batch, requestBodyReadLocally: true,
        rawIdentifiersAddressesHeadersOrBodyValuesReported: false, additionalProtocolCommands: 0, additionalHTTPRequests: 0,
        completionGateChanged: false, rendererDocumentCommitOrBodyDeliveryEstablished: false}
    },
  }
  const handle = Object.freeze({}); observers.set(handle, api); return handle
}

export const captureRPCRequestContext = (observer, request) => observers.get(observer)?.capture(request) ?? null
export const observeRPCRequestContext = (observer, type, event, session) => observers.get(observer)?.observe(type, event, session)
export const bindRPCRequestContext = (observer, id, entry, session) => observers.get(observer)?.bind(id, entry, session)
export const describeRPCRequestContext = (observer, reference, entry) => observers.get(observer)?.describe(reference, entry) ?? unavailableRPCRequestContext('OBSERVER_NOT_CONFIGURED')
export const summarizeRPCRequestContext = observer => observers.get(observer)?.summary() ?? {status: 'UNAVAILABLE', reason: 'OBSERVER_NOT_CONFIGURED'}
