import { describe, expect, it } from 'vitest'

import { formatBytes } from '../formatters'

describe('formatBytes', () => {
  it('uses progressive binary units', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(1024)).toBe('1 KB')
    expect(formatBytes(1024 * 1024)).toBe('1 MB')
    expect(formatBytes(1024 * 1024 * 1024)).toBe('1 GB')
  })

  it('keeps useful precision while promoting units', () => {
    expect(formatBytes(1536)).toBe('1.5 KB')
    expect(formatBytes(10 * 1024 * 1024)).toBe('10 MB')
    expect(formatBytes(1024 * 1024 + 512 * 1024)).toBe('1.5 MB')
  })
})
