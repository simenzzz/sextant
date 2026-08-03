import { useState, type FormEvent } from 'react'

export interface QuestionBoxProps {
  /** Called with a trimmed question and the chosen database. */
  onAsk: (question: string, database: string) => void
  onStop: () => void
  /** True while a run is in flight. */
  busy: boolean
  /** The databases the runtime is configured to answer against. */
  databases: readonly string[]
}

export function QuestionBox({ onAsk, onStop, busy, databases }: QuestionBoxProps) {
  const [question, setQuestion] = useState('')
  const [database, setDatabase] = useState(databases[0] ?? '')

  const submit = (e: FormEvent) => {
    e.preventDefault()
    // The reducer and the server both revalidate; this only stops an obviously
    // empty submission from becoming a round trip.
    if (!question.trim() || busy) return
    onAsk(question, database)
  }

  return (
    <form onSubmit={submit} aria-label="Ask a question">
      <label htmlFor="question">Question</label>
      <textarea
        id="question"
        name="question"
        value={question}
        onChange={(e) => setQuestion(e.target.value)}
        placeholder="How many cancelled orders are there?"
        rows={3}
        // Mirrors question_request.v1's maxLength, so the contract's limit is
        // visible in the UI rather than discovered as a rejection.
        maxLength={2000}
        disabled={busy}
      />

      <label htmlFor="database">Database</label>
      <select
        id="database"
        name="database"
        value={database}
        onChange={(e) => setDatabase(e.target.value)}
        disabled={busy || databases.length <= 1}
      >
        {databases.map((slug) => (
          <option key={slug} value={slug}>
            {slug}
          </option>
        ))}
      </select>

      <button type="submit" disabled={busy || !question.trim()}>
        Ask
      </button>
      {busy && (
        <button type="button" onClick={onStop}>
          Stop
        </button>
      )}
    </form>
  )
}
