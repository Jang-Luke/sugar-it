<script lang="ts">
  import { createEventDispatcher } from 'svelte'
  import { ko } from '../ko'
  import { parseError, type Api, type ApiError, type Conflict, type Entry, type Job, type PeerStatus } from '../lib/api'
  import { canDrop, joinPath, snapshotSelection, targetOf, type Pane } from '../lib/panes'
  import { errorText, isActive } from '../lib/transfers'
  import Dialog from '../components/Dialog.svelte'
  import PaneView from '../components/PaneView.svelte'

  export let api: Api
  export let peers: PeerStatus[] = []
  export let jobs: Job[] = []

  const dispatch = createEventDispatcher<{ showTransfers: void }>()

  let left: Pane = { id: 'left', peer: 'local', share: '', path: '' }
  let right: Pane = { id: 'right', peer: 'local', share: '', path: '' }
  let leftSel: Entry[] = []
  let rightSel: Entry[] = []
  let leftView: PaneView
  let rightView: PaneView
  let activeId: 'left' | 'right' = 'left'
  const policies: Conflict[] = ['fail', 'skip', 'overwrite', 'rename']
  let conflict: Conflict = 'fail'
  let dragSrc: Pane | null = null
  let notice: { ok: boolean; text: string; detail?: string } | null = null
  // A dialog acts on what was selected when it opened, not on what the pane shows later.
  let dialog: null | { kind: 'delete' | 'rename' | 'mkdir'; pane: Pane; entries: Entry[]; view: PaneView } = null
  let busy = false

  $: src = activeId === 'left' ? left : right
  $: dst = activeId === 'left' ? right : left
  $: srcSel = activeId === 'left' ? leftSel : rightSel
  $: srcView = activeId === 'left' ? leftView : rightView

  // Refresh both panes whenever a transfer finishes.
  let activeJobs = new Set<string>()
  $: {
    const now = new Set(jobs.filter(isActive).map((j) => j.ID))
    let finished = false
    for (const id of activeJobs) if (!now.has(id)) finished = true
    activeJobs = now
    if (finished) {
      void leftView?.loadEntries()
      void rightView?.loadEntries()
    }
  }

  function fail(e: unknown) {
    const err: ApiError = parseError(e)
    notice = { ok: false, text: errorText(err.code), detail: err.message }
  }

  function startTransfer(op: 'copy' | 'move', from: Pane, entries: Entry[], to: Pane) {
    if (entries.length === 0) {
      notice = { ok: false, text: ko.files.selectFirst }
      return Promise.resolve()
    }
    if (from.share === '' || to.share === '') {
      notice = { ok: false, text: ko.files.pickShares }
      return Promise.resolve()
    }
    if (!canDrop(from, to)) {
      notice = { ok: false, text: ko.files.sameFolder }
      return Promise.resolve()
    }
    // Fix the targets now: navigating while jobs are being started must not change them.
    const srcs = entries.map((e) => targetOf(from, e))
    const dstTarget = targetOf(to)
    const policy = conflict
    return (async () => {
      busy = true
      let started = 0
      try {
        for (const s of srcs) {
          await api.startJob(op, s, dstTarget, policy)
          started++
        }
        notice = { ok: true, text: ko.files.started(started, op === 'copy' ? ko.files.opCopy : ko.files.opMove) }
      } catch (e) {
        fail(e)
      } finally {
        busy = false
      }
    })()
  }

  function onDrop(target: Pane, move: boolean) {
    const from = dragSrc
    const entries = from ? (from.id === 'left' ? leftSel : rightSel) : []
    dragSrc = null
    if (from) void startTransfer(move ? 'move' : 'copy', from, entries, target)
  }

  async function doDelete() {
    if (!dialog) return
    const { pane: p, entries, view } = dialog
    busy = true
    try {
      for (const e of entries) {
        await api.remove(p.peer, p.share, joinPath(p.path, e.Name), e.Type === 'dir')
      }
      notice = null
    } catch (e) {
      fail(e)
    } finally {
      busy = false
      dialog = null
      void view?.loadEntries()
    }
  }

  async function doRename(name: string) {
    if (!dialog) return
    const { pane: p, entries, view } = dialog
    busy = true
    try {
      await api.rename(p.peer, p.share, joinPath(p.path, entries[0].Name), name.trim())
      notice = null
    } catch (e) {
      fail(e)
    } finally {
      busy = false
      dialog = null
      void view?.loadEntries()
    }
  }

  async function doMkdir(name: string) {
    if (!dialog) return
    const { pane: p, view } = dialog
    busy = true
    try {
      await api.mkdir(p.peer, p.share, joinPath(p.path, name.trim()))
      notice = null
    } catch (e) {
      fail(e)
    } finally {
      busy = false
      dialog = null
      void view?.loadEntries()
    }
  }

  function ask(kind: 'delete' | 'rename' | 'mkdir') {
    if (src.share === '') {
      notice = { ok: false, text: ko.files.pickShares }
      return
    }
    if (kind !== 'mkdir' && srcSel.length === 0) {
      notice = { ok: false, text: ko.files.selectFirst }
      return
    }
    if (kind === 'rename' && srcSel.length !== 1) {
      notice = { ok: false, text: ko.files.renameOne }
      return
    }
    dialog = { kind, ...snapshotSelection(src, srcSel), view: srcView }
  }

  $: canMove = canDrop(src, dst) && srcSel.length > 0 && !busy
  $: shownNames = (dialog?.entries ?? []).slice(0, 5).map((e) => e.Name)
