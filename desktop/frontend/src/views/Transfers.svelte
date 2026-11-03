<script lang="ts">
  import { ko } from '../ko'
  import { parseError, type Api, type ApiError, type Job, type PeerStatus } from '../lib/api'
  import { errorText, failedCount, isActive, percent } from '../lib/transfers'
  import { formatBytes } from '../lib/format'

  export let api: Api
  export let jobs: Job[] = []
  export let pollError: ApiError | null = null
  export let selfId = ''
  export let peers: PeerStatus[] = []

  let open = new Set<string>()
  let actionError: ApiError | null = null

  const key = (j: Job) => j.Host + '/' + j.ID

  function hostName(id: string): string {
    if (id === selfId) return ko.transfers.here
    return peers.find((p) => p.ID === id)?.Name ?? id.slice(0, 8)
  }

  function where(e: { Peer: string; Share: string; Path: string }): string {
    return `${hostName(e.Peer)}:${e.Share}/${e.Path}`
  }

  const label = <T extends Record<string, string>>(m: T, k: string): string =>
    Object.hasOwn(m, k) ? m[k as keyof T] : k
  const opLabel = (j: Job) => label(ko.transfers.ops, j.Spec.Op)
  const stateLabel = (j: Job) => label(ko.transfers.states, j.State)
  const conflictLabel = (j: Job) => label(ko.files.conflicts, j.Spec.OnConflict)

  function toggle(j: Job) {
    const k = key(j)
    open.has(k) ? open.delete(k) : open.add(k)
    open = open
  }

  async function act(f: () => Promise<void>) {
    actionError = null
    try {
      await f()
    } catch (e) {
      actionError = parseError(e)
    }
  }

  $: ordered = [...jobs].sort((a, b) => (a.Created < b.Created ? 1 : a.Created > b.Created ? -1 : 0))
</script>

<div class="transfers">
  {#if pollError}
    <div class="banner bad">
      <strong>{ko.transfers.pollError}</strong>
      <span class="muted mono">{errorText(pollError.code)} {pollError.message}</span>
    </div>
  {/if}
  {#if actionError}
    <div class="banner bad">
      <strong>{errorText(actionError.code)}</strong>
      <span class="muted mono">{actionError.message}</span>
    </div>
  {/if}

  {#if ordered.length === 0}
    <p class="muted">{ko.transfers.none}</p>
  {/if}

  {#each ordered as j (key(j))}
    <div class="card job">
      <div class="row top">
        <strong>{opLabel(j)}</strong>
        <span class="mono grow">{where(j.Spec.Src)} {ko.transfers.arrow} {where(j.Spec.Dst)}</span>
        <span class="chip" class:ok={j.State === 'done'} class:bad={j.State === 'failed' || j.State === 'partial'}>
          {stateLabel(j)}
        </span>
        {#if isActive(j)}
          <button on:click={() => act(() => api.cancelJob(j.Host, j.ID))}>{ko.transfers.cancel}</button>
        {/if}
        {#if j.State === 'failed' || j.State === 'partial'}
          <button on:click={() => act(() => api.retryJob(j.Host, j.ID))}>{ko.transfers.retry}</button>
        {/if}
      </div>
      <div class="bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={percent(j)}>
        <div class="fill" class:bad={j.State === 'failed'} style="width: {percent(j)}%"></div>
      </div>
      <div class="row meta muted">
        <span>{percent(j)}%</span>
        <span>{formatBytes(j.DoneBytes)} / {formatBytes(j.TotalBytes)}</span>
        {#if isActive(j) && j.BytesPerSec > 0}<span>{formatBytes(j.BytesPerSec)}{ko.transfers.perSec}</span>{/if}
        <span>{ko.transfers.files(j.DoneFiles, j.TotalFiles)}</span>
        {#if j.SkippedFiles > 0}<span title={(j.Skipped ?? []).map((s) => s.Path).join('\n')}>{ko.transfers.skipped(j.SkippedFiles)}</span>{/if}
        {#if failedCount(j) > 0}<span class="error">{ko.transfers.failureCount(failedCount(j))}</span>{/if}
        <span>{ko.transfers.conflict(conflictLabel(j))}</span>
      </div>
      {#if j.Error}
        <div class="error">
          {ko.transfers.jobError}: {errorText(j.Error.Code)}
          <span class="muted mono">{j.Error.Path} {j.Error.Message}</span>
        </div>
      {/if}
      {#if (j.State === 'failed' || j.State === 'partial') && (failedCount(j) > 0 || (j.Failures ?? []).length > 0)}
        <button class="link" on:click={() => toggle(j)}>
          {open.has(key(j)) ? ko.transfers.hideFailures : ko.transfers.showFailures}
        </button>
        {#if open.has(key(j))}
          <div class="failures">
            {#if (j.Failures ?? []).length < failedCount(j)}
              <div class="muted">{ko.transfers.failureCapped((j.Failures ?? []).length, failedCount(j))}</div>
            {/if}
            {#each j.Failures ?? [] as f}
              <div class="frow">
                <span class="mono">{f.Path}</span>
                <span class="error">{errorText(f.Code)}</span>
                {#if f.Message}<span class="muted mono">{f.Message}</span>{/if}
              </div>
            {/each}
          </div>
        {/if}
      {/if}
    </div>
  {/each}
</div>

<style>
  .transfers { height: 100%; overflow: auto; }
  .banner { padding: 8px 12px; border-radius: 6px; margin-bottom: 10px; display: flex; gap: 10px; flex-wrap: wrap; }
  .banner.bad { background: var(--danger-bg); color: var(--danger); }
  .job { margin-bottom: 10px; }
  .top .grow { flex: 1; min-width: 0; word-break: break-all; }
  .bar { height: 8px; background: var(--line); border-radius: 4px; overflow: hidden; margin: 8px 0 4px; }
  .fill { height: 100%; background: var(--accent); transition: width 0.3s; }
  .fill.bad { background: var(--danger); }
  .meta { font-size: 12px; gap: 14px; }
  .link { border: none; background: none; color: var(--accent); padding: 4px 0; }
  .failures { max-height: 220px; overflow: auto; border-top: 1px solid var(--line); margin-top: 4px; padding-top: 4px; }
  .frow { display: flex; gap: 10px; flex-wrap: wrap; padding: 2px 0; }
</style>
