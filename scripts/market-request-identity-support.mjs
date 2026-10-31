// Passive compatibility observations; these never satisfy a navigation gate.
export const reviewedPuppeteerSourceVersion = '25.11.0'
export const maximumOpaqueRequestIdLength = 128
const maximumActiveRecords = 256
const maximumRecentRecords = 8
const lifecycleSignals = new Set(['request', 'response', 'requestfinished', 'requestfailed'])
const validId = value => typeof value === 'string' && value.length > 0 &&
  value.length <= maximumOpaqueRequestIdLength && !/[^\x21-\x7e]/.test(value)

// The ID is retained only in the existing bounded in-memory Page record.
// Do not return it from a report or inspect request headers, bodies or clients.
export function readPageRequestIdentity(request, page) {
  try {
    const requestId = request.id
    if (!validId(requestId)) return {status: 'UNAVAILABLE', reason: 'ID_UNAVAILABLE', requestId: null}
    if (typeof request.frame !== 'function') return {status: 'UNAVAILABLE', reason: 'FRAME_UNAVAILABLE', requestId: null}
    const frame = request.frame()
    if (!frame || typeof frame.page !== 'function') return {status: 'UNAVAILABLE', reason: 'FRAME_UNAVAILABLE', requestId: null}
    if (!page || frame.page() !== page) return {status: 'UNAVAILABLE', reason: 'PAGE_MISMATCH', requestId: null}
    return {status: 'AVAILABLE', reason: null, requestId}
  } catch {
    return {status: 'UNAVAILABLE', reason: 'READ_UNAVAILABLE', requestId: null}
  }
}

export function pageRequestIdMatches(active, recent, requestId, url, method) {
  const base = {reviewedPuppeteerSourceVersion, samePageObjectRequired: true, sameURLAndMethodRequired: true,
    opaqueIdentifierLogged: false, exactProtocolSessionIdentityEstablished: false,
    completionGateChanged: false, maximumActiveRecords, maximumRecentRecords}
  const unavailable = reason => ({...base, status: 'UNAVAILABLE', reason, activeMatches: 0, recentMatches: 0,
    ambiguous: false, observationsTruncated: false, observations: []})
  if (!validId(requestId)) return unavailable('QUERY_ID_UNAVAILABLE')
  try {
    const bounded = (records, limit) => {
      const result = []
      for (const entry of records.values()) {
        if (result.length === limit) throw new Error('bounded record limit')
        result.push(entry)
      }
      return result
    }
    const live = bounded(active, maximumActiveRecords), ended = bounded(recent, maximumRecentRecords)
    const usable = entry => entry.requestIdentity?.status === 'AVAILABLE' && validId(entry.requestIdentity.requestId)
    if (![...live, ...ended].some(usable)) return unavailable('NO_USABLE_PAGE_IDS')
    const matches = entry => usable(entry) && entry.requestIdentity.requestId === requestId && entry.url === url && entry.method === method
    const a = live.filter(matches), b = ended.filter(matches), all = [...a, ...b]
    return {...base, status: all.length > 1 ? 'AMBIGUOUS' : all.length === 1 ? 'MATCHED' : 'NO_MATCH', reason: null,
      activeMatches: a.length, recentMatches: b.length, ambiguous: all.length > 1,
      observationsTruncated: all.length > maximumRecentRecords,
      observations: all.slice(0, maximumRecentRecords).map(entry => ({
        signals: entry.signals.filter(value => lifecycleSignals.has(value)).slice(-maximumRecentRecords),
        responseStatus: Number.isInteger(entry.responseStatus) && entry.responseStatus >= 100 && entry.responseStatus <= 599 ? entry.responseStatus : null,
        interceptResolutionHandled: typeof entry.interceptResolutionHandled === 'boolean' ? entry.interceptResolutionHandled : null,
        terminal: ['requestfinished', 'requestfailed'].includes(entry.terminal) ? entry.terminal : null,
        failureClass: ['net::ERR_ABORTED', 'net::ERR_FAILED', 'net::ERR_BLOCKED_BY_CLIENT', 'other'].includes(entry.failureClass) ? entry.failureClass : null}))}
  } catch {
    return unavailable('RECORDS_UNAVAILABLE_OR_OVER_LIMIT')
  }
}
