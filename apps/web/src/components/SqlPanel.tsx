export interface SqlPanelProps {
  sql: string | null
  tables: readonly string[]
}

export function SqlPanel({ sql, tables }: SqlPanelProps) {
  if (!sql) return null

  return (
    <section aria-label="SQL">
      <h2>SQL</h2>
      {/*
        This shows the statement the guard cleared, not the raw generation.
        The two differ whenever the guard injected or clamped a LIMIT, and
        showing the generation would mean the panel and the database disagree
        about what ran. The reducer enforces which one lands here.
      */}
      <pre>
        <code>{sql}</code>
      </pre>
      {tables.length > 0 && <p>Tables in scope: {tables.join(', ')}</p>}
    </section>
  )
}
