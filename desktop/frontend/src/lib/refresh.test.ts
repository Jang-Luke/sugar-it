import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AutoRefresher, jobSignatures, jobsChanged, nextDelay, pollAction, touchesPane } from './refresh'
import type { Job } from './api'
import type { Pane } from './panes'

describe('nextDelay', () => {
  it('is 3 s for fast listings', () => {
    expect(nextDelay(0)).toBe(3000)
    expect(nextDelay(299)).toBe(3000)
  })
  it('is ten times a slow listing', () => {
    expect(nextDelay(300)).toBe(3000)
    expect(nextDelay(2000)).toBe(20000)
  })
})

describe('AutoRefresher', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  const make = (opts: { dur?: () => number; can?: () => boolean } = {}) => {
    const calls: number[] = []
    const t0 = Date.now()
    let running = 0, maxRunning = 0
    const r = new AutoRefresher({
      run: async () => {
        calls.push(Date.now() - t0)
        running++
        maxRunning = Math.max(maxRunning, running)
        await new Promise((res) => setTimeout(res, opts.dur?.() ?? 0))
        running--
      },
      canRun: opts.can ?? (() => true),
    })
    return { r, calls, max: () => maxRunning }
  }

  it('polls every 3 s while started', async () => {
    const { r, calls } = make()
    r.start()
    await vi.advanceTimersByTimeAsync(3000)
    expect(calls).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(3100)
    expect(calls).toHaveLength(2)
    r.stop()
    await vi.advanceTimersByTimeAsync(10000)
    expect(calls).toHaveLength(2)
  })

  it('does not run while it cannot, and runs soon after it can', async () => {
    let ok = false
    const { r, calls } = make({ can: () => ok })
    r.start()
    await vi.advanceTimersByTimeAsync(10000)
    expect(calls).toHaveLength(0)
    ok = true
    await vi.advanceTimersByTimeAsync(1000)
    expect(calls).toHaveLength(1)
    r.stop()
  })

  it('never overlaps and backs off to ten times a slow listing', async () => {
    const { r, calls, max } = make({ dur: () => 2000 })
    r.start()
    await vi.advanceTimersByTimeAsync(3000) // starts at 3000, ends 5000, next at 25000
    expect(calls).toEqual([3000])
    await vi.advanceTimersByTimeAsync(19000) // t = 22000
    expect(calls).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(3000) // t = 25000
    expect(calls).toHaveLength(2)
    expect(max()).toBe(1)
    r.stop()
  })

  it('request() runs soon for a fast listing and is coalesced while one is in flight', async () => {
    const { r, calls } = make({ dur: () => 100 })
    r.start()
    await vi.advanceTimersByTimeAsync(3000)
    expect(calls).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(200) // ended at 3100
    r.request()
    r.request()
    await vi.advanceTimersByTimeAsync(1500) // 10 x 100 ms after the end: due 4100
    expect(calls).toHaveLength(2)
    r.stop()
  })

  it('request() during a run reruns once afterwards, not at once twice', async () => {
    const { r, calls, max } = make({ dur: () => 100 })
    r.start()
    await vi.advanceTimersByTimeAsync(3050) // in flight
    r.request()
    r.request()
    await vi.advanceTimersByTimeAsync(2000)
    expect(calls).toHaveLength(2)
    expect(max()).toBe(1)
    r.stop()
  })

  it('request() on a slow listing is held back by the same backoff', async () => {
    const { r, calls } = make({ dur: () => 2000 })
    r.start()
    await vi.advanceTimersByTimeAsync(5000) // one run, ended at 5000
    r.request()
    await vi.advanceTimersByTimeAsync(10000) // 15000 < 25000
    expect(calls).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(10000)
    expect(calls).toHaveLength(2)
    r.stop()
  })

  it('a failing run does not stop the polling', async () => {
    let n = 0
    const r = new AutoRefresher({ run: async () => { n++; throw new Error('x') }, canRun: () => true })
    r.start()
    await vi.advanceTimersByTimeAsync(7000)
    expect(n).toBe(2)
    r.stop()
  })
})

const pane = (over: Partial<Pane> = {}): Pane => ({ id: 'left', peer: 'local', share: 'docs', path: '', ...over })
const job = (id: string, over: Partial<Job> = {}, src = 'SELF', dst = 'NAS'): Job => ({
  ID: id, Host: 'h', Origin: 'h',
  Spec: { Op: 'copy', Src: { Peer: src, Share: 'docs', Path: 'a' }, Dst: { Peer: dst, Share: 'media', Path: '' }, OnConflict: 'fail' },
  State: 'running', Error: null, TotalFiles: 10, DoneFiles: 1, SkippedFiles: 0, FailedFiles: 0,
  TotalBytes: 100, DoneBytes: 10, BytesPerSec: 0, Failures: null, Skipped: null, Created: '', Updated: '', ...over,
})
const peers = [{ ID: 'NAS', Name: 'nas' }]

