<script lang="ts">
  import { createEventDispatcher } from 'svelte'
  import { ko } from '../ko'
  import { parseError, type Api, type ApiError, type AppState, type MountResult, type ShareInfo, type Status } from '../lib/api'
  import { errorText } from '../lib/transfers'
  import { formatTime } from '../lib/format'
  import Dialog from '../components/Dialog.svelte'

  export let api: Api
  export let status: Status | null = null
  export let state: AppState | null = null

  const dispatch = createEventDispatcher<{ changed: void }>()

  let shares: ShareInfo[] = []
  let err: ApiError | null = null
  let info = ''
  let busy = false

  let invite = ''
  let joinText = ''
  let shareName = ''
  let sharePath = ''
  let shareRO = false
  let drive = ''
  let mountResult: MountResult | null = null
  let serviceOutput = ''
  let newName = ''
  let confirm: null | { kind: 'peer' | 'share' | 'service'; name: string } = null

  $: dialog = confirm?.kind === 'peer'
    ? { title: ko.hosts.removeTitle, message: ko.hosts.removeMessage, label: ko.hosts.removeConfirm }
    : confirm?.kind === 'share'
      ? { title: ko.hosts.removeShareTitle, message: ko.hosts.removeShareMessage, label: ko.hosts.removeShareConfirm }
      : { title: ko.hosts.serviceUninstallTitle, message: ko.hosts.serviceUninstallMessage, label: ko.hosts.serviceUninstallConfirm }

  $: peers = status?.Peers ?? []
  $: platform = state?.Platform ?? ''

  async function run<T>(f: () => Promise<T>): Promise<T | undefined> {
    err = null
    info = ''
    busy = true
    try {
      return await f()
    } catch (e) {
      err = parseError(e)
      return undefined
    } finally {
      busy = false
    }
  }

  async function loadShares() {
    try {
      shares = (await api.listShares('local')) ?? []
    } catch (e) {
      err = parseError(e)
    }
  }
  loadShares()

  async function renameSelf() {
    const name = newName.trim()
    const ok = await run(async () => {
      await api.renameSelf(name)
      return true
    })
    if (ok) {
      info = ko.hosts.renamed(name)
      newName = ''
      dispatch('changed')
    }
  }

  async function makeInvite() {
    const v = await run(() => api.invite(0))
    if (v !== undefined) invite = v
  }

  async function copyInvite() {
    try {
      await navigator.clipboard.writeText(invite)
      info = ko.hosts.copied
    } catch {
      // The text stays selectable in the box.
    }
  }

  async function join() {
    const p = await run(() => api.joinPeer(joinText.trim()))
    if (p) {
      info = ko.hosts.joined(p.Name)
      joinText = ''
      dispatch('changed')
    }
  }

  async function pickFolder() {
    const p = await run(() => api.pickFolder(ko.hosts.pickFolderTitle))
    if (p) sharePath = p
  }

  async function addShare() {
    const ok = await run(async () => {
      await api.addShare(shareName.trim(), sharePath, shareRO)
      return true
    })
    if (ok) {
      shareName = sharePath = ''
      shareRO = false
      await loadShares()
      dispatch('changed')
    }
  }

  async function doConfirm() {
    if (!confirm) return
    const { kind, name } = confirm
    if (kind === 'service') {
      const r = await run(() => api.uninstallService())
      confirm = null
      if (r) {
        serviceOutput = r.Output
        info = ko.hosts.serviceUninstalled
      }
      return
    }
    const ok = await run(async () => {
      if (kind === 'peer') await api.removePeer(name)
      else await api.removeShare(name)
      return true
    })
    confirm = null
    if (ok) {
      await loadShares()
      dispatch('changed')
    }
  }

  async function mount() {
    mountResult = null
    const r = await run(() => api.mount(drive.trim()))
    if (r) {
      mountResult = r
      info = ko.hosts.mountDone
    }
  }

  async function unmount() {
    mountResult = null
    const r = await run(() => api.unmount(drive.trim()))
    if (r) {
      mountResult = r
      info = ko.hosts.mountDone
    }
  }

  async function showToken() {
    // The token is shown by a native dialog; this code never receives it.
    await run(() => api.showMountToken(ko.hosts.tokenDialogTitle, ko.hosts.tokenDialogMessage))
  }

  async function installService() {
    const r = await run(() => api.installService())
    if (r) {
      serviceOutput = r.Output
      info = ko.hosts.serviceDone
    }
  }
</script>

