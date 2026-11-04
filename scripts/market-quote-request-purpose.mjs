// The fixture marks its existing held request object. No request field is read.
export const quotePurposeLimits = Object.freeze({markedRequests: 2})
export const quotePurposePhases = Object.freeze(['ACCOUNT_CHANGE_REFUSAL', 'FORM_EDIT_SNAPSHOT'])
const object = value => value !== null && (typeof value === 'object' || typeof value === 'function')

export function createQuoteRequestPurposeObserver() {
  const records = new WeakMap()
  let marked = 0, refused = 0, conflicts = 0
  const increment = value => Math.min(Number.MAX_SAFE_INTEGER, value + 1)
  return Object.freeze({
    mark(request, phase) {
      if (!object(request) || !quotePurposePhases.includes(phase)) {refused = increment(refused); return false}
      const previous = records.get(request)
      if (previous) {
        if (previous.status === 'MARKED' && previous.phase === phase) return true
        records.set(request, {status: 'AMBIGUOUS', phase: null}); conflicts = increment(conflicts); return false
      }
      if (marked === quotePurposeLimits.markedRequests) {refused = increment(refused); return false}
      records.set(request, {status: 'MARKED', phase}); marked++
      return true
    },
    describe(request) {
      if (!object(request)) return {status: 'UNAVAILABLE', phase: null}
      const value = records.get(request)
      return value ? {...value} : {status: 'NOT_MARKED', phase: null}
    },
    summary() {
      return {markedRequests: marked, maximumMarkedRequests: quotePurposeLimits.markedRequests,
        refusedMarks: refused, conflictingMarks: conflicts, requestFieldsRead: false,
        rawRequestValuesReported: false, additionalProtocolCommands: 0, additionalHTTPRequests: 0,
        completionGateChanged: false, responseDeliveryEstablished: false}
    },
  })
}
