import { describe, expect, it } from 'vitest'
import { EntryLoader, reconcileSelection, sameListing, sortEntries } from './listing'
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

describe('EntryLoader.refresh discards while a drag or dialog holds the rows', () => {
  it('discards a reply when apply() is false, then applies the next one', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('alpha', 'dir'), e('zeta', 'dir')])
    const before = ld.entries
    let hold = true
    const get = () => [] as Entry[]
    const r1 = await ld.refresh(async () => [e('aaa-new', 'dir'), e('alpha', 'dir'), e('zeta', 'dir')], get, () => true, () => !hold)
    expect(r1).toBe('discarded')
    expect(ld.entries).toBe(before)
    hold = false
    const r2 = await ld.refresh(async () => [e('aaa-new', 'dir'), e('alpha', 'dir'), e('zeta', 'dir')], get, () => true, () => !hold)
    expect(r2).toBe('replaced')
    expect(ld.entries.map((x) => x.Name)).toEqual(['aaa-new', 'alpha', 'zeta'])
  })

  it('checks apply() when the reply arrives, not when the request starts', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('alpha', 'dir')])
    const d = deferred<Entry[]>()
    let dragging = false
    const p = ld.refresh(() => d.promise, () => [], () => true, () => !dragging)
    dragging = true // a drag starts while the request is in flight
    d.resolve([e('aaa-new', 'dir'), e('alpha', 'dir')])
    expect(await p).toBe('discarded')
    expect(ld.entries.map((x) => x.Name)).toEqual(['alpha'])
  })

  it('does not record a failure while discarding', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('a')])
    const r = await ld.refresh(async () => { throw 'boom' }, () => [], () => true, () => false)
    expect(r).toBe('discarded')
    expect(ld.bgError).toBeNull()
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

describe('EntryLoader.refresh (background)', () => {
  const names = (ld: EntryLoader) => ld.entries.map((x) => x.Name)

  it('replaces rows without blanking or a loading state when the listing changed', async () => {
    const seen: boolean[] = []
    const ld = new EntryLoader(() => seen.push(ld.loading))
    await ld.load(async () => [e('a')])
    seen.length = 0
    const d = deferred<Entry[]>()
    const p = ld.refresh(() => d.promise, () => [])
    expect(names(ld)).toEqual(['a'])
    expect(ld.loading).toBe(false)
    d.resolve([e('a'), e('b')])
    expect(await p).toBe('replaced')
    expect(names(ld)).toEqual(['a', 'b'])
    expect(seen.every((l) => l === false)).toBe(true)
  })

  it('keeps the same rows and does not notify when nothing changed', async () => {
    let n = 0
    const ld = new EntryLoader(() => n++)
    await ld.load(async () => [e('a'), e('b')])
    const rows = ld.entries
    n = 0
    expect(await ld.refresh(async () => [e('b'), e('a')], () => [])).toBe('same')
    expect(ld.entries).toBe(rows)
    expect(n).toBe(0)
  })

  it('keeps the selection by name and drops names that vanished', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('a'), e('b'), e('c')])
    const picked = [ld.entries[0], ld.entries[2]]
    await ld.refresh(async () => [e('a'), { ...e('c'), Size: 5 }, e('d')], () => picked)
    expect(ld.selected.map((x) => x.Name).sort()).toEqual(['a', 'c'])
    // the selection holds the new row objects, so identity checks in the view still work
    for (const s of ld.selected) expect(ld.entries).toContain(s)
    await ld.refresh(async () => [e('d')], () => ld.selected)
    expect(ld.selected).toEqual([])
  })

  it('reads the selection when the reply arrives, not when it was requested', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('a'), e('b')])
    const d = deferred<Entry[]>()
    let sel: Entry[] = []
    const p = ld.refresh(() => d.promise, () => sel)
    sel = [ld.entries[1]]
    d.resolve([e('a'), e('b'), e('c')])
    await p
    expect(ld.selected.map((x) => x.Name)).toEqual(['b'])
  })

  it('discards a reply requested before a navigation, and never resurrects old rows', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('old')])
    const d = deferred<Entry[]>()
    const p = ld.refresh(() => d.promise, () => [])
    const nav = deferred<Entry[]>()
    const q = ld.load(() => nav.promise) // navigation starts: rows cleared
    d.resolve([e('old'), e('older')])
    expect(await p).toBe('discarded')
    expect(ld.entries).toEqual([])
    nav.resolve([e('new')])
    await q
    expect(names(ld)).toEqual(['new'])
  })

  it('discards a reply after invalidate', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('old')])
    const d = deferred<Entry[]>()
    const p = ld.refresh(() => d.promise, () => [])
    ld.invalidate()
    d.resolve([e('x')])
    expect(await p).toBe('discarded')
    expect(ld.entries).toEqual([])
  })

  it('discards a reply when the caller says the pane is no longer the same folder', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('a')])
    expect(await ld.refresh(async () => [e('z')], () => [], () => false)).toBe('discarded')
    expect(names(ld)).toEqual(['a'])
  })

  it('discards a failure that arrives after a navigation', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('a')])
    const d = deferred<Entry[]>()
    const p = ld.refresh(() => d.promise, () => [])
    await ld.load(async () => [e('b')])
    d.reject('boom')
    expect(await p).toBe('discarded')
    expect(ld.bgError).toBeNull()
    expect(ld.error).toBeNull()
  })

  it('does not start while a navigation load is under way', async () => {
    const ld = new EntryLoader(() => {})
    const nav = deferred<Entry[]>()
    const q = ld.load(() => nav.promise)
    let called = false
    expect(await ld.refresh(async () => ((called = true), [e('x')]), () => [])).toBe('discarded')
    expect(called).toBe(false)
    nav.resolve([e('n')])
    await q
  })

  it('a background failure keeps rows and selection, and reports the error', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('a'), e('b')])
    ld.selected = [ld.entries[0]]
    const r = await ld.refresh(async () => { throw JSON.stringify({ code: 'peer_offline', message: 'down' }) }, () => ld.selected)
    expect(r).toBe('failed')
    // the view owns the live selection; a notification must not overwrite it with a stale one
    let notified: Entry[] = []
    const ld2 = new EntryLoader(() => (notified = ld2.selected))
    await ld2.load(async () => [e('a'), e('b')])
    const live = [ld2.entries[1]]
    await ld2.refresh(async () => { throw 'x' }, () => live)
    expect(notified).toEqual(live)
    await ld2.refresh(async () => [e('a'), e('b')], () => live)
    expect(notified).toEqual(live)
    expect(names(ld)).toEqual(['a', 'b'])
    expect(ld.selected).toHaveLength(1)
    expect(ld.bgError?.code).toBe('peer_offline')
    expect(ld.error).toBeNull()
    // the next good reply clears it
    await ld.refresh(async () => [e('a'), e('b')], () => ld.selected)
    expect(ld.bgError).toBeNull()
  })

  it('a navigation failure still clears rows', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [e('a')])
    await ld.refresh(async () => { throw 'x' }, () => [])
    await ld.load(async () => { throw JSON.stringify({ code: 'peer_offline', message: '' }) })
    expect(ld.entries).toEqual([])
    expect(ld.error?.code).toBe('peer_offline')
    expect(ld.bgError).toBeNull()
  })

  it('a good background reply recovers from a navigation error for the same folder', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => { throw JSON.stringify({ code: 'peer_offline', message: '' }) })
    expect(await ld.refresh(async () => [e('a')], () => [])).toBe('replaced')
    expect(ld.error).toBeNull()
    expect(names(ld)).toEqual(['a'])
  })
})

