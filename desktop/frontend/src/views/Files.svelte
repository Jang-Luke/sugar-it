<script lang="ts">
  import { createEventDispatcher } from 'svelte'
  import { ko } from '../ko'
  import { parseError, type Api, type ApiError, type Conflict, type Entry, type Job, type PeerStatus } from '../lib/api'
  import { deletePrompt, isMacPlatform, dropOperation, joinPath, planDrop, snapshotSelection, targetOf, type DropRefusal, type Pane } from '../lib/panes'
  import { actionStates, contextSelection } from '../lib/menu'
  import { jobsChanged } from '../lib/refresh'
  import { errorText } from '../lib/transfers'
  import ContextMenu from '../components/ContextMenu.svelte'
  import Dialog from '../components/Dialog.svelte'
  import PaneView from '../components/PaneView.svelte'

  export let api: Api
  export let peers: PeerStatus[] = []
  export let jobs: Job[] = []
  export let selfId = ''

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
  let dragEntries: Entry[] = []
  let notice: { ok: boolean; text: string; detail?: string } | null = null
  // A dialog acts on what was selected when it opened, not on what the pane shows later.
  let dialog: null | { kind: 'delete' | 'rename' | 'mkdir'; pane: Pane; entries: Entry[]; view: PaneView } = null
  let busy = false
  // The context menu acts on what was selected when it opened.
  let menu: null | { x: number; y: number; side: 'left' | 'right'; pane: Pane; entries: Entry[]; row: Entry | null } = null

  $: src = activeId === 'left' ? left : right
  $: dst = activeId === 'left' ? right : left
  $: srcSel = activeId === 'left' ? leftSel : rightSel
  $: srcView = activeId === 'left' ? leftView : rightView

  // Refresh both panes, silently, whenever a job touching what they show starts,
  // progresses, changes state or disappears, so a job that finishes between two
  // polls of the job list is not missed either.
  let jobSigs = new Map<string, string>()
  function onJobs(js: Job[]) {
    const r = jobsChanged(jobSigs, js, [left, right], selfId, peers)
    jobSigs = r.next
    if (r.changed) {
      leftView?.requestRefresh()
      rightView?.requestRefresh()
    }
  }
  $: onJobs(jobs)

  function fail(e: unknown) {
    const err: ApiError = parseError(e)
    notice = { ok: false, text: errorText(err.code), detail: err.message }
  }

  function refusalText(r: DropRefusal): string {
    return r === 'empty' ? ko.files.selectFirst : r === 'noShare' ? ko.files.pickShares : r === 'self' ? ko.files.dropSelf : ko.files.sameFolder
  }

  /** `over` is the folder row dropped on, if any; otherwise `to`'s own folder is the target. */
  function startTransfer(op: 'copy' | 'move', from: Pane, entries: Entry[], to: Pane, over: Entry | null = null) {
    const plan = planDrop(from, entries, to, over)
    if (!plan.ok) {
      notice = { ok: false, text: refusalText(plan.reason) }
      return Promise.resolve()
    }
    // Fix the sources and the target now: navigating or refreshing while jobs
    // are being started must not change them. The target ends with "/".
    const srcs = entries.map((e) => targetOf(from, e))
    const dstTarget = plan.target
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

  function onDrop(target: Pane, d: { shift: boolean; ctrl: boolean; over: Entry | null }) {
    // Copies: the panes may navigate or refresh while jobs start.
    const from = dragSrc ? { ...dragSrc } : null
    const entries = [...dragEntries]
    const to = { ...target }
    dragSrc = null
    dragEntries = []
    if (!from) return
    const plan = planDrop(from, entries, to, d.over)
    if (!plan.ok) {
      notice = { ok: false, text: refusalText(plan.reason) }
      return
    }
    void startTransfer(dropOperation(from, plan.dest, d), from, entries, to, d.over)
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

  function askFor(kind: 'delete' | 'rename' | 'mkdir', p: Pane, sel: Entry[], view: PaneView) {
    if (p.share === '') {
      notice = { ok: false, text: ko.files.pickShares }
      return
    }
    if (kind !== 'mkdir' && sel.length === 0) {
      notice = { ok: false, text: ko.files.selectFirst }
      return
    }
    if (kind === 'rename' && sel.length !== 1) {
      notice = { ok: false, text: ko.files.renameOne }
      return
    }
    dialog = { kind, ...snapshotSelection(p, sel), view }
  }

  const ask = (kind: 'delete' | 'rename' | 'mkdir') => askFor(kind, src, srcSel, srcView)

  $: states = actionStates({ src, dst, sel: srcSel, busy })

  function openMenu(side: 'left' | 'right', d: { x: number; y: number; row: Entry | null }) {
    activeId = side
    const sel = contextSelection(side === 'left' ? leftSel : rightSel, d.row)
    if (side === 'left') leftSel = sel
    else rightSel = sel
    menu = { x: d.x, y: d.y, side, pane: { ...(side === 'left' ? left : right) }, entries: [...sel], row: d.row }
  }

  $: menuItems = menu ? menuList(menu, busy, left, right) : []
  function menuList(m: NonNullable<typeof menu>, b: boolean, l: Pane, r: Pane) {
    const other = m.side === 'left' ? r : l
    if (m.row === null) {
      const s = actionStates({ src: m.pane, dst: other, sel: m.entries, busy: b })
      return [
        { id: 'mkdir', label: ko.files.mkdir, enabled: s.mkdir },
        { id: 'refresh', label: ko.common.refresh, enabled: true },
      ]
    }
    const s = actionStates({ src: m.pane, dst: other, sel: m.entries, busy: b })
    return [
      { id: 'open', label: ko.files.menu.open, enabled: s.open },
      { id: 'copy', label: ko.files.menu.copyOther, enabled: s.copy },
      { id: 'move', label: ko.files.menu.moveOther, enabled: s.move },
      { id: 'rename', label: ko.files.rename, enabled: s.rename },
      { id: 'delete', label: ko.files.del, enabled: s.del, danger: true },
    ]
  }

  function menuSelect(id: string) {
    const m = menu
    menu = null
    if (!m) return
    const view = m.side === 'left' ? leftView : rightView
    const other = m.side === 'left' ? right : left
    switch (id) {
      case 'open':
        if (m.entries.length === 1) view?.openFolder(m.entries[0].Name)
        break
      case 'copy':
        void startTransfer('copy', m.pane, m.entries, other)
        break
      case 'move':
        void startTransfer('move', m.pane, m.entries, other)
        break
      case 'rename':
        askFor('rename', m.pane, m.entries, view)
        break
      case 'delete':
        askFor('delete', m.pane, m.entries, view)
        break
      case 'mkdir':
        askFor('mkdir', m.pane, m.entries, view)
        break
      case 'refresh':
        void view?.refresh()
        break
    }
  }

  $: paused = dialog !== null || menu !== null
  $: prompt = deletePrompt(dialog?.entries ?? [])
</script>

<div class="files">
  <div class="toolbar row">
    <button disabled={!states.copy} on:click={() => startTransfer('copy', src, srcSel, dst)}>{ko.files.copyTo}</button>
    <button disabled={!states.move} on:click={() => startTransfer('move', src, srcSel, dst)}>{ko.files.moveTo}</button>
    <button class="danger" disabled={!states.del} on:click={() => ask('delete')}>{ko.files.del}</button>
    <button disabled={!states.rename} on:click={() => ask('rename')}>{ko.files.rename}</button>
    <button disabled={!states.mkdir} on:click={() => ask('mkdir')}>{ko.files.mkdir}</button>
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
      {dragEntries}
      {paused}
      on:activate={() => (activeId = 'left')}
      on:dragstart={(e) => ((dragSrc = { ...left }), (dragEntries = e.detail))}
      on:dragend={() => ((dragSrc = null), (dragEntries = []))}
      on:drop={(e) => onDrop(left, e.detail)}
      on:contextmenu={(e) => openMenu('left', e.detail)}
    />
    <PaneView
      bind:this={rightView}
      {api}
      {peers}
      bind:pane={right}
      bind:selected={rightSel}
      active={activeId === 'right'}
      {dragSrc}
      {dragEntries}
      {paused}
      on:activate={() => (activeId = 'right')}
      on:dragstart={(e) => ((dragSrc = { ...right }), (dragEntries = e.detail))}
      on:dragend={() => ((dragSrc = null), (dragEntries = []))}
      on:drop={(e) => onDrop(right, e.detail)}
      on:contextmenu={(e) => openMenu('right', e.detail)}
    />
  </div>
  <div class="muted hint">{isMacPlatform(navigator.platform) ? ko.files.dragHintMac : ko.files.dragHint}</div>
</div>

{#if menu}
  <ContextMenu x={menu.x} y={menu.y} label={ko.files.menu.label} items={menuItems} on:select={(e) => menuSelect(e.detail)} on:close={() => (menu = null)} />
{/if}

{#if dialog?.kind === 'delete'}
  <Dialog
    title={ko.files.deleteTitle}
    message={prompt.hasFolder ? ko.files.deleteMessageFolders : ko.files.deleteMessage}
    names={prompt.names}
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
