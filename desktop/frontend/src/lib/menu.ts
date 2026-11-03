import type { Entry } from './api'
import { canDrop, type Pane } from './panes'

/**
 * What a right click acts on. A row outside the selection becomes the only
 * selected row; a row inside it keeps the selection; empty space changes nothing.
 */
export function contextSelection(selected: Entry[], row: Entry | null): Entry[] {
  if (row === null) return selected
  return selected.includes(row) ? selected : [row]
}

export interface ActionStates {
  open: boolean
  copy: boolean
  move: boolean
  del: boolean
  rename: boolean
  mkdir: boolean
}

/** Which actions are possible. The toolbar buttons and the context menu both use this. */
export function actionStates(c: { src: Pane; dst: Pane; sel: Entry[]; busy: boolean }): ActionStates {
  const { src, dst, sel, busy } = c
  const move = canDrop(src, dst) && sel.length > 0 && !busy
  return {
    open: !busy && sel.length === 1 && sel[0].Type === 'dir',
    copy: move,
    move,
    del: !busy && sel.length > 0,
    rename: !busy && sel.length === 1,
    mkdir: !busy && src.share !== '',
  }
}

/** The top-left corner for a menu of `size` opened at `at`, kept inside the window. */
export function clampMenu(
  at: { x: number; y: number },
  size: { w: number; h: number },
  view: { w: number; h: number },
): { x: number; y: number } {
  return {
    x: Math.max(0, Math.min(at.x, view.w - size.w)),
    y: Math.max(0, Math.min(at.y, view.h - size.h)),
  }
}
