/**
 * One place that decides where the agent runtime lives.
 *
 * Previously this logic existed twice with different semantics — one copy
 * treated an empty VITE_API_URL as unset, the other did not, so
 * `VITE_API_URL=""` sent one caller to the page origin and the other to
 * localhost. One function, one rule.
 */
export const DEFAULT_API_BASE_URL = 'http://localhost:8080'

/** Resolves the API base URL: explicit argument, then env, then default. */
export function apiBaseUrl(explicit?: string): string {
  if (explicit) return explicit
  const fromEnv = import.meta.env?.VITE_API_URL
  return typeof fromEnv === 'string' && fromEnv !== '' ? fromEnv : DEFAULT_API_BASE_URL
}

/**
 * Joins a server-supplied path onto the API base.
 *
 * Concatenation is not safe here: `${base}${path}` with a path like
 * `@evil.example/x` yields `http://localhost:8080@evil.example/x`, whose
 * origin is evil.example — and run paths come from server-supplied responses.
 *
 * Nor is the leading-slash rule sufficient on its own, which is what it used
 * to be. `new URL('//evil.example/x', 'http://localhost:8080')` resolves to
 * `http://evil.example/x`: a protocol-relative URL starts with a slash and
 * still escapes the base entirely. Verified, not assumed.
 *
 * So the check is on the RESULT, not the input. Whatever string arrives, the
 * resolved origin must be the base's — which no rule about leading characters
 * can guarantee, and one comparison can.
 */
export function apiUrl(path: string, explicitBase?: string): string {
  if (!path.startsWith('/')) {
    throw new Error(`apiUrl: path must start with "/", got ${JSON.stringify(path)}`)
  }

  const base = apiBaseUrl(explicitBase)
  const resolved = new URL(path, base)
  if (resolved.origin !== new URL(base).origin) {
    throw new Error(
      `apiUrl: path resolves to ${resolved.origin}, not the API origin ${new URL(base).origin}`,
    )
  }
  return resolved.toString()
}