describe('touchesPane', () => {
  it('matches source or destination by peer id and share', () => {
    expect(touchesPane(job('1'), pane(), 'SELF', peers)).toBe(true) // source: local docs
    expect(touchesPane(job('1'), pane({ peer: 'nas', share: 'media' }), 'SELF', peers)).toBe(true) // destination
  })
  it('does not match another share or peer', () => {
    expect(touchesPane(job('1'), pane({ share: 'other' }), 'SELF', peers)).toBe(false)
    expect(touchesPane(job('1'), pane({ peer: 'nas', share: 'docs' }), 'SELF', peers)).toBe(false)
    expect(touchesPane(job('1'), pane({ peer: 'ghost' }), 'SELF', peers)).toBe(false)
  })
  it('a pane without a share touches nothing', () => {
    expect(touchesPane(job('1'), pane({ share: '' }), 'SELF', peers)).toBe(false)
  })
})

describe('jobsChanged', () => {
  const shown = [pane(), pane({ id: 'right', peer: 'nas', share: 'media' })]
  const check = (prev: Map<string, string>, jobs: Job[]) => jobsChanged(prev, jobs, shown, 'SELF', peers)

  it('reports a job first seen, even one that is already done (finished within one poll)', () => {
    const r = check(new Map(), [job('1', { State: 'done', DoneFiles: 10 })])
    expect(r.changed).toBe(true)
  })
  it('is quiet when nothing moved', () => {
    const jobs = [job('1')]
    const first = check(new Map(), jobs)
    expect(check(first.next, jobs).changed).toBe(false)
  })
  it('reports progress by file count and state changes', () => {
    const first = check(new Map(), [job('1')])
    expect(check(first.next, [job('1', { DoneFiles: 2 })]).changed).toBe(true)
    expect(check(first.next, [job('1', { State: 'waiting_peer' })]).changed).toBe(true)
    expect(check(first.next, [job('1', { State: 'failed', FailedFiles: 1 })]).changed).toBe(true)
  })
  it('ignores byte progress inside a file, which cannot change a listing', () => {
    const first = check(new Map(), [job('1')])
    expect(check(first.next, [job('1', { DoneBytes: 50, BytesPerSec: 9 })]).changed).toBe(false)
  })
  it('reports a job that disappeared from the list', () => {
    const first = check(new Map(), [job('1')])
    expect(check(first.next, []).changed).toBe(true)
  })
  it('ignores jobs that touch nothing shown', () => {
    const other = job('2', {}, 'X', 'Y')
    expect(check(new Map(), [other]).changed).toBe(false)
  })
  it('keys by host and id so equal ids on two hosts stay apart', () => {
    const sig = jobSignatures([job('1', { Host: 'a' }), job('1', { Host: 'b' })], () => true)
    expect(sig.size).toBe(2)
  })
})

describe('manual refresh resets the backoff', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('polls again after 3 s, not ten times the slow listing, once reset', async () => {
    const calls: number[] = []
    const t0 = Date.now()
    const r = new AutoRefresher({
      run: async () => {
        calls.push(Date.now() - t0)
        await new Promise((res) => setTimeout(res, 2000))
      },
      canRun: () => true,
    })
    r.start()
    await vi.advanceTimersByTimeAsync(3000) // runs 3000..5000
    await vi.advanceTimersByTimeAsync(2100) // t = 5100, next would be 25000
    r.resetBackoff()
    r.request()
    await vi.advanceTimersByTimeAsync(100)
    expect(calls).toEqual([3000, 5100])
    r.stop()
  })

  it('the regular delay after a run that follows a reset is based on that run only', async () => {
    let dur = 2000
    const calls: number[] = []
    const t0 = Date.now()
    const r = new AutoRefresher({
      run: async () => {
        calls.push(Date.now() - t0)
        await new Promise((res) => setTimeout(res, dur))
      },
      canRun: () => true,
    })
    r.start()
    await vi.advanceTimersByTimeAsync(3000)
    await vi.advanceTimersByTimeAsync(2100)
    dur = 10
    r.resetBackoff()
    r.request()
    await vi.advanceTimersByTimeAsync(3200) // ran at 5100, ends 5110, next at 8110
    expect(calls).toEqual([3000, 5100, 8110].slice(0, calls.length))
    expect(calls.length).toBe(3)
    r.stop()
  })
})

describe('pollAction', () => {
  it('lists the shares again in the background when the share listing failed', () => {
    expect(pollAction({ shareCount: 0, sharesFailed: true, share: '' })).toBe('shares')
  })
  it('does nothing for a host that simply has no shares', () => {
    expect(pollAction({ shareCount: 0, sharesFailed: false, share: '' })).toBe('none')
  })
  it('does nothing while no share is chosen', () => {
    expect(pollAction({ shareCount: 2, sharesFailed: false, share: '' })).toBe('none')
  })
  it('reloads the folder otherwise', () => {
    expect(pollAction({ shareCount: 2, sharesFailed: false, share: 's' })).toBe('entries')
  })
})
