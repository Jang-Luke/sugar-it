<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import { ko } from './ko'
  import { parseError, wailsApi, type Api, type ApiError, type AppState, type Job, type Status } from './lib/api'
  import { errorText, isActive, pollJobs } from './lib/transfers'
  import { noticeDaemonText, noticeText, progressText, showBanner, updateBusy, updateErrorText } from './lib/update'
  import Dialog from './components/Dialog.svelte'
  import Files from './views/Files.svelte'
  import Hosts from './views/Hosts.svelte'
  import Transfers from './views/Transfers.svelte'

  export let api: Api = wailsApi()

  type Tab = 'files' | 'transfers' | 'hosts'
  let tab: Tab = 'files'
  let state: AppState | null = null
  let status: Status | null = null
  let statusError: ApiError | null = null
  let jobs: Job[] = []
  let jobsError: ApiError | null = null
  let installMsg: ApiError | null = null
  let stopPolling = () => {}
  let statusTimer: ReturnType<typeof setTimeout> | undefined
  let dead = false
  let dismissedUpdate = '' // 나중에: hidden until the next start, for this version only
  let confirmUpdate = false
  let updateError: ApiError | null = null

  async function loadState() {
    try {
      state = await api.appState()
    } catch (e) {
      statusError = parseError(e)
    }
  }

  async function loadStatus() {
    try {
      status = await api.status()
      statusError = null
    } catch (e) {
      statusError = parseError(e)
    }
    await loadState()
    if (!dead) statusTimer = setTimeout(loadStatus, 2000)
  }

  async function installService() {
    installMsg = null
    try {
      await api.installService()
      tab = 'hosts'
    } catch (e) {
      installMsg = parseError(e)
    }
  }

  function updateClicked() {
    if (state?.Update.OneClick) confirmUpdate = true
    else void openReleasePage()
  }

  async function openReleasePage() {
    updateError = null
    try {
      await api.openReleasePage()
    } catch (e) {
      updateError = parseError(e)
    }
  }

  async function applyUpdate() {
    confirmUpdate = false
    updateError = null
    // The status poll is too slow for a progress display.
    const fast = setInterval(loadState, 500)
    try {
      await api.applyUpdate()
    } catch (e) {
      updateError = parseError(e)
    } finally {
      clearInterval(fast)
      await loadState()
    }
  }

  async function dismissNotice() {
    await api.dismissUpdateNotice()
    await loadState()
  }

  onMount(() => {
    void loadStatus()
    stopPolling = pollJobs(
      api,
      (j) => {
        jobs = j
        jobsError = null
      },
      1000,
      (e) => (jobsError = e),
    )
  })

  onDestroy(() => {
    dead = true
    stopPolling()
    if (statusTimer) clearTimeout(statusTimer)
  })

  $: activeCount = jobs.filter(isActive).length
  $: update = state?.Update
</script>

