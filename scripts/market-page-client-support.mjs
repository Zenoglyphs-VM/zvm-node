// Passive listeners on the request's existing client; no protocol commands.
import {maximumOpaqueRequestIdLength, reviewedPuppeteerSourceVersion} from './market-request-identity-support.mjs'

export const pageClientObservationLimits = Object.freeze({clients: 4, active: 256, recent: 8, signals: 8})
const events = ['Network.requestWillBeSent', 'Network.requestWillBeSentExtraInfo', 'Network.responseReceived',
  'Network.responseReceivedExtraInfo', 'Network.loadingFinished', 'Network.loadingFailed',
  'Network.requestServedFromCache', 'Fetch.requestPaused']
const validId = value => typeof value === 'string' && value.length > 0 &&
  value.length <= maximumOpaqueRequestIdLength && !/[^\x21-\x7e]/.test(value)
const status = value => Number.isInteger(value) && value >= 100 && value <= 599 ? value : null

export function createPageClientObserver(active, recent) {
  const clients = new Map(), observations = new WeakMap()
  let closed = false, readErrors = 0, refusedClients = 0, unmatchedSignals = 0
  const rows = () => {
    if (active.size > pageClientObservationLimits.active || recent.size > pageClientObservationLimits.recent)
      throw new Error('record bound exceeded')
    return [...active.values(), ...recent.values()]
  }
  const remove = (client, listeners) => {
    for (const [type, listener] of listeners) {
      try {client.off(type, listener)} catch {readErrors++}
    }
  }
  const receive = (client, type, event) => {
    if (closed) return
    try {
      const id = type === 'Fetch.requestPaused' ? event?.networkId : event?.requestId
      if (!validId(id)) return
      const matching = rows().filter(entry => observations.get(entry)?.client === client &&
        entry.requestIdentity?.status === 'AVAILABLE' && entry.requestIdentity.requestId === id)
      if (!matching.length) {unmatchedSignals++; return}
      // Copy only allowlisted scalars. Never read headers, bodies or URL data.
      const signal = {event: type, timestampPresent: Number.isFinite(event.timestamp),
        responseStatus: status(type === 'Fetch.requestPaused' ? event.responseStatusCode :
          type === 'Network.responseReceivedExtraInfo' ? event.statusCode : event.response?.status),
        responseStage: type === 'Fetch.requestPaused' ?
          event.responseStatusCode !== undefined || event.responseErrorReason !== undefined : null}
      for (const entry of matching) {
        const value = observations.get(entry)
        if (value.signals.length === pageClientObservationLimits.signals) {value.signals.shift(); value.truncated = true}
        value.signals.push({...signal})
      }
    } catch {readErrors++}
  }
  return {
    observe(request, entry) {
      if (observations.has(entry)) return
      const value = {status: 'UNAVAILABLE', reason: 'ID_UNAVAILABLE', client: null, signals: [], truncated: false}
      observations.set(entry, value)
      if (closed) {value.reason = 'OBSERVER_CLOSED'; return}
      if (entry.requestIdentity?.status !== 'AVAILABLE' || !validId(entry.requestIdentity.requestId)) return
      try {
        const client = request.client
        if (!client || typeof client.on !== 'function' || typeof client.off !== 'function' || client.detached === true) {
          value.reason = 'CLIENT_UNAVAILABLE'; return
        }
        if (!clients.has(client)) {
          if (clients.size === pageClientObservationLimits.clients) {refusedClients++; value.reason = 'CLIENT_LIMIT'; return}
          const listeners = []
          try {
            for (const type of events) {
              const listener = event => receive(client, type, event)
              // Register intent first so a throwing implementation is unwound.
              listeners.push([type, listener]); client.on(type, listener)
            }
          } catch {
            remove(client, listeners); value.reason = 'SUBSCRIPTION_UNAVAILABLE'; readErrors++; return
          }
          clients.set(client, listeners)
        }
        value.client = client; value.status = 'AVAILABLE'; value.reason = null
      } catch {value.reason = 'READ_UNAVAILABLE'; readErrors++}
    },
    matches(id, url, method) {
      const base = {reviewedPuppeteerSourceVersion, passiveOnly: true, completionGateChanged: false,
        rawIdentifiersLogged: false, crossSessionProtocolIdentityEstablished: false,
        serverRequestCorrelationEstablished: false, observationWindow: 'AFTER_PAGE_REQUEST_CALLBACK',
        clientSelection: 'ORIGINAL_REQUEST_CLIENT_OBJECT', followsClientReassignment: false,
        maximumClients: pageClientObservationLimits.clients, maximumSignalsPerRecord: pageClientObservationLimits.signals,
        closed, readErrors, refusedClients}
      try {
        if (!validId(id)) throw new Error('invalid query identity')
        const values = rows().filter(entry => entry.requestIdentity?.status === 'AVAILABLE' &&
          entry.requestIdentity.requestId === id && entry.url === url && entry.method === method)
        return {...base, status: values.length > 1 ? 'AMBIGUOUS' : values.length === 1 ? 'MATCHED' : 'NO_MATCH',
          observationsTruncated: values.length > pageClientObservationLimits.recent,
          observations: values.slice(0, pageClientObservationLimits.recent).map(entry => {
            const value = observations.get(entry)
            return {status: value?.status ?? 'UNAVAILABLE', reason: value?.reason ?? 'NOT_OBSERVED',
              signals: value?.signals.map(signal => ({...signal})) ?? [], signalsTruncated: value?.truncated ?? false}
          })}
      } catch {return {...base, status: 'UNAVAILABLE', observationsTruncated: false, observations: []}}
    },
    summary() {return {clients: clients.size, maximumClients: pageClientObservationLimits.clients,
      readErrors, refusedClients, unmatchedSignals, closed, passiveOnly: true, completionGateChanged: false}},
    close() {
      if (closed) return
      closed = true
      for (const [client, listeners] of clients) remove(client, listeners)
      clients.clear()
    },
  }
}