describe('sameListing and reconcileSelection', () => {
  it('detects any visible difference', () => {
    const a = e('a')
    expect(sameListing([a], [{ ...a }])).toBe(true)
    expect(sameListing([a], [{ ...a, Size: 1 }])).toBe(false)
    expect(sameListing([a], [{ ...a, ModTime: 'x' }])).toBe(false)
    expect(sameListing([a], [{ ...a, ETag: 'x' }])).toBe(false)
    expect(sameListing([a], [{ ...a, Type: 'dir' }])).toBe(false)
    expect(sameListing([a], [a, e('b')])).toBe(false)
  })
  it('keeps order of the selection and ignores duplicates', () => {
    const rows = [e('a'), e('b'), e('c')]
    expect(reconcileSelection([e('c'), e('a'), e('gone')], rows).map((x) => x.Name)).toEqual(['c', 'a'])
  })
})

describe('reconcileSelection when an entry changes type', () => {
  const f = (Name: string, Type: Entry['Type'], Size = 0, ETag = ''): Entry => ({ ...e(Name, Type), Size, ETag })
  it('drops a selected file that another host replaced with a folder of the same name', () => {
    expect(reconcileSelection([f('report', 'file')], [f('report', 'dir')])).toEqual([])
  })
  it('drops a selected folder replaced by a file', () => {
    expect(reconcileSelection([f('x', 'dir')], [f('x', 'file')])).toEqual([])
  })
  it('keeps an entry whose size or ETag changed but whose type did not', () => {
    const now = f('a', 'file', 9, 'new')
    expect(reconcileSelection([f('a', 'file', 1, 'old')], [now])).toEqual([now])
  })
  it('refresh: a type change empties the selection', async () => {
    const ld = new EntryLoader(() => {})
    await ld.load(async () => [f('report', 'file')])
    const sel = [ld.entries[0]]
    await ld.refresh(async () => [f('report', 'dir')], () => sel)
    expect(ld.entries[0].Type).toBe('dir')
    expect(ld.selected).toEqual([])
  })
})