<main>
  {#if state?.StartError}
    <div class="top bad">
      <strong>{ko.banner.startError}</strong>
      <span class="muted mono">{state.StartError}</span>
    </div>
  {/if}
  {#if state?.Failure}
    <div class="top bad">
      <strong>{ko.banner.failure}</strong>
      <span class="muted mono">{state.Failure}</span>
    </div>
  {/if}
  {#if state?.InProcess}
    <div class="top warn">
      <strong>{ko.banner.inProcess(state.Platform, state.CloseHides)}</strong>
      <span class="grow"></span>
      {#if installMsg}<span class="error">{errorText(installMsg.code)} <span class="mono">{installMsg.message}</span></span>{/if}
      <button on:click={installService}>{ko.banner.installService}</button>
    </div>
  {/if}
  {#if statusError && !state?.StartError}
    <div class="top bad">
      <strong>{ko.banner.statusError}</strong>
      <span class="mono">{errorText(statusError.code)}</span>
      <span class="muted mono">{statusError.message}</span>
      <span class="grow"></span>
      <button on:click={loadStatus}>{ko.banner.retry}</button>
    </div>
  {/if}
  {#if update?.Notice}
    <div class="top bad" role="alert">
      <strong>{noticeText(update.Notice)} {noticeDaemonText(update)}</strong>
      {#if update.NoticeDetail}<span class="muted mono">{update.NoticeDetail}</span>{/if}
      <span class="grow"></span>
      <button on:click={openReleasePage}>{ko.update.openPage}</button>
      <button on:click={dismissNotice}>{ko.common.close}</button>
    </div>
  {/if}
  {#if update && (showBanner(update, dismissedUpdate) || updateError)}
    <div class="top update">
      {#if updateBusy(update)}
        <span>{progressText(update)}</span>
      {:else if showBanner(update, dismissedUpdate)}
        <span>{ko.update.available(update.Latest, state?.Platform ?? '', update.OneClick)}</span>
      {/if}
      {#if updateError}
        <span class="error">{ko.update.failed} {updateErrorText(updateError.code)} <span class="mono">{updateError.message}</span></span>
      {/if}
      <span class="grow"></span>
      {#if !updateBusy(update) && showBanner(update, dismissedUpdate)}
        <button class="primary" on:click={updateClicked}>{update.OneClick ? ko.update.install : ko.update.openPage}</button>
        {#if updateError && update.OneClick}<button on:click={openReleasePage}>{ko.update.openPage}</button>{/if}
        <button on:click={() => (dismissedUpdate = update?.Latest ?? '')}>{ko.update.later}</button>
      {:else if updateError && !updateBusy(update)}
        <button on:click={openReleasePage}>{ko.update.openPage}</button>
        <button on:click={() => (updateError = null)}>{ko.common.close}</button>
      {/if}
    </div>
  {/if}

  <nav>
    <button class:on={tab === 'files'} on:click={() => (tab = 'files')}>{ko.tabs.files}</button>
    <button class:on={tab === 'transfers'} on:click={() => (tab = 'transfers')}>
      {ko.tabs.transfers}{activeCount > 0 ? ` (${activeCount})` : ''}{jobsError ? ' !' : ''}
    </button>
    <button class:on={tab === 'hosts'} on:click={() => (tab = 'hosts')}>{ko.tabs.hosts}</button>
    <span class="grow"></span>
    {#if status}<span class="muted">{status.Name}</span>{/if}
  </nav>

  <section class="body">
    <!-- Files stays mounted so that the panes keep their place while another tab is open. -->
    <div class="tab" hidden={tab !== 'files'}>
      <Files {api} peers={status?.Peers ?? []} {jobs} selfId={status?.ID ?? ''} on:showTransfers={() => (tab = 'transfers')} />
    </div>
    {#if tab === 'transfers'}
      <div class="tab">
        <Transfers {api} {jobs} pollError={jobsError} selfId={status?.ID ?? ''} peers={status?.Peers ?? []} />
      </div>
    {:else if tab === 'hosts'}
      <div class="tab">
        <Hosts {api} {status} {state} on:changed={loadStatus} on:updateFound={() => { dismissedUpdate = ''; void loadState() }} />
      </div>
    {/if}
  </section>
</main>

{#if confirmUpdate}
  <Dialog
    title={ko.update.confirmTitle}
    message={ko.update.confirmMessage(activeCount)}
    confirmLabel={ko.update.install}
    on:confirm={applyUpdate}
    on:cancel={() => (confirmUpdate = false)}
  />
{/if}

<style>
  main { display: flex; flex-direction: column; height: 100%; padding: 10px 14px; gap: 8px; }
  .top { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; padding: 8px 12px; border-radius: 6px; }
  .top.warn { background: var(--warn-bg); }
  .top.bad { background: var(--danger-bg); color: var(--danger); }
  .top.update { background: var(--row-hover); }
  .grow { flex: 1; }
  nav { display: flex; gap: 6px; align-items: center; }
  nav button.on { background: var(--accent); color: var(--accent-text); border-color: var(--accent); }
  .body { flex: 1; min-height: 0; }
  .tab { height: 100%; }
  .tab[hidden] { display: none; }
</style>
