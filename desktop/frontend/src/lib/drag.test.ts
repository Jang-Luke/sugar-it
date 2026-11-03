import { describe, expect, it } from 'vitest'
import { APP_DRAG_TYPE, isAppDrag, staleDrag } from './drag'

describe('isAppDrag', () => {
  it('accepts a drag that carries the in-app type', () => {
    expect(isAppDrag(['text/plain', APP_DRAG_TYPE])).toBe(true)
  })
  it('ignores an Explorer file drag', () => {
    expect(isAppDrag(['Files'])).toBe(false)
  })
  it('ignores text and a missing list', () => {
    expect(isAppDrag(['text/plain'])).toBe(false)
    expect(isAppDrag(undefined)).toBe(false)
    expect(isAppDrag([])).toBe(false)
  })
})

describe('staleDrag', () => {
  it('a mouse move with no button down while an app drag is recorded means dragend was missed', () => {
    expect(staleDrag(true, 0)).toBe(true)
  })
  it('is not stale while the button is down or no drag is recorded', () => {
    expect(staleDrag(true, 1)).toBe(false)
    expect(staleDrag(false, 0)).toBe(false)
  })
})
