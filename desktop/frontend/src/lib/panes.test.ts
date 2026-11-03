import { describe, expect, it } from 'vitest'
import { canDrop, targetOf, type Pane } from './panes'
import type { Entry } from './api'

const pane = (over: Partial<Pane> = {}): Pane => ({ id: 'left', peer: 'local', share: 'docs', path: '', ...over })
const entry = (name: string, type: Entry['Type'] = 'file'): Entry => ({
  Name: name, Path: '', Type: type, Size: 0, ModTime: '', Mode: 0, ETag: '',
})

describe('targetOf', () => {
  it('names the share root as a directory (trailing slash)', () => {
    expect(targetOf(pane())).toBe('local:docs/')
  })
  it('names a folder with a trailing slash', () => {
    expect(targetOf(pane({ path: 'a/b' }))).toBe('local:docs/a/b/')
  })
  it('names an entry in the share root', () => {
    expect(targetOf(pane(), entry('x.txt'))).toBe('local:docs/x.txt')
  })
  it('keeps spaces and Korean as written', () => {
    expect(targetOf(pane({ peer: 'nas', path: '내 문서/하위 폴더' }), entry('한글 파일 (1).txt')))
      .toBe('nas:docs/내 문서/하위 폴더/한글 파일 (1).txt')
  })
  it('does not double a slash and does not escape anything', () => {
    expect(targetOf(pane({ path: 'a#b%20c?d:e' }), entry('f'))).toBe('local:docs/a#b%20c?d:e/f')
  })
  it('is empty for a pane without a share', () => {
    expect(targetOf(pane({ share: '' }))).toBe('')
    expect(targetOf(pane({ share: '' }), entry('x'))).toBe('')
  })
})

describe('canDrop', () => {
  const left = pane({ id: 'left', peer: 'local', share: 'docs', path: 'a' })
  const right = pane({ id: 'right', peer: 'nas', share: 'media', path: '' })
  it('allows a drop on the other pane', () => {
    expect(canDrop(left, right)).toBe(true)
  })
  it('refuses the same pane', () => {
    expect(canDrop(left, left)).toBe(false)
    expect(canDrop(left, { ...left })).toBe(false)
  })
  it('refuses the same folder even in the other pane', () => {
    expect(canDrop(left, { ...left, id: 'right' })).toBe(false)
  })
  it('allows another folder of the same share', () => {
    expect(canDrop(left, { ...left, id: 'right', path: 'b' })).toBe(true)
  })
  it('refuses when a pane has no share selected', () => {
    expect(canDrop(left, { ...right, share: '' })).toBe(false)
    expect(canDrop({ ...left, share: '' }, right)).toBe(false)
  })
})
