<script lang="ts">
  import { createEventDispatcher, onDestroy, onMount } from 'svelte'
  import { ko } from '../ko'
  import { parseError, type Api, type ApiError, type Entry, type PeerStatus, type ShareInfo } from '../lib/api'
  import { dropOperation, joinPath, planDrop, type Pane } from '../lib/panes'
  import { AutoRefresher } from '../lib/refresh'
  import { errorText } from '../lib/transfers'
  import { formatBytes, formatTime } from '../lib/format'
  import { EntryLoader } from '../lib/listing'
  import VirtualList from './VirtualList.svelte'

  export let api: Api
  export let pane: Pane
  export let peers: PeerStatus[] = []
  export let active = false
  export let selected: Entry[] = []
  export let dragSrc: Pane | null = null // the pane a drag started from, if one is under way
  export let dragEntries: Entry[] = [] // what is being dragged
  export let paused = false // a dialog or menu is open: no background refresh

  const dispatch = createEventDispatcher<{
    activate: void
    dragstart: Entry[]
    dragend: void
    drop: { shift: boolean; ctrl: boolean; over: Entry | null }
    contextmenu: { x: number; y: number; row: Entry | null }
    changed: void
  }>()

  let shares: ShareInfo[] = []
  let entries: Entry[] = []
  let error: ApiError | null = null
  let bgError: ApiError | null = null
  let loading = false
  let sharesSeq = 0 // drops share lists that arrive after a newer request
  // Rows and selection are emptied the moment a load starts; see EntryLoader.
  const loader = new EntryLoader(() => {
    entries = loader.entries
    error = loader.error
    bgError = loader.bgError
    loading = loader.loading
    selected = loader.selected
  })
  let list: VirtualList
  let over = false // the drop would go to this pane's own folder
  let overRow: Entry | null = null // the folder row the drop would go into
  let hint: { text: string; x: number; y: number } | null = null
  let root: HTMLElement

  $: peerNames = ([{ name: 'local', label: ko.common.local as string, note: '' }]).concat(
    peers.map((p) => ({
      name: p.Name,
      label: p.Name,
      note: p.Incompatible ? ko.common.incompatible : p.Online ? '' : ko.common.offline,
    })),
  )

  async function loadShares() {
    const my = ++sharesSeq
    loader.invalidate()
    entries = []
    selected = []
    loading = true
    error = null
    try {
      const s = (await api.listShares(pane.peer)) ?? []
      if (my !== sharesSeq) return
      shares = s
      if (!shares.some((x) => x.Name === pane.share)) pane.share = ''
      if (pane.share === '' && shares.length > 0) pane.share = shares[0].Name
      pane = pane
      loading = false
      if (pane.share !== '') await loadEntries()
    } catch (e) {
      if (my !== sharesSeq) return
      shares = []
      error = parseError(e)
      loading = false
    }
  }

  export async function loadEntries() {
    if (pane.share === '') {
      loader.invalidate()
      entries = []
      selected = []
      return
    }
    const { peer, share, path } = pane
    await loader.load(() => api.list(peer, share, path))
    if (!error) list?.scrollToTop()
  }

  /** Reload what is shown, keeping the folder. */
  export function refresh() {
    return shares.length === 0 || error ? loadShares() : loadEntries()
  }

  /** Reload the same folder silently; see EntryLoader.refresh. */
  async function backgroundRefresh() {
    if (pane.share === '') return
    const { peer, share, path } = pane
    await loader.refresh(
      () => api.list(peer, share, path),
      () => selected,
      () => pane.peer === peer && pane.share === share && pane.path === path,
    )
  }

  const refresher = new AutoRefresher({
    run: backgroundRefresh,
    canRun: () =>
      document.visibilityState === 'visible' &&
      root?.offsetParent !== null && // not on another tab
      !paused &&
      dragSrc === null &&
      pane.share !== '' &&
      shares.length > 0 &&
      !loading,
  })

  /** Refresh soon, silently (a job touching this pane changed). */
  export function requestRefresh() {
    refresher.request()
  }

  /** Open a folder of this pane by name, as a double click does. */
  export function openFolder(name: string) {
    enter({ Name: name, Type: 'dir' } as Entry)
  }

  function retry() {
    return error && shares.length === 0 ? loadShares() : loadEntries()
  }

  function pickPeer(e: Event) {
    sharesSeq++
    pane.peer = (e.target as HTMLSelectElement).value
    pane.share = ''
    pane.path = ''
    pane = pane
    dispatch('activate')
    void loadShares()
  }

  function pickShare(e: Event) {
    pane.share = (e.target as HTMLSelectElement).value
    pane.path = ''
    pane = pane
    dispatch('activate')
    void loadEntries()
  }

  function enter(e: Entry) {
    if (e.Type !== 'dir') return
    pane.path = joinPath(pane.path, e.Name)
    pane = pane
    void loadEntries()
  }

  function up() {
    const i = pane.path.lastIndexOf('/')
    pane.path = i < 0 ? '' : pane.path.slice(0, i)
    pane = pane
    void loadEntries()
  }

  function select(e: Entry, ev: MouseEvent) {
    dispatch('activate')
    if (ev.ctrlKey || ev.metaKey) {
      selected = selected.includes(e) ? selected.filter((x) => x !== e) : [...selected, e]
    } else if (ev.shiftKey && selected.length > 0) {
      const a = entries.indexOf(selected[selected.length - 1])
      const b = entries.indexOf(e)
      selected = entries.slice(Math.min(a, b), Math.max(a, b) + 1)
    } else {
      selected = [e]
    }
  }

  function dragStart(ev: DragEvent, e: Entry) {
    if (!selected.includes(e)) selected = [e]
    ev.dataTransfer?.setData('text/plain', e.Name)
    if (ev.dataTransfer) ev.dataTransfer.effectAllowed = 'copyMove'
    dispatch('dragstart', selected)
  }

  /** What dropping the current drag on this pane, or on one of its rows, would do. */
  function plan(row: Entry | null, ev: DragEvent) {
    if (!dragSrc) return null
    const p = planDrop(dragSrc, dragEntries, pane, row)
    if (!p.ok) return null
    const op = dropOperation(dragSrc, p.dest, { shift: ev.shiftKey, ctrl: ev.ctrlKey })
    return { p, op }
  }

  function dragOver(ev: DragEvent, row: Entry | null) {
    // A refused row must not fall through to the pane's own folder.
    if (row) ev.stopPropagation()
    const r = plan(row, ev)
    if (!r) {
      overRow = null
      over = false
      hint = null
      return
    }
    ev.preventDefault()
    if (ev.dataTransfer) ev.dataTransfer.dropEffect = r.op
    const folder = row !== null && row.Type === 'dir'
    overRow = folder ? row : null
    over = !folder
    const host = r.p.dest.peer === 'local' ? (ko.common.local as string) : r.p.dest.peer
    const where = `${host}:${r.p.dest.share}/${r.p.dest.path}`
    hint = {
      text: ko.files.dropHint(r.op === 'copy' ? ko.files.opCopy : ko.files.opMove, where),
      x: ev.clientX,
      y: ev.clientY,
    }
  }

  function clearDrag() {
    over = false
    overRow = null
    hint = null
  }

  function leavePane(ev: DragEvent) {
    if (!root?.contains(ev.relatedTarget as Node | null)) clearDrag()
  }

  function dropped(ev: DragEvent, row: Entry | null) {
    if (row) ev.stopPropagation()
    const r = plan(row, ev)
    clearDrag()
    if (!r) return
    ev.preventDefault()
    dispatch('drop', { shift: ev.shiftKey, ctrl: ev.ctrlKey, over: row })
  }

  function context(ev: MouseEvent, row: Entry | null) {
    ev.preventDefault()
    ev.stopPropagation()
    dispatch('activate')
    dispatch('contextmenu', { x: ev.clientX, y: ev.clientY, row })
  }

  function visible() {
    if (document.visibilityState === 'visible') refresher.request()
  }

  onMount(() => {
    void loadShares()
    refresher.start()
    document.addEventListener('visibilitychange', visible)
  })
  onDestroy(() => {
    refresher.stop()
    document.removeEventListener('visibilitychange', visible)
  })

  $: crumbs = pane.path === '' ? [] : pane.path.split('/')
  $: readOnly = shares.find((s) => s.Name === pane.share)?.ReadOnly ?? false
