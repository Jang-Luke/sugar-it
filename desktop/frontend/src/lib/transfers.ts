import { ko } from '../ko'
import { parseError, type Api, type ApiError, type Job } from './api'

/** Korean text for a spec section 6 error code; any other code gets a generic text. */
export function errorText(code: string): string {
  return Object.hasOwn(ko.errors, code) && code !== 'unknown' ? ko.errors[code as keyof typeof ko.errors] : ko.errors.unknown
}

/** Percent done by bytes, 0 to 100. A finished job with no bytes counts as done. */
export function percent(j: Job): number {
  if (j.TotalBytes <= 0) return j.State === 'done' ? 100 : 0
  return Math.min(100, Math.max(0, Math.floor((j.DoneBytes / j.TotalBytes) * 100)))
}

/** Failed files of a job. Failures is capped at 1000 entries; this is not. */
export function failedCount(j: Job): number {
  return j.FailedFiles
}

export const ACTIVE_STATES = ['queued', 'running', 'waiting_peer']

export function isActive(j: Job): boolean {
  return ACTIVE_STATES.includes(j.State)
}

/**
 * Loads the jobs now and then every ms (default 1s). A failure is reported to
 * onError and does not stop the polling. Requests never overlap, and after the
 * returned function is called nothing is requested or reported any more.
 */
export function pollJobs(
  api: Api,
  onUpdate: (jobs: Job[]) => void,
  ms = 1000,
  onError?: (e: ApiError) => void,
): () => void {
  let stopped = false
  let timer: ReturnType<typeof setTimeout> | undefined
  const tick = async () => {
    try {
      const jobs = await api.jobs()
      if (!stopped) onUpdate(jobs ?? [])
    } catch (e) {
      if (!stopped) onError?.(parseError(e))
    }
    if (!stopped) timer = setTimeout(tick, ms)
  }
  void tick()
  return () => {
    stopped = true
    if (timer !== undefined) clearTimeout(timer)
  }
}

/**
 * Finds transfers that have just ended with an error (failed or partial), so
 * the Files view can stay quiet until one does. A failure counts once: when a
 * job seen active ends that way, or when a job started here (track) is first
 * seen already ended, since it can finish between two polls of the job list.
 * Failures from before the first look, or from elsewhere, are not news.
 */
export function jobFailureWatcher() {
  const seen = new Map<string, string>()
  const tracked = new Set<string>()
  return {
    track(id: string) {
      tracked.add(id)
    },
    update(jobs: Job[]): Job[] {
      const failed = jobs.filter((j) => {
        if (j.State !== 'failed' && j.State !== 'partial') return false
        const before = seen.get(j.ID)
        return before === undefined ? tracked.has(j.ID) : ACTIVE_STATES.includes(before)
      })
      for (const j of jobs) {
        seen.set(j.ID, j.State)
        tracked.delete(j.ID)
      }
      return failed
    },
  }
}
