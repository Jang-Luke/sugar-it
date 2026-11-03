import type { Entry } from './api'

/** One side of the file view: a host, a share on it and a folder inside. */
export interface Pane {
  id: string // 'left' or 'right'
  peer: string // 'local' or a host name
  share: string // '' while no share is chosen
  path: string // '' for the share root, else 'a/b' with no leading or trailing slash
}

export function joinPath(dir: string, name: string): string {
  return dir === '' ? name : dir + '/' + name
}

/**
 * The "peer:share/path" string the Control API takes. Without an entry it names
 * the pane's folder (trailing slash: a destination directory); with one it
 * names that entry inside the folder. A pane without a share has no target.
 */
export function targetOf(p: Pane, entry?: Entry): string {
  if (p.share === '') return ''
  const base = `${p.peer}:${p.share}/`
  if (entry) return base + joinPath(p.path, entry.Name)
  return base + (p.path === '' ? '' : p.path + '/')
}

/** Whether something dragged from src may be dropped on dst. */
export function canDrop(src: Pane, dst: Pane): boolean {
  if (src.share === '' || dst.share === '') return false
  if (src.id === dst.id) return false
  return !(src.peer === dst.peer && src.share === dst.share && src.path === dst.path)
}

/** What a dialog acts on: the selection and the folder it was made in, frozen. */
export function snapshotSelection(p: Pane, entries: Entry[]): { pane: Pane; entries: Entry[] } {
  return { pane: { ...p }, entries: [...entries] }
}

/**
 * Explorer's convention. Shift always moves and Ctrl always copies (when both
 * are held, copy: it destroys nothing). With neither, a drop inside the same
 * peer and share is a move and a drop anywhere else is a copy.
 */
export function dropOperation(
  src: Pane,
  dst: Pane,
  mods: { shift: boolean; ctrl: boolean },
): 'copy' | 'move' {
  if (mods.ctrl) return 'copy'
  if (mods.shift) return 'move'
  return src.peer === dst.peer && src.share === dst.share ? 'move' : 'copy'
}

export type DropRefusal = 'noShare' | 'empty' | 'self' | 'noop'
export type DropPlan = { ok: true; dest: Pane; target: string } | { ok: false; reason: DropRefusal }

/**
 * Where a drop of `entries` (listed directly in `src`) onto `dst` goes. Dropped
 * on a folder row (`over`) it goes into that folder; on anything else, into the
 * pane's own folder. `dest` and `target` are copies: they do not change when
 * a pane navigates or refreshes afterwards. `target` always ends with "/", so
 * the spec's cp rule puts the entries under it.
 *
 * Refused: nothing dragged, a pane without a share, a target that is one of the
 * dragged entries or inside one, and a target that already holds the entries.
 */
export function planDrop(src: Pane, entries: Entry[], dst: Pane, over?: Entry | null): DropPlan {
  if (entries.length === 0) return { ok: false, reason: 'empty' }
  if (src.share === '' || dst.share === '') return { ok: false, reason: 'noShare' }
  const dest: Pane = over && over.Type === 'dir' ? { ...dst, path: joinPath(dst.path, over.Name) } : { ...dst }
  const sameShare = src.peer === dest.peer && src.share === dest.share
  if (sameShare) {
    for (const e of entries) {
      const p = joinPath(src.path, e.Name)
      if (dest.path === p || dest.path.startsWith(p + '/')) return { ok: false, reason: 'self' }
    }
    if (dest.path === src.path) return { ok: false, reason: 'noop' }
  }
  return { ok: true, dest, target: targetOf(dest) }
}
