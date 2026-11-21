const providerID = "opencode"
const fallback = { providerID, id: "claude-haiku-5-5" }
const gemini = (model) =>
  model.providerID === providerID && model.id === "gemini-4-hydrogen" && model.variant === "low"
const sources = new Set(["typed", "voice", "tap", "timer"])
const primaryGemini = (event) => event.kind === "primary" && event.agent === "echoear" && gemini(event.model)
const rejected = (status) => status === 429 || status === 503 || status === 403
const isTurnBoundary = (row) => ["user", "idle", "synthetic", "shell"].includes(row.type)
const transient = ({ status, type }) =>
  (status === 429 && ["provider.rate-limit", "provider.quota", "provider.model-budget-limit"].includes(type)) ||
  (status === 503 && type === "provider.internal")

function unavailable({ status, type, response }) {
  if (status !== 403 || type !== "provider.auth") return false
  // Console's Gemini route encodes this endpoint rejection as a Google error.
  // OpenCode classifies the HTTP status as auth; do not accept other 403s.
  try {
    const body = JSON.parse(response?.body).error
    return body.code === 403 && body.status === "UNKNOWN" &&
      body.message === "Upstream request failed: Endpoint is unavailable."
  } catch {
    return false
  }
}

export default {
  id: "echoear.model-fallback",
  async setup({ session }) {
    const options = { providerID }
    const responses = new Map()
    const turns = new Map()
    const history = ({ sessionID }) => session.context({ sessionID }).catch(() => [])

    await session.hook("http.request", (event) => {
      if (primaryGemini(event)) responses.delete(event.sessionID)
    }, options)
    await session.hook("http.response", (event) => {
      if (primaryGemini(event) && rejected(event.response.status))
        responses.set(event.sessionID, event.response.status)
    }, options)
    await session.hook("retry", async (event) => {
      if (event.agent !== "echoear") return
      const { sessionID, model } = event
      if (turns.has(sessionID) && model.providerID === providerID && model.id === fallback.id) {
        event.decision = { retry: false }
        return
      }
      if (!gemini(model) || responses.get(sessionID) !== event.error.status) return
      responses.delete(sessionID)
      if (!transient(event.error) && !unavailable(event.error)) return
      const rows = await history(event)
      const index = rows.findLastIndex(isTurnBoundary)
      const user = rows[index]
      if (user?.type !== "user" || !sources.has(user.metadata?.echoEarSource)) return
      event.decision = { retry: false }
      // A non-stream HTTP rejection cannot deliver a tool call. Any row after the
      // user means this may be a continuation or already have published output.
      if (event.attempt !== 2 || index !== rows.length - 1) return
      const turn = { id: user.id }
      turns.set(sessionID, turn)
      event.decision = { retry: true, delay: 0 }
      // OpenCode waits for the execution to settle, including cancellation.
      void session.wait({ sessionID }).catch(() => {}).finally(() => {
        if (turns.get(sessionID) === turn) turns.delete(sessionID)
      })
    }, options)
    await session.hook("experimental.model.resolve", async (event) => {
      const turn = turns.get(event.sessionID)
      if (!turn || !primaryGemini(event)) return
      const rows = await history(event)
      if (rows.findLast(isTurnBoundary)?.id === turn.id) event.model = { ...fallback }
    }, options)
  },
}
