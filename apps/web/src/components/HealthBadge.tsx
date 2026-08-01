import { useEffect, useState } from 'react'

import { apiUrl } from '../lib/apiUrl'

type Health = 'checking' | 'ok' | 'unreachable'

export interface HealthBadgeProps {
  baseUrl?: string
  /** Test seam: inject a fetch rather than patching the global. */
  fetchImpl?: typeof fetch
}

/**
 * Reports whether the agent runtime is reachable.
 *
 * Small, but it proves the whole chain end to end at P0: the page builds, the
 * browser reaches the Go service, and the origin allowlist actually permits
 * this origin. A green badge is the first thing that breaks if any of those
 * regress — which is why SEXTANT_ALLOWED_ORIGINS must list the web origin in
 * infra/docker-compose.yml.
 */
export function HealthBadge({ baseUrl, fetchImpl }: HealthBadgeProps) {
  const [health, setHealth] = useState<Health>('checking')

  useEffect(() => {
    const doFetch = fetchImpl ?? globalThis.fetch
    if (typeof doFetch !== 'function') {
      setHealth('unreachable')
      return
    }

    // Abort on unmount so a slow response cannot set state on a component
    // that is gone.
    const controller = new AbortController()
    const url = apiUrl('/healthz', baseUrl)

    doFetch(url, { signal: controller.signal })
      .then((res) => setHealth(res.ok ? 'ok' : 'unreachable'))
      .catch(() => {
        if (!controller.signal.aborted) setHealth('unreachable')
      })

    return () => controller.abort()
  }, [baseUrl, fetchImpl])

  return (
    <p data-testid="health-badge">
      Agent runtime: <strong>{health}</strong>
    </p>
  )
}
