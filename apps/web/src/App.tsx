import { HealthBadge } from './components/HealthBadge'

/**
 * P0 shell. The question box, trace timeline, SQL panel, result table, and
 * cost ledger land in P1 — this exists so the app is a real, running,
 * type-checked page rather than an empty directory.
 */
export function App() {
  return (
    <main>
      <h1>Sextant</h1>
      <p>
        Ask a database questions in plain English. Scaffolding only — the agent
        runtime is not wired up until P1.
      </p>
      <HealthBadge />
    </main>
  )
}
