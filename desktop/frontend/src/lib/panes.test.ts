import { describe, expect, it } from 'vitest'
import { canDrop, dropOperation, planDrop, snapshotSelection, targetOf, type Pane } from './panes'
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

describe('snapshotSelection', () => {
  it('copies the pane and the entries so later changes do not reach it', () => {
    const p = pane({ path: 'a' })
    const sel = [entry('x'), entry('y', 'dir')]
    const snap = snapshotSelection(p, sel)
    p.path = 'b'
    p.share = 'other'
    sel.length = 0
    expect(snap.pane).toMatchObject({ peer: 'local', share: 'docs', path: 'a' })
    expect(snap.entries.map((e) => e.Name)).toEqual(['x', 'y'])
  })
})

describe('dropOperation', () => {
  const a = pane({ id: 'left', path: 'a' })
  const sameShare = pane({ id: 'right', path: 'b' })
  const otherShare = pane({ id: 'right', share: 'media' })
  const otherPeer = pane({ id: 'right', peer: 'nas' })
  const none = { shift: false, ctrl: false }
  it('moves within the same peer and share, copies otherwise', () => {
    expect(dropOperation(a, sameShare, none)).toBe('move')
    expect(dropOperation(a, otherShare, none)).toBe('copy')
    expect(dropOperation(a, otherPeer, none)).toBe('copy')
  })
  it('Shift always moves', () => {
    expect(dropOperation(a, otherPeer, { shift: true, ctrl: false })).toBe('move')
    expect(dropOperation(a, otherShare, { shift: true, ctrl: false })).toBe('move')
  })
  it('Ctrl always copies', () => {
    expect(dropOperation(a, sameShare, { shift: false, ctrl: true })).toBe('copy')
  })
  it('copies when both are held, the choice that destroys nothing', () => {
    expect(dropOperation(a, sameShare, { shift: true, ctrl: true })).toBe('copy')
  })
})

describe('planDrop', () => {
  const src = pane({ id: 'left', path: 'a' })
  const other = pane({ id: 'right', path: 'b' })
  const folder = (n: string) => entry(n, 'dir')

  it('targets the other pane folder with a trailing slash', () => {
    const r = planDrop(src, [entry('x')], other)
    expect(r).toMatchObject({ ok: true, target: 'local:docs/b/' })
  })
  it('targets the share root with a trailing slash', () => {
    const r = planDrop(src, [entry('x')], pane({ id: 'right' }))
    expect(r).toMatchObject({ ok: true, target: 'local:docs/' })
  })
  it('targets a folder row, not the pane folder', () => {
    const r = planDrop(src, [entry('x')], other, folder('sub'))
    expect(r).toMatchObject({ ok: true, target: 'local:docs/b/sub/' })
    if (r.ok) expect(r.dest).toMatchObject({ peer: 'local', share: 'docs', path: 'b/sub' })
  })
  it('a folder row in the root of a share', () => {
    const r = planDrop(src, [entry('x')], pane({ id: 'right' }), folder('한 글'))
    expect(r).toMatchObject({ ok: true, target: 'local:docs/한 글/' })
  })
  it('allows a folder row in the same pane', () => {
    const r = planDrop(src, [entry('x')], { ...src }, folder('sub'))
    expect(r).toMatchObject({ ok: true, target: 'local:docs/a/sub/' })
  })
  it('refuses dropping a folder onto itself', () => {
    expect(planDrop(src, [folder('sub')], { ...src }, folder('sub'))).toEqual({ ok: false, reason: 'self' })
  })
  it('refuses one of several dragged entries as the target', () => {
    expect(planDrop(src, [entry('x'), folder('sub')], { ...src }, folder('sub'))).toEqual({ ok: false, reason: 'self' })
  })
  it('refuses a target inside a dragged folder, seen from the other pane', () => {
    const inside = pane({ id: 'right', path: 'a/sub/deep' })
    expect(planDrop(src, [folder('sub')], inside, folder('leaf'))).toEqual({ ok: false, reason: 'self' })
    expect(planDrop(src, [folder('sub')], inside)).toEqual({ ok: false, reason: 'self' })
  })
  it('does not mistake a name prefix for containment', () => {
    const r = planDrop(src, [folder('sub')], pane({ id: 'right', path: 'a/sub2' }))
    expect(r.ok).toBe(true)
  })
  it('does not call another share or peer self', () => {
    expect(planDrop(src, [folder('sub')], pane({ id: 'right', share: 'media', path: 'a/sub' })).ok).toBe(true)
    expect(planDrop(src, [folder('sub')], pane({ id: 'right', peer: 'nas', path: 'a/sub' })).ok).toBe(true)
  })
  it('refuses a no-op: entries already directly in the target folder', () => {
    expect(planDrop(src, [entry('x')], { ...src })).toEqual({ ok: false, reason: 'noop' })
    expect(planDrop(src, [entry('x')], { ...src, id: 'right' })).toEqual({ ok: false, reason: 'noop' })
    expect(planDrop(src, [entry('x')], { ...src, id: 'right' }, entry('file.txt'))).toEqual({ ok: false, reason: 'noop' })
  })
  it('a non-folder row means the pane folder', () => {
    const r = planDrop(src, [entry('x')], other, entry('file.txt'))
    expect(r).toMatchObject({ ok: true, target: 'local:docs/b/' })
  })
  it('refuses when a pane has no share', () => {
    expect(planDrop({ ...src, share: '' }, [entry('x')], other)).toEqual({ ok: false, reason: 'noShare' })
    expect(planDrop(src, [entry('x')], { ...other, share: '' })).toEqual({ ok: false, reason: 'noShare' })
  })
  it('refuses an empty drag', () => {
    expect(planDrop(src, [], other)).toEqual({ ok: false, reason: 'empty' })
  })
  it('returns a destination that later pane changes cannot reach', () => {
    const dst = pane({ id: 'right', path: 'b' })
    const r = planDrop(src, [entry('x')], dst, folder('sub'))
    dst.path = 'zzz'
    if (r.ok) expect(r.dest.path).toBe('b/sub')
  })
})
