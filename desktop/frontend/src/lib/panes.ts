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
