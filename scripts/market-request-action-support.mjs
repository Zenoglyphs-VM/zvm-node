// Observe the fixture's existing continue/abort calls. Do not send commands here.
import {types} from 'node:util'
import {performance} from 'node:perf_hooks'

export const requestActionObservationLimits = Object.freeze({pending: 256, perRequest: 3})
const nativeThen = Promise.prototype.then, nativePrototype = Promise.prototype
const object = value => value !== null && (typeof value === 'object' || typeof value === 'function')
const base = Object.freeze({observationWindow: 'OWNED_METHOD_CALL_AND_RETURNED_PROMISE',
  requestObjectLocalObservation: true, requestFieldsRead: false, resultOrErrorValuesLogged: false,
  protocolCommandsSent: 0, protocolAcknowledgementEstablished: false, networkCompletionEstablished: false,
  serverRequestCorrelationEstablished: false, completionGateChanged: false,
  maximumPendingActions: requestActionObservationLimits.pending, maximumActionsPerRequest: requestActionObservationLimits.perRequest})

export function createRequestActionObserver() {
  const records = new WeakMap()
  let pending = 0, refusedActions = 0
  return Object.freeze({
    observe(request, kind, operation) {
      // Refusing diagnostic allocation must never refuse the original operation.
      if (!object(request) || !['CONTINUE', 'ABORT'].includes(kind)) return operation()
      let entry = records.get(request)
      if (!entry) {entry = {actions: [], truncated: false}; records.set(request, entry)}
      if (entry.actions.length === requestActionObservationLimits.perRequest || pending === requestActionObservationLimits.pending) {
        entry.truncated = true; refusedActions++; return operation()
      }
      const action = {kind, state: 'CALLING', started: performance.now(), elapsedMilliseconds: null,
        returnedPromiseIdentityChanged: false}
      entry.actions.push(action)
      let result
      try {result = operation()} catch (error) {
        action.state = 'SYNC_THROW'; action.elapsedMilliseconds = Math.round(performance.now() - action.started)
        throw error
      }
      if (!types.isPromise(result)) {
        action.state = 'SYNC_RETURN'; action.elapsedMilliseconds = Math.round(performance.now() - action.started)
        return result
      }
      // Do not assimilate unknown thenables or promise subclasses, or read .then.
      if (Object.getPrototypeOf(result) !== nativePrototype) {action.state = 'UNOBSERVED_PROMISE'; return result}
      action.state = 'PENDING'; pending++
      const settled = state => {
        action.state = state; action.elapsedMilliseconds = Math.round(performance.now() - action.started); pending--
      }
      try {
        const observed = nativeThen.call(result, value => {settled('FULFILLED'); return value},
          error => {settled('REJECTED'); throw error})
        // Return the derived promise so an uncaught rejection is not consumed.
        // Fulfillment/rejection values are unchanged; promise identity and one
        // settlement microtask differ. This is method-outcome diagnostics only.
        action.returnedPromiseIdentityChanged = true
        return observed
      } catch {
        action.state = 'UNOBSERVED_PROMISE'; pending--; return result
      }
    },
    describe(request) {
      const entry = object(request) ? records.get(request) : undefined
      return {...base, status: entry ? 'OBSERVED' : 'NOT_OBSERVED', truncated: entry?.truncated ?? false,
        actions: entry?.actions.map(action => ({kind: action.kind, state: action.state,
          ageMilliseconds: Math.round(performance.now() - action.started), elapsedMilliseconds: action.elapsedMilliseconds,
          returnedPromiseIdentityChanged: action.returnedPromiseIdentityChanged})) ?? []}
    },
    summary() {return {...base, pendingActions: pending, refusedActions}},
  })
}

const shared = createRequestActionObserver()
export const observeOwnedRequestAction = (request, kind, operation) => shared.observe(request, kind, operation)
export const describeOwnedRequestActions = request => shared.describe(request)