</script>

<div class="files">
  <div class="toolbar row">
    <button disabled={!canMove} on:click={() => startTransfer('copy', src, srcSel, dst)}>{ko.files.copyTo}</button>
    <button disabled={!canMove} on:click={() => startTransfer('move', src, srcSel, dst)}>{ko.files.moveTo}</button>
    <button class="danger" disabled={busy || srcSel.length === 0} on:click={() => ask('delete')}>{ko.files.del}</button>
    <button disabled={busy || srcSel.length !== 1} on:click={() => ask('rename')}>{ko.files.rename}</button>
    <button disabled={busy || src.share === ''} on:click={() => ask('mkdir')}>{ko.files.mkdir}</button>
    <label class="policy">
      <span class="muted">{ko.files.conflict}</span>
      <select bind:value={conflict}>
        {#each policies as c}
          <option value={c}>{ko.files.conflicts[c]}</option>
        {/each}
      </select>
    </label>
  </div>

  {#if notice}
    <div class="notice" class:bad={!notice.ok}>
      <span>{notice.text}</span>
      {#if notice.detail}<span class="muted mono">{notice.detail}</span>{/if}
      {#if notice.ok}<button on:click={() => dispatch('showTransfers')}>{ko.tabs.transfers}</button>{/if}
      <button on:click={() => (notice = null)}>{ko.common.close}</button>
    </div>
  {/if}

  <div class="panes">
    <PaneView
      bind:this={leftView}
      {api}
      {peers}
      bind:pane={left}
      bind:selected={leftSel}
      active={activeId === 'left'}
      {dragSrc}
      on:activate={() => (activeId = 'left')}
      on:dragstart={() => (dragSrc = left)}
      on:dragend={() => (dragSrc = null)}
      on:drop={(e) => onDrop(left, e.detail.move)}
    />
    <PaneView
      bind:this={rightView}
      {api}
      {peers}
      bind:pane={right}
      bind:selected={rightSel}
      active={activeId === 'right'}
      {dragSrc}
      on:activate={() => (activeId = 'right')}
      on:dragstart={() => (dragSrc = right)}
      on:dragend={() => (dragSrc = null)}
      on:drop={(e) => onDrop(right, e.detail.move)}
    />
  </div>
  <div class="muted hint">{ko.files.dragHint}</div>
</div>

{#if dialog?.kind === 'delete'}
  <Dialog
    title={ko.files.deleteTitle}
    message={ko.files.deleteMessage}
    names={shownNames}
    extra={dialog.entries.length > 5 ? ko.files.deleteMore(dialog.entries.length - 5) : ''}
    confirmLabel={ko.files.deleteConfirm}
    danger
    {busy}
    on:confirm={doDelete}
    on:cancel={() => (dialog = null)}
  />
{:else if dialog?.kind === 'rename'}
  <Dialog
    title={ko.files.renameTitle}
    names={[dialog.entries[0].Name]}
    input={dialog.entries[0].Name}
    inputLabel={ko.files.renameLabel}
    confirmLabel={ko.files.change}
    {busy}
    on:confirm={(e) => doRename(e.detail)}
    on:cancel={() => (dialog = null)}
  />
{:else if dialog?.kind === 'mkdir'}
  <Dialog
    title={ko.files.mkdirTitle}
    input=""
    inputLabel={ko.files.mkdirLabel}
    confirmLabel={ko.files.create}
    {busy}
    on:confirm={(e) => doMkdir(e.detail)}
    on:cancel={() => (dialog = null)}
  />
{/if}

<style>
  .files { display: flex; flex-direction: column; height: 100%; min-height: 0; gap: 8px; }
  .toolbar .policy { margin-left: auto; display: flex; gap: 6px; align-items: center; }
  .panes { flex: 1; min-height: 0; display: flex; gap: 10px; }
  .notice { display: flex; gap: 10px; align-items: center; padding: 6px 10px; border-radius: 6px; background: var(--row-hover); }
  .notice.bad { background: var(--danger-bg); color: var(--danger); }
  .hint { font-size: 12px; }
</style>