<div class="hosts">
  {#if err}
    <div class="banner bad">
      <strong>{errorText(err.code)}</strong>
      {#if err.message}<span class="muted mono">{err.message}</span>{/if}
    </div>
  {/if}
  {#if info}<div class="banner ok">{info}</div>{/if}

  <div class="card">
    <h2>{ko.hosts.thisHost}</h2>
    {#if status}
      <div class="row">
        <span class="muted">{ko.hosts.hostName}</span><strong>{status.Name}</strong>
        <span class="muted">{ko.hosts.version}</span><span>{status.Version}</span>
        <span class="muted">{ko.hosts.hostId}</span><span class="mono">{status.ID}</span>
      </div>
      <div class="row rename">
        <input placeholder={ko.hosts.renameLabel} title={ko.hosts.renameHint} bind:value={newName} />
        <button disabled={busy || newName.trim() === ''} on:click={renameSelf}>{ko.hosts.renameButton}</button>
      </div>
    {/if}
  </div>

  <div class="card">
    <h2>{ko.hosts.hostsTitle}</h2>
    {#if peers.length === 0}
      <p class="muted">{ko.hosts.noHosts}</p>
    {/if}
    {#each peers as p (p.ID)}
      <div class="row item">
        <strong>{p.Name}</strong>
        {#if p.Incompatible}
          <span class="chip bad">{ko.common.incompatible}</span>
        {:else}
          <span class="chip" class:ok={p.Online} class:bad={!p.Online}>{p.Online ? ko.common.online : ko.common.offline}</span>
        {/if}
        <span class="muted mono grow">{(p.Addrs ?? []).join(', ')}</span>
        <span class="muted">{ko.hosts.lastSeen}: {formatTime(p.LastSeen) || ko.hosts.never}</span>
        <button class="danger" disabled={busy} on:click={() => (confirm = { kind: 'peer', name: p.Name })}>{ko.hosts.remove}</button>
      </div>
    {/each}

    <h2 class="sub">{ko.hosts.inviteTitle}</h2>
    <div class="row">
      <button disabled={busy} on:click={makeInvite}>{ko.hosts.inviteCreate}</button>
    </div>
    {#if invite}
      <p class="muted">{ko.hosts.inviteHelp}</p>
      <textarea class="mono wide" readonly rows="3" value={invite}></textarea>
      <button on:click={copyInvite}>{ko.hosts.inviteCopy}</button>
    {/if}

    <h2 class="sub">{ko.hosts.joinTitle}</h2>
    <textarea class="mono wide" rows="3" placeholder={ko.hosts.joinPlaceholder} bind:value={joinText}></textarea>
    <button class="primary" disabled={busy || joinText.trim() === ''} on:click={join}>{ko.hosts.join}</button>
  </div>

  <div class="card">
    <h2>{ko.hosts.sharesTitle}</h2>
    {#if shares.length === 0}<p class="muted">{ko.hosts.noShares}</p>{/if}
    {#each shares as s (s.Name)}
      <div class="row item">
        <strong>{s.Name}</strong>
        {#if s.ReadOnly}<span class="chip">{ko.common.readOnly}</span>{/if}
        <span class="grow"></span>
        <button class="danger" disabled={busy} on:click={() => (confirm = { kind: 'share', name: s.Name })}>{ko.hosts.removeShare}</button>
      </div>
    {/each}
    <div class="row addshare">
      <input placeholder={ko.hosts.shareName} title={ko.hosts.shareNameHint} bind:value={shareName} />
      <button disabled={busy} on:click={pickFolder}>{ko.hosts.pickFolder}</button>
      <span class="mono grow" title={sharePath}>{sharePath}</span>
      <label><input type="checkbox" bind:checked={shareRO} /> {ko.hosts.readOnly}</label>
      <button class="primary" disabled={busy || shareName.trim() === '' || sharePath === ''} on:click={addShare}>{ko.hosts.addShare}</button>
    </div>
  </div>

  <div class="card">
    <h2>{ko.hosts.mountTitle}</h2>
    <p class="muted">{ko.hosts.mountHelp}</p>
    <div class="row">
      {#if platform === 'windows'}
        <input placeholder={ko.hosts.drive} bind:value={drive} />
      {/if}
      <button class="primary" disabled={busy} on:click={mount}>{ko.hosts.mount}</button>
      <button disabled={busy} on:click={unmount}>{ko.hosts.unmount}</button>
      {#if platform === 'darwin' || platform === 'windows'}
        <button on:click={showToken}>{ko.hosts.showToken}</button>
      {/if}
    </div>
    {#if platform === 'darwin' || platform === 'windows'}
      <p class="muted">{ko.hosts.needsToken}</p>
    {:else if platform === 'linux'}
      <p class="muted">{ko.hosts.linuxMountNote}</p>
    {/if}
    {#if mountResult}
      {#if mountResult.Notes?.length}
        <div class="muted">{ko.hosts.notes}</div>
        <pre class="mono out">{mountResult.Notes.join('\n')}</pre>
      {/if}
      {#if mountResult.Output}
        <div class="muted">{ko.hosts.output}</div>
        <pre class="mono out">{mountResult.Output}</pre>
      {/if}
    {/if}
  </div>

  <div class="card">
    <h2>{ko.hosts.serviceTitle}</h2>
    <p class="muted">{ko.hosts.serviceHelp}</p>
    {#if state?.InProcess}<p class="muted">{ko.hosts.serviceWaitsForApp}</p>{/if}
    <div class="row">
      <button disabled={busy} on:click={installService}>{ko.hosts.serviceInstall}</button>
      <button class="danger" disabled={busy} on:click={() => (confirm = { kind: 'service', name: 'sugar-it' })}>{ko.hosts.serviceUninstall}</button>
    </div>
    {#if serviceOutput}<pre class="mono out">{serviceOutput}</pre>{/if}
  </div>
</div>

{#if confirm}
  <Dialog
    title={dialog.title}
    message={dialog.message}
    names={[confirm.name]}
    confirmLabel={dialog.label}
    danger
    {busy}
    on:confirm={doConfirm}
    on:cancel={() => (confirm = null)}
  />
{/if}

<style>
  .hosts { height: 100%; overflow: auto; padding-right: 4px; }
  .banner { padding: 8px 12px; border-radius: 6px; margin-bottom: 10px; display: flex; gap: 10px; flex-wrap: wrap; }
  .banner.bad { background: var(--danger-bg); color: var(--danger); }
  .banner.ok { background: var(--row-hover); }
  .item { padding: 4px 0; border-bottom: 1px solid var(--line); }
  .grow { flex: 1; min-width: 0; }
  .sub { margin-top: 16px; }
  .wide { width: 100%; display: block; margin: 6px 0; }
  .addshare, .rename { margin-top: 10px; }
  .out { background: var(--bg); padding: 8px; border-radius: 6px; white-space: pre-wrap; margin: 4px 0; }
</style>
