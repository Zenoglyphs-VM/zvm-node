// Read only an owned HTTP/1.1 connection observation. Request values, bodies,
// addresses and capability material never enter its public diagnostic result.
export const connectionScope = 'owned-loopback-http1-post-handler-connection-lifecycle'
export const connectionLimits = Object.freeze({records:256,connections:32,query:8,body:16384,chunks:64,timeout:750})
const facts = {scope:connectionScope,HTTPArgumentsReplaced:false,payloadOrAddressesReported:false,
  responseDeliveryEstablished:false,protocolSessionIdentityEstablished:false,qualifiesNavigation:false,
  additionalProtocolCommands:0,fixtureCorrelationIsAuthentication:false}
export const unavailableConnectionObservation = reason => ({...facts,status:'UNAVAILABLE',reason})
const exact = (value,keys) => value && typeof value === 'object' && !Array.isArray(value) &&
  Object.keys(value).length === keys.length && keys.every(key=>Object.hasOwn(value,key))
const nonceValid = value => typeof value === 'string' && /^[a-f0-9]{32}$/.test(value)

export function validateConnectionSnapshot(value,count) {
  const keys=['version','scope','maximumRecords','maximumConnections','maximumQuery','connectionTrackingOverflow','matches',
    'HTTPArgumentsReplaced','payloadOrAddressesReported','responseDeliveryEstablished','protocolSessionIdentityEstablished','qualifiesNavigation']
  if (!exact(value,keys) || value.version!==1 || value.scope!==connectionScope || value.maximumRecords!==256 ||
      value.maximumConnections!==32 || value.maximumQuery!==8 || typeof value.connectionTrackingOverflow!=='boolean' ||
      value.HTTPArgumentsReplaced!==false || value.payloadOrAddressesReported!==false || value.responseDeliveryEstablished!==false ||
      value.protocolSessionIdentityEstablished!==false || value.qualifiesNavigation!==false || !Array.isArray(value.matches) ||
      value.matches.length!==count) throw new Error('invalid owned connection snapshot')
  const matchKeys=['index','status','reason','HTTP1CycleBound','handlerReturned','handlerReturnedNormally',
    'serverIdleAfterHandler','connectionClosedAfterHandler','connectionHijackedAfterHandler']
  const booleans=matchKeys.slice(3)
  const reasons={MATCHED:[''],UNKNOWN:['NO_REQUEST'],AMBIGUOUS:['NONCE','CYCLE'],
    UNAVAILABLE:['NO_CONTEXT','PROTOCOL','FOREIGN_CONTEXT','NO_ACTIVE_CYCLE']}
  for (const [index,row] of value.matches.entries()) {
    if (!exact(row,matchKeys) || row.index!==index || !Object.hasOwn(reasons,row.status) ||
        !reasons[row.status].includes(row.reason) || booleans.some(key=>typeof row[key]!=='boolean') ||
        (row.handlerReturnedNormally&&!row.handlerReturned) ||
        (row.status==='MATCHED')!==row.HTTP1CycleBound ||
        ([row.serverIdleAfterHandler,row.connectionClosedAfterHandler,row.connectionHijackedAfterHandler].some(Boolean)&&
          (!row.HTTP1CycleBound||!row.handlerReturned)) ||
        (row.status==='UNKNOWN'&&booleans.some(key=>row[key]))) throw new Error('invalid owned connection match')
  }
  return value
}

function configuration(fixture,nonces,options) {
  if (!fixture || typeof fixture.base!=='string' || fixture.base.length>2048 || typeof fixture.token!=='string' ||
      !/^[a-f0-9]{64}$/.test(fixture.token) || !Array.isArray(nonces) || nonces.length<1 || nonces.length>8 ||
      nonces.some(value=>!nonceValid(value)) || new Set(nonces).size!==nonces.length || typeof options.fetchImpl!=='function' ||
      !Number.isSafeInteger(options.timeoutMilliseconds) || options.timeoutMilliseconds<1 || options.timeoutMilliseconds>750) throw new Error('invalid owned connection configuration')
  const base=new URL(fixture.base)
  if (base.protocol!=='http:'||base.hostname!=='127.0.0.1'||!base.port||base.username||base.password||base.pathname!=='/'||base.search||base.hash)
    throw new Error('invalid owned connection origin')
  return base
}
async function readBody(response,count) {
  const reader=response.body?.getReader();if(!reader)throw new Error('missing owned connection reader')
  let bytes=0,reads=0;const chunks=[]
  try {
    for (;;) {
      const {done,value}=await reader.read();if(done)break
      if (!(value instanceof Uint8Array)||++reads>64||bytes+value.byteLength>16384) {
        void reader.cancel().catch(()=>{});throw new Error('bounded owned connection body required')
      }
      bytes+=value.byteLength;chunks.push(value)
    }
  } finally {reader.releaseLock()}
  const raw=new Uint8Array(bytes);let offset=0
  for(const chunk of chunks){raw.set(chunk,offset);offset+=chunk.byteLength}
  return validateConnectionSnapshot(JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(raw)),count)
}
export async function readOwnedHTTP1Connections(fixture,nonces,{fetchImpl=globalThis.fetch,timeoutMilliseconds=750}={}) {
  let base
  try {base=configuration(fixture,nonces,{fetchImpl,timeoutMilliseconds})} catch {return unavailableConnectionObservation('CONFIGURATION')}
  const controller=new AbortController();let timer
  try {
    const deadline=new Promise(resolve=>{timer=setTimeout(()=>{controller.abort();resolve(unavailableConnectionObservation('DEADLINE'))},timeoutMilliseconds)})
    const observation=(async()=>{
      try {
        const response=await fetchImpl(new URL('/__fixture/rpc-connections',base),{method:'GET',redirect:'error',
          headers:{'X-ZVM-Fixture':fixture.token,'X-ZVM-Fixture-Requests':nonces.join(',')},signal:controller.signal})
        if(response.status!==200||!response.headers.get('content-type')?.toLowerCase().startsWith('application/json'))
          return unavailableConnectionObservation('HTTP_RESPONSE')
        const snapshot=await readBody(response,nonces.length)
        return {...facts,status:'AVAILABLE',reason:null,connectionTrackingOverflow:snapshot.connectionTrackingOverflow,
          matches:snapshot.matches.map(row=>({...row})),maximumRecords:256,maximumConnections:32,maximumQuery:8}
      }catch{return unavailableConnectionObservation(controller.signal.aborted?'DEADLINE':'TRANSPORT_OR_SCHEMA')}
    })()
    return await Promise.race([observation,deadline])
  } finally {clearTimeout(timer)}
}
