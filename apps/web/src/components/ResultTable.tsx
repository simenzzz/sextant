import type { ResultSetV1 } from '../lib/protocol'

export interface ResultTableProps {
  result: ResultSetV1 | null
}

/**
 * Renders one cell.
 *
 * NULL is rendered as a marked-up placeholder rather than an empty cell,
 * because an empty string and a NULL are different answers and a table that
 * shows them identically is a table that misreports the query.
 */
function Cell({ value }: { value: unknown }) {
  if (value === null || value === undefined) {
    return <span className="null">NULL</span>
  }
  if (typeof value === 'boolean') return <>{value ? 'true' : 'false'}</>
  return <>{String(value)}</>
}

export function ResultTable({ result }: ResultTableProps) {
  if (!result) return null

  return (
    <section aria-label="Result">
      <h2>Result</h2>

      {result.row_count === 0 ? (
        // An empty result is a real answer, and a distinct one from a failure.
        // Saying so beats rendering an empty table that looks like a bug.
        <p>The query ran and matched no rows.</p>
      ) : (
        <>
          <table>
            <thead>
              <tr>
                {result.columns.map((c) => (
                  <th key={c.name} scope="col">
                    {c.name}
                    {c.type ? <span className="type"> {c.type}</span> : null}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {result.rows.map((row, i) => (
                // Row position is the only identity a SQL result has — there
                // is no key in the data to use.
                <tr key={i}>
                  {row.map((cell, j) => (
                    <td key={j}>
                      <Cell value={cell} />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>

          <p>
            {result.truncated ? (
              // The frame hit its byte budget. Presenting a partial table as
              // complete would misreport the answer.
              <>
                {result.row_count} rows, showing the first {result.rows.length}
              </>
            ) : (
              <>{result.row_count} rows</>
            )}
            {result.elapsed_ms !== undefined ? <> · {result.elapsed_ms} ms</> : null}
          </p>
        </>
      )}
    </section>
  )
}
