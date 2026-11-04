// Read one current SDK boolean. Never infer wire acknowledgement or delivery.
import {maximumOpaqueRequestIdLength, reviewedPuppeteerSourceVersion} from './market-request-identity-support.mjs'

const object = value => value !== null && (typeof value === 'object' || typeof value === 'function')
const base = Object.freeze({reviewedPuppeteerSourceVersion, observationWindow: 'DIAGNOSTIC_QUERY',
  onlyInterceptResolutionBooleanRead: true, requestHeadersBodiesURLsOrErrorValuesRead: false,
  protocolCommandsSent: 0, protocolAcknowledgementEstablished: false, networkCompletionEstablished: false,
  serverRequestCorrelationEstablished: false, exactProtocolSessionIdentityEstablished: false,
  completionGateChanged: false, observationCanBecomeStaleImmediately: true})
const unavailable = reason => ({...base, status: 'UNAVAILABLE', reason, interceptResolutionHandled: null})

export function readCurrentRequestHandling(request) {
  if (!object(request)) return unavailable('REQUEST_UNAVAILABLE')
  try {
    const method = request.isInterceptResolutionHandled
    if (typeof method !== 'function') return unavailable('METHOD_UNAVAILABLE')
    const value = Reflect.apply(method, request, [])
    return typeof value === 'boolean' ? {...base, status: 'AVAILABLE', reason: null, interceptResolutionHandled: value} : unavailable('NON_BOOLEAN_STATE')
  } catch {return unavailable('READ_UNAVAILABLE')}
}

export function matchCurrentRequestHandling(active, recent, id, url, method) {
  const empty = (status, reason) => ({...base, status, reason, activeMatches: 0, recentMatches: 0,
    requestObjectLocalObservation: true, observation: null})
  try {
    if (!(active instanceof Map) || !(recent instanceof Map) || active.size > 256 || recent.size > 8 ||
        typeof id !== 'string' || !id || id.length > maximumOpaqueRequestIdLength || /[^\x21-\x7e]/.test(id) ||
        typeof url !== 'string' || typeof method !== 'string') return empty('UNAVAILABLE', 'INVALID_QUERY_OR_BOUNDS')
    const matching = records => [...records].filter(([, entry]) => entry?.requestIdentity?.status === 'AVAILABLE' &&
      entry.requestIdentity.requestId === id && entry.url === url && entry.method === method)
    const live = matching(active), ended = matching(recent), values = [...live, ...ended]
    return {...empty(values.length > 1 ? 'AMBIGUOUS' : values.length === 1 ? 'MATCHED' : 'NO_MATCH', null),
      activeMatches: live.length, recentMatches: ended.length,
      observation: values.length === 1 ? readCurrentRequestHandling(values[0][0]) : null}
  } catch {return empty('UNAVAILABLE', 'QUERY_UNAVAILABLE')}
}
