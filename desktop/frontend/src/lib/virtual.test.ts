import { describe, expect, it } from 'vitest'
import { windowRange } from './virtual'

describe('windowRange', () => {
  const N = 100_000
  const ROW = 28

  it('renders only a screenful out of 100,000 rows at the top', () => {
    const w = windowRange({ scrollTop: 0, viewport: 600, rowHeight: ROW, total: N, overscan: 5 })
    expect(w.start).toBe(0)
    expect(w.end).toBeLessThan(40)
    expect(w.totalHeight).toBe(N * ROW)
    expect(w.offsetTop).toBe(0)
  })

  it('stays small and consistent in the middle', () => {
    const top = 50_000 * ROW + 10
    const w = windowRange({ scrollTop: top, viewport: 600, rowHeight: ROW, total: N, overscan: 5 })
    expect(w.start).toBe(50_000 - 5)
    expect(w.end - w.start).toBeLessThan(40)
    expect(w.offsetTop).toBe(w.start * ROW)
    // every visible row is inside the window
    expect(w.start * ROW).toBeLessThanOrEqual(top)
    expect(w.end * ROW).toBeGreaterThanOrEqual(top + 600)
  })

  it('clamps at the bottom', () => {
    const w = windowRange({ scrollTop: N * ROW, viewport: 600, rowHeight: ROW, total: N, overscan: 5 })
    expect(w.end).toBe(N)
    expect(w.start).toBeGreaterThanOrEqual(N - 40)
  })

  it('covers every row while scrolling through all 100,000 with a bounded window', () => {
    let covered = 0
    let maxWindow = 0
    const t0 = performance.now()
    for (let top = 0; top < N * ROW; top += 500) {
      const w = windowRange({ scrollTop: top, viewport: 600, rowHeight: ROW, total: N, overscan: 3 })
      maxWindow = Math.max(maxWindow, w.end - w.start)
      covered = Math.max(covered, w.end)
      expect(Math.floor(top / ROW)).toBeGreaterThanOrEqual(w.start)
      expect(Math.floor(top / ROW)).toBeLessThan(w.end)
    }
    expect(covered).toBe(N)
    expect(maxWindow).toBeLessThan(40)
    expect(performance.now() - t0).toBeLessThan(1000)
  })

  it('handles empty lists, short lists and nonsense input', () => {
    expect(windowRange({ scrollTop: 0, viewport: 600, rowHeight: ROW, total: 0, overscan: 5 }))
      .toEqual({ start: 0, end: 0, offsetTop: 0, totalHeight: 0 })
    const few = windowRange({ scrollTop: 0, viewport: 600, rowHeight: ROW, total: 3, overscan: 5 })
    expect([few.start, few.end]).toEqual([0, 3])
    const neg = windowRange({ scrollTop: -50, viewport: 0, rowHeight: ROW, total: 10, overscan: 2 })
    expect(neg.start).toBe(0)
    expect(neg.end).toBeLessThanOrEqual(10)
  })
})
