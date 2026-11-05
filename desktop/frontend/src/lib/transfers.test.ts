import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { errorText, failedCount, jobFailureWatcher, percent, pollJobs } from './transfers'
import { ko } from '../ko'
import type { Api, Job } from './api'

const SPEC_CODES = [
  'not_found', 'exists', 'not_empty', 'read_only', 'invalid_path', 'invalid_name', 'invalid_request',
  'share_not_found', 'share_overlap', 'peer_not_found', 'peer_offline', 'peer_unauthorized',
  'peer_incompatible', 'changed', 'disk_full', 'hash_mismatch', 'conflict', 'cross_host_rename',
  'canceled', 'internal',
]

describe('errorText', () => {
  it('has Korean text for all 20 spec codes', () => {
    expect(SPEC_CODES).toHaveLength(20)
    const seen = new Set<string>()
    for (const c of SPEC_CODES) {
      const t = errorText(c)
      expect(t, c).toMatch(/[가-힣]/)
      expect(t, c).not.toBe(ko.errors.unknown)
      seen.add(t)
    }
    expect(seen.size).toBe(20)
  })
  it('does not carry more codes than the spec', () => {
    expect(Object.keys(ko.errors).filter((k) => k !== 'unknown').sort()).toEqual([...SPEC_CODES].sort())
  })
  it('falls back to a generic Korean text', () => {
    expect(errorText('no_such_code')).toBe(ko.errors.unknown)
    expect(errorText('')).toBe(ko.errors.unknown)
    expect(errorText('constructor')).toBe(ko.errors.unknown)
    expect(errorText('__proto__')).toBe(ko.errors.unknown)
  })
})

const job = (over: Partial<Job> = {}): Job => ({
  ID: 'j1', Host: 'h', Origin: 'o',
  Spec: { Op: 'copy', Src: { Peer: 'a', Share: 's', Path: 'x' }, Dst: { Peer: 'b', Share: 's', Path: '' }, OnConflict: 'fail' },
  State: 'running', Error: null,
  TotalFiles: 1, DoneFiles: 0, SkippedFiles: 0, FailedFiles: 0, TotalBytes: 0, DoneBytes: 0, BytesPerSec: 0,
  Failures: null, Skipped: null, Created: '', Updated: '', ...over,
})

describe('progress helpers', () => {
  it('computes percent from DoneBytes and TotalBytes, never above 100', () => {
    expect(percent(job({ TotalBytes: 200, DoneBytes: 50 }))).toBe(25)
    expect(percent(job({ TotalBytes: 100, DoneBytes: 150 }))).toBe(100)
    expect(percent(job({ TotalBytes: 100, DoneBytes: -5 }))).toBe(0)
    expect(percent(job({ TotalBytes: 0, DoneBytes: 0 }))).toBe(0)
    expect(percent(job({ State: 'done', TotalBytes: 0, DoneBytes: 0 }))).toBe(100)
  })
  it('counts failures from FailedFiles, not from the capped list', () => {
    expect(failedCount(job({ FailedFiles: 4321, Failures: new Array(1000).fill({ Path: 'p', Code: 'internal', Message: 'm' }) }))).toBe(4321)
    expect(failedCount(job({ FailedFiles: 0, Failures: null }))).toBe(0)
  })
})

