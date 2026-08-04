// The remote wire deliberately includes lifecycle events so the client can
// update agent state. They are not conversation content: rendering them in the
// feed creates a second, unformatted copy when an engine mirrors its final
// response through a status event. Keep the feed to the three human-readable
// conversation shapes only.
const conversationKinds = new Set(['user', 'assistant', 'tool'])

export function displayTranscript(events) {
  if (!Array.isArray(events)) return []
  return events.filter((event) => conversationKinds.has(event?.kind))
}
