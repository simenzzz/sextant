import { describe, expect, it } from 'vitest'

import { DEFAULT_API_BASE_URL, apiBaseUrl, apiUrl } from './apiUrl'

describe('apiBaseUrl', () => {
  it('prefers an explicit base', () => {
    expect(apiBaseUrl('http://runtime.test')).toBe('http://runtime.test')
  })

  it('falls back to the default when nothing is configured', () => {
    expect(apiBaseUrl()).toBe(DEFAULT_API_BASE_URL)
  })
})

describe('apiUrl', () => {
  it('joins a path onto the base', () => {
    expect(apiUrl('/healthz', 'http://runtime.test')).toBe('http://runtime.test/healthz')
  })

  /**
   * Concatenation would make this `http://runtime.test@evil.example/x`, whose
   * origin is evil.example. Run paths come from server-supplied run ids, so
   * this is reachable rather than theoretical.
   */
  it('refuses a path that does not start with a slash', () => {
    expect(() => apiUrl('@evil.example/x', 'http://runtime.test')).toThrow(/must start with/)
    expect(() => apiUrl('runs/1', 'http://runtime.test')).toThrow(/must start with/)
  })

  it('does not let a path escape the base origin', () => {
    expect(new URL(apiUrl('/runs/../../x', 'http://runtime.test')).origin).toBe(
      'http://runtime.test',
    )
  })
})

describe('origin containment', () => {
  it('refuses a protocol-relative path that escapes the base', () => {
    // The leading-slash rule alone does NOT prevent this: new URL() resolves
    // '//evil.example/x' against any base to http://evil.example/x. Run paths
    // come from a server response, so the check has to be on the result.
    expect(() => apiUrl('//evil.example/x', 'http://localhost:8080')).toThrow(/origin/)
    expect(() => apiUrl('//evil.example', 'http://localhost:8080')).toThrow(/origin/)
    expect(() => apiUrl('///evil.example/x', 'http://localhost:8080')).toThrow(/origin/)
  })

  it('still accepts ordinary run paths', () => {
    expect(apiUrl('/v1/runs/r_abc/events', 'http://localhost:8080')).toBe(
      'http://localhost:8080/v1/runs/r_abc/events',
    )
  })

  it('refuses a path that is not rooted', () => {
    expect(() => apiUrl('v1/runs/x', 'http://localhost:8080')).toThrow(/must start with/)
    expect(() => apiUrl('@evil.example/x', 'http://localhost:8080')).toThrow(/must start with/)
  })
})