describe('pollJobs', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  const fakeApi = (jobs: () => Promise<Job[]>) => ({ jobs }) as unknown as Api

  it('polls immediately and every interval, passing the result on', async () => {
    const jobs = vi.fn(async () => [job()])
    const onUpdate = vi.fn()
    const stop = pollJobs(fakeApi(jobs), onUpdate, 1000)
    await vi.advanceTimersByTimeAsync(0)
    expect(jobs).toHaveBeenCalledTimes(1)
    expect(onUpdate).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(3000)
    expect(jobs).toHaveBeenCalledTimes(4)
    stop()
  })

  it('defaults to one second', async () => {
    const jobs = vi.fn(async () => [])
    const stop = pollJobs(fakeApi(jobs), () => {})
    await vi.advanceTimersByTimeAsync(0)
    await vi.advanceTimersByTimeAsync(999)
    expect(jobs).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(jobs).toHaveBeenCalledTimes(2)
    stop()
  })

  it('does not call again after the stop function', async () => {
    const jobs = vi.fn(async () => [])
    const stop = pollJobs(fakeApi(jobs), () => {}, 1000)
    await vi.advanceTimersByTimeAsync(2500)
    const n = jobs.mock.calls.length
    stop()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(jobs).toHaveBeenCalledTimes(n)
  })

  it('drops a reply that arrives after the stop function', async () => {
    let resolve!: (j: Job[]) => void
    const jobs = vi.fn(() => new Promise<Job[]>((r) => (resolve = r)))
    const onUpdate = vi.fn()
    const stop = pollJobs(fakeApi(jobs), onUpdate, 1000)
    await vi.advanceTimersByTimeAsync(0)
    stop()
    resolve([job()])
    await vi.advanceTimersByTimeAsync(5000)
    expect(onUpdate).not.toHaveBeenCalled()
    expect(jobs).toHaveBeenCalledTimes(1)
  })

  it('keeps polling after a failure and reports it', async () => {
    let n = 0
    const jobs = vi.fn(async () => {
      n++
      if (n === 2) throw JSON.stringify({ code: 'internal', message: 'no daemon' })
      return [job({ ID: 'j' + n })]
    })
    const onUpdate = vi.fn()
    const onError = vi.fn()
    const stop = pollJobs(fakeApi(jobs), onUpdate, 1000, onError)
    await vi.advanceTimersByTimeAsync(3000)
    expect(jobs).toHaveBeenCalledTimes(4)
    expect(onError).toHaveBeenCalledTimes(1)
    expect(onError.mock.calls[0][0]).toMatchObject({ code: 'internal', message: 'no daemon' })
    expect(onUpdate).toHaveBeenCalledTimes(3)
    stop()
  })

  it('does not start a second request while one is still running', async () => {
    let resolve!: (j: Job[]) => void
    const jobs = vi.fn(() => new Promise<Job[]>((r) => (resolve = r)))
    const stop = pollJobs(fakeApi(jobs), () => {}, 1000)
    await vi.advanceTimersByTimeAsync(5000)
    expect(jobs).toHaveBeenCalledTimes(1)
    resolve([])
    await vi.advanceTimersByTimeAsync(1000)
    expect(jobs).toHaveBeenCalledTimes(2)
    stop()
  })
})

describe('jobFailureWatcher', () => {
  const j = (ID: string, State: string): Job => ({ ID, State }) as Job

  it('stays quiet about failures that were already there when it first looked', () => {
    const w = jobFailureWatcher()
    expect(w.update([j('old', 'failed'), j('older', 'partial')])).toEqual([])
    expect(w.update([j('old', 'failed'), j('older', 'partial')])).toEqual([])
  })

  it('reports a job that fails or partly fails after being seen active, once', () => {
    const w = jobFailureWatcher()
    w.update([j('a', 'running'), j('b', 'queued'), j('c', 'waiting_peer')])
    expect(w.update([j('a', 'failed'), j('b', 'partial'), j('c', 'waiting_peer')]).map((x) => x.ID)).toEqual(['a', 'b'])
    expect(w.update([j('a', 'failed'), j('b', 'partial'), j('c', 'waiting_peer')])).toEqual([])
  })

  it('stays quiet about success and cancellation', () => {
    const w = jobFailureWatcher()
    w.update([j('a', 'running'), j('b', 'running')])
    expect(w.update([j('a', 'done'), j('b', 'canceled')])).toEqual([])
  })

  it('reports a job started here that failed before any poll saw it active', () => {
    const w = jobFailureWatcher()
    w.update([])
    w.track('fast')
    expect(w.update([j('fast', 'failed')]).map((x) => x.ID)).toEqual(['fast'])
    expect(w.update([j('fast', 'failed')])).toEqual([])
  })

  it('stays quiet about a job first seen failed that was not started here', () => {
    const w = jobFailureWatcher()
    w.update([])
    expect(w.update([j('elsewhere', 'failed')])).toEqual([])
  })

  it('reports a retried job that fails again', () => {
    const w = jobFailureWatcher()
    w.update([j('a', 'failed')])
    w.update([j('a', 'running')])
    expect(w.update([j('a', 'failed')]).map((x) => x.ID)).toEqual(['a'])
  })
})
