import { render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { HealthBadge } from './HealthBadge'

const okResponse = { ok: true } as Response

describe('HealthBadge', () => {
  it('reports ok when the runtime answers', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(okResponse)
    render(<HealthBadge baseUrl="http://runtime.test" fetchImpl={fetchImpl} />)

    await waitFor(() => expect(screen.getByTestId('health-badge')).toHaveTextContent('ok'))
    expect(fetchImpl).toHaveBeenCalledWith('http://runtime.test/healthz', expect.anything())
  })

  it('reports unreachable when the request fails', async () => {
    const fetchImpl = vi.fn().mockRejectedValue(new Error('connection refused'))
    render(<HealthBadge baseUrl="http://runtime.test" fetchImpl={fetchImpl} />)

    await waitFor(() => expect(screen.getByTestId('health-badge')).toHaveTextContent('unreachable'))
  })

  it('reports unreachable on a non-2xx response', async () => {
    const fetchImpl = vi.fn().mockResolvedValue({ ok: false } as Response)
    render(<HealthBadge baseUrl="http://runtime.test" fetchImpl={fetchImpl} />)

    await waitFor(() => expect(screen.getByTestId('health-badge')).toHaveTextContent('unreachable'))
  })

  it('aborts the request on unmount', () => {
    // Otherwise a slow response resolves into a component that is gone, which
    // React warns about and which masks real teardown bugs.
    let signal: AbortSignal | undefined
    const fetchImpl = vi.fn((_url: string, init?: RequestInit) => {
      signal = init?.signal ?? undefined
      return new Promise<Response>(() => {})
    }) as unknown as typeof fetch

    const { unmount } = render(<HealthBadge baseUrl="http://runtime.test" fetchImpl={fetchImpl} />)
    expect(signal?.aborted).toBe(false)
    unmount()
    expect(signal?.aborted).toBe(true)
  })
})