</script>

<!-- svelte-ignore a11y-no-static-element-interactions -->
<section
  class="pane"
  class:active
  class:over
  bind:this={root}
  on:mousedown={() => dispatch('activate')}
  on:dragover={(ev) => dragOver(ev, null)}
  on:dragleave={leavePane}
  on:drop={(ev) => dropped(ev, null)}
>
  <div class="head row">
    <label>
      <span class="muted">{ko.files.host}</span>
      <select value={pane.peer} on:change={pickPeer}>
        {#each peerNames as p}
          <option value={p.name}>{p.label}{p.note ? ` (${p.note})` : ''}</option>
        {/each}
      </select>
    </label>
    <label>
      <span class="muted">{ko.files.share}</span>
      <select value={pane.share} on:change={pickShare} disabled={shares.length === 0}>
        {#if shares.length === 0}
          <option value="">{ko.files.noShares}</option>
        {:else if pane.share === ''}
          <option value="">{ko.files.noShare}</option>
        {/if}
        {#each shares as s}
          <option value={s.Name}>{s.Name}{s.ReadOnly ? ` (${ko.common.readOnly})` : ''}</option>
        {/each}
      </select>
    </label>
    <button on:click={() => refresh()} title={ko.common.refresh} disabled={loading}>{ko.common.refresh}</button>
  </div>

  <div class="path row">
    <button on:click={up} disabled={pane.path === '' || pane.share === ''}>{ko.files.up}</button>
    <span class="crumbs mono">
      {pane.share === '' ? '' : pane.share}{#each crumbs as c} / {c}{/each}
    </span>
    {#if readOnly}<span class="chip">{ko.common.readOnly}</span>{/if}
  </div>

  {#if bgError && !error}
    <div class="err">
      <div class="error"><strong>{ko.files.refreshFailed}</strong></div>
      <div class="muted mono">{errorText(bgError.code)}{bgError.message ? ' · ' + bgError.message : ''}</div>
    </div>
  {/if}

  {#if error}
    <div class="err">
      <div class="error"><strong>{errorText(error.code)}</strong></div>
      {#if error.message}<div class="muted mono">{error.message}</div>{/if}
      <button on:click={retry}>{ko.common.retry}</button>
    </div>
  {:else if loading && entries.length === 0}
    <div class="muted pad">{ko.common.loading}</div>
  {:else if pane.share === ''}
    <div class="muted pad">{shares.length === 0 ? ko.files.noShares : ko.files.noShare}</div>
  {:else if entries.length === 0}
    <div class="muted pad">{ko.common.empty}</div>
  {/if}

  <div class="cols muted">
    <span class="c-name">{ko.files.name}</span><span class="c-size">{ko.files.size}</span><span class="c-time">{ko.files.modified}</span>
  </div>
  <!-- svelte-ignore a11y-no-static-element-interactions -->
  <div class="listwrap" on:contextmenu={(ev) => context(ev, null)}>
  <VirtualList bind:this={list} items={error ? [] : entries} rowHeight={28} let:item>
    <div
      class="entry"
      role="option"
      aria-selected={selected.includes(item)}
      tabindex="-1"
      on:keydown={(ev) => ev.key === 'Enter' && enter(item)}
      class:sel={selected.includes(item)}
      class:dropon={overRow === item}
      draggable="true"
      on:click={(ev) => select(item, ev)}
      on:dblclick={() => enter(item)}
      on:dragstart={(ev) => dragStart(ev, item)}
      on:dragend={() => (clearDrag(), dispatch('dragend'))}
      on:dragover={(ev) => dragOver(ev, item)}
      on:drop={(ev) => dropped(ev, item)}
      on:contextmenu={(ev) => context(ev, item)}
    >
      <span class="c-name" title={item.Name}>
        <span class="kind muted">{item.Type === 'dir' ? '▸' : item.Type === 'file' ? '·' : '◦'}</span>{item.Name}
      </span>
      <span class="c-size muted">{item.Type === 'dir' ? '' : formatBytes(item.Size)}</span>
      <span class="c-time muted">{formatTime(item.ModTime)}</span>
    </div>
  </VirtualList>
  </div>
  <div class="foot muted">
    {ko.files.items(entries.length)}{selected.length ? ' · ' + ko.files.selected(selected.length) : ''}
  </div>
</section>

{#if hint}
  <div class="drophint" style="left: {hint.x + 14}px; top: {hint.y + 18}px">{hint.text}</div>
{/if}

<style>
  .pane {
    flex: 1; min-width: 0; display: flex; flex-direction: column;
    background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 8px;
  }
  .pane.active { border-color: var(--accent); }
  .pane.over { outline: 2px dashed var(--accent); }
  .head label { display: flex; align-items: center; gap: 6px; }
  .path { margin: 8px 0 4px; }
  .crumbs { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .err { padding: 8px; background: var(--danger-bg); border-radius: 6px; margin: 6px 0; display: flex; flex-direction: column; gap: 6px; align-items: flex-start; }
  .pad { padding: 8px; }
  .cols, .entry { display: flex; gap: 8px; align-items: center; padding: 0 8px; }
  .cols { font-size: 12px; border-bottom: 1px solid var(--line); padding-bottom: 2px; }
  .entry { height: 100%; cursor: default; user-select: none; }
  .entry:hover { background: var(--row-hover); }
  .entry.sel { background: var(--row-sel); }
  .entry.dropon { outline: 2px solid var(--accent); outline-offset: -2px; background: var(--row-sel); }
  .listwrap { flex: 1; min-height: 0; display: flex; flex-direction: column; }
  .drophint {
    position: fixed; z-index: 40; pointer-events: none; padding: 3px 8px; border-radius: 6px;
    background: var(--panel); border: 1px solid var(--accent); font-size: 12px; white-space: nowrap;
  }
  .c-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .c-size { width: 80px; text-align: right; flex: none; }
  .c-time { width: 130px; flex: none; }
  .kind { display: inline-block; width: 16px; }
  .foot { font-size: 12px; padding-top: 4px; }
</style>
