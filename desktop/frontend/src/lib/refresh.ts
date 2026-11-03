import type { Job, PeerStatus } from './api'
import type { Pane } from './panes'

export const POLL_MS = 3000

/** The wait before the next background listing: at least 3 s, and ten times a slow one. */
export function nextDelay(lastMs: number, baseMs = POLL_MS): number {
  return Math.max(baseMs, 10 * lastMs)
}

/**
 * Background refresh of one pane. Polls every 3 s (more when listings are
 * slow), never runs two at once, and only runs while canRun() is true;
 * otherwise it looks again in a second. request() asks for a refresh sooner
 * (a job changed): it is coalesced with one in flight and held back by the
 * same ten-times-duration backoff, so a 100,000 row folder is not hammered.
 */
export class AutoRefresher {
  private timer: ReturnType<typeof setTimeout> | undefined
  private at = 0 // when the timer fires
  private inFlight = false
  private wanted = false
  private started = false
  private lastEnd = 0
  private lastDur = 0

  constructor(private o: { run: () => Promise<unknown>; canRun: () => boolean; baseMs?: number }) {}

  start() {
    this.started = true
    this.schedule(Date.now() + (this.o.baseMs ?? POLL_MS))
  }

  stop() {
    this.started = false
    if (this.timer !== undefined) clearTimeout(this.timer)
    this.timer = undefined
  }

  request() {
    if (!this.started) return
    this.wanted = true
    if (this.inFlight) return
    const due = this.lastEnd + 10 * this.lastDur
    const when = Math.max(Date.now(), due)
    if (this.timer === undefined || when < this.at) this.schedule(when)
  }

  private schedule(when: number) {
    if (this.timer !== undefined) clearTimeout(this.timer)
    this.at = when
    this.timer = setTimeout(() => void this.fire(), Math.max(0, when - Date.now()))
  }

  private async fire() {
    this.timer = undefined
    if (!this.started || this.inFlight) return
    if (!this.o.canRun()) {
      this.schedule(Date.now() + 1000)
      return
    }
    this.inFlight = true
    this.wanted = false
    const t0 = Date.now()
    try {
      await this.o.run()
    } catch {
      // a failed refresh is reported by run itself; polling goes on
    }
    this.inFlight = false
    this.lastEnd = Date.now()
    this.lastDur = this.lastEnd - t0
    if (!this.started) return
    const regular = this.lastEnd + nextDelay(this.lastDur, this.o.baseMs)
    this.schedule(this.wanted ? Math.min(regular, this.lastEnd + 10 * this.lastDur) : regular)
  }
}

/** Whether a job reads or writes the peer and share a pane shows. */
export function touchesPane(
  j: Job,
  p: Pane,
  selfId: string,
  peers: Pick<PeerStatus, 'ID' | 'Name'>[],
): boolean {
  if (p.share === '') return false
  const id = p.peer === 'local' ? selfId : peers.find((x) => x.Name === p.peer)?.ID
  if (!id) return false
  return [j.Spec.Src, j.Spec.Dst].some((e) => e.Peer === id && e.Share === p.share)
}

/**
 * What of a job can change a listing: its state and the counts of files
 * finished, skipped and failed. Bytes inside a file cannot, so they are left
 * out and a big copy does not refresh the panes every second.
 */
export function jobSignatures(jobs: Job[], touches: (j: Job) => boolean): Map<string, string> {
  const m = new Map<string, string>()
  for (const j of jobs) {
    if (touches(j)) m.set(`${j.Host}/${j.ID}`, [j.State, j.TotalFiles, j.DoneFiles, j.SkippedFiles, j.FailedFiles].join('|'))
  }
  return m
}

/**
 * Whether any job touching a shown pane is new, changed or gone since `prev`.
 * A job first seen already finished counts, which covers one that started and
 * ended between two polls of the job list.
 */
export function jobsChanged(
  prev: Map<string, string>,
  jobs: Job[],
  shown: Pane[],
  selfId: string,
  peers: Pick<PeerStatus, 'ID' | 'Name'>[],
): { changed: boolean; next: Map<string, string> } {
  const next = jobSignatures(jobs, (j) => shown.some((p) => touchesPane(j, p, selfId, peers)))
  let changed = next.size !== prev.size
  if (!changed) for (const [k, v] of next) if (prev.get(k) !== v) changed = true
  return { changed, next }
}
