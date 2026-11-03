import { describe, expect, it } from 'vitest'
import { EntryLoader, sortEntries } from './listing'
import type { Entry } from './api'

const e = (Name: string, Type: Entry['Type'] = 'file'): Entry => ({ Name, Path: Name, Type, Size: 0, ModTime: '', Mode: 0, ETag: '' })

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (r: unknown) => void
  const promise = new Promise<T>((a, b) => ((resolve = a), (reject = b)))
  return { promise, resolve, reject }
}

describe('EntryLoader', () => {
  it('clears rows and selection the moment a load starts', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('a'), e('b')])
    ld.selected = [ld.entries[0]]
    const d = deferred<Entry[]>()
    const p = ld.load(() => d.promise)
    expect(ld.entries).toEqual([])
    expect(ld.selected).toEqual([])
    expect(ld.loading).toBe(true)
    d.resolve([e('c')])
    await p
    expect(ld.entries.map((x) => x.Name)).toEqual(['c'])
    expect(ld.loading).toBe(false)
  })

  it('leaves no rows and no selection when the listing fails', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('a')])
    ld.selected = [ld.entries[0]]
    await ld.load(async () => {
      throw JSON.stringify({ code: 'peer_offline', message: 'down' })
    })
    expect(ld.entries).toEqual([])
    expect(ld.selected).toEqual([])
    expect(ld.error?.code).toBe('peer_offline')
    expect(ld.loading).toBe(false)
  })

  it('ignores an older slow reply after a newer navigation', async () => {
    const ld = new EntryLoader(() => {})
    const slow = deferred<Entry[]>()
    const p1 = ld.load(() => slow.promise)
    await ld.load(async () => [e('new')])
    slow.resolve([e('old')])
    await p1
    expect(ld.entries.map((x) => x.Name)).toEqual(['new'])
    const slowFail = deferred<Entry[]>()
    const p2 = ld.load(() => slowFail.promise)
    await ld.load(async () => [e('newer')])
    slowFail.reject('boom')
    await p2
    expect(ld.error).toBeNull()
    expect(ld.entries.map((x) => x.Name)).toEqual(['newer'])
  })

  it('invalidate drops rows and any reply in flight', async () => {
    const ld = new EntryLoader(() => {})
    const d = deferred<Entry[]>()
    const p = ld.load(() => d.promise)
    ld.invalidate()
    d.resolve([e('x')])
    await p
    expect(ld.entries).toEqual([])
  })
})

describe('sortEntries', () => {
  it('puts folders first, then sorts by Korean collation', () => {
    const out = sortEntries([e('나'), e('가', 'dir'), e('다'), e('나나', 'dir'), e('가')])
    expect(out.map((x) => x.Name)).toEqual(['가', '나나', '가', '나', '다'])
  })

  it('sorts 100,000 names quickly', () => {
    const names = Array.from({ length: 100_000 }, (_, i) => `파일 ${(i * 7919) % 100_000} 문서.txt`)
    const es = names.map((n, i) => e(n, i % 10 === 0 ? 'dir' : 'file'))
    const t0 = performance.now()
    const out = sortEntries(es)
    const ms = performance.now() - t0
    console.log(`sortEntries(100000) took ${ms.toFixed(0)} ms`)
    expect(out).toHaveLength(100_000)
    expect(out[0].Type).toBe('dir')
    expect(ms).toBeLessThan(2000)
  })
})
