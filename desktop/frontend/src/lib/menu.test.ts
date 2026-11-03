import { describe, expect, it } from 'vitest'
import { actionStates, clampMenu, contextSelection } from './menu'
import type { Pane } from './panes'
import type { Entry } from './api'

const pane = (over: Partial<Pane> = {}): Pane => ({ id: 'left', peer: 'local', share: 'docs', path: '', ...over })
const entry = (name: string, type: Entry['Type'] = 'file'): Entry => ({
  Name: name, Path: '', Type: type, Size: 0, ModTime: '', Mode: 0, ETag: '',
})

describe('contextSelection', () => {
  const a = entry('a'), b = entry('b'), c = entry('c')
  it('selects only the row when it was not selected', () => {
    expect(contextSelection([a, b], c)).toEqual([c])
  })
  it('keeps the selection when the row is inside it', () => {
    expect(contextSelection([a, b], b)).toEqual([a, b])
  })
  it('keeps the selection on empty space', () => {
    expect(contextSelection([a, b], null)).toEqual([a, b])
  })
})

describe('actionStates', () => {
  const left = pane(), right = pane({ id: 'right', path: 'x' })
  const base = { src: left, dst: right, busy: false }
  it('enables everything for one selected folder', () => {
    const s = actionStates({ ...base, sel: [entry('d', 'dir')] })
    expect(s).toEqual({ open: true, copy: true, move: true, del: true, rename: true, mkdir: true })
  })
  it('open needs exactly one folder', () => {
    expect(actionStates({ ...base, sel: [entry('f')] }).open).toBe(false)
    expect(actionStates({ ...base, sel: [entry('d', 'dir'), entry('e', 'dir')] }).open).toBe(false)
  })
  it('rename needs exactly one entry', () => {
    expect(actionStates({ ...base, sel: [entry('a'), entry('b')] }).rename).toBe(false)
  })
  it('nothing on entries without a selection, but a folder can still be made', () => {
    expect(actionStates({ ...base, sel: [] })).toEqual({ open: false, copy: false, move: false, del: false, rename: false, mkdir: true })
  })
  it('copy and move need a usable other pane', () => {
    const s = actionStates({ ...base, dst: { ...left, id: 'right' }, sel: [entry('a')] })
    expect(s.copy).toBe(false)
    expect(s.move).toBe(false)
    expect(s.del).toBe(true)
  })
  it('everything is off while busy; no share, no new folder', () => {
    const s = actionStates({ ...base, busy: true, sel: [entry('a')] })
    expect(Object.values(s).some(Boolean)).toBe(false)
    expect(actionStates({ ...base, src: pane({ share: '' }), sel: [] }).mkdir).toBe(false)
  })
})

describe('clampMenu', () => {
  const size = { w: 200, h: 150 }, view = { w: 1000, h: 600 }
  it('keeps the point when the menu fits', () => {
    expect(clampMenu({ x: 100, y: 100 }, size, view)).toEqual({ x: 100, y: 100 })
  })
  it('shifts left and up from the right and bottom edges', () => {
    expect(clampMenu({ x: 950, y: 580 }, size, view)).toEqual({ x: 800, y: 450 })
  })
  it('never goes negative, even in a window smaller than the menu', () => {
    expect(clampMenu({ x: -5, y: -5 }, size, view)).toEqual({ x: 0, y: 0 })
    expect(clampMenu({ x: 50, y: 50 }, size, { w: 120, h: 90 })).toEqual({ x: 0, y: 0 })
  })
})
