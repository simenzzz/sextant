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
 * origin is evil.example — and run paths come from server-supplied run ids.
 * The URL constructor resolves properly, and the leading-slash requirement
 * keeps a relative path from escaping the base.
 */
export function apiUrl(path: string, explicitBase?: string): string {
  if (!path.startsWith('/')) {
    throw new Error(`apiUrl: path must start with "/", got ${JSON.stringify(path)}`)
  }
  return new URL(path, apiBaseUrl(explicitBase)).toString()
}
