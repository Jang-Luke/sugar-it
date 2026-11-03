export interface WindowInput {
  scrollTop: number
  viewport: number // visible height in px
  rowHeight: number // fixed row height in px
  total: number // number of rows
  overscan: number // extra rows rendered above and below
}

export interface RowWindow {
  start: number // first rendered row
  end: number // one past the last rendered row
  offsetTop: number // px from the top of the list to the first rendered row
  totalHeight: number
}

/** Which rows of a fixed-height list to render for a scroll position. */
export function windowRange(i: WindowInput): RowWindow {
  const total = Math.max(0, Math.floor(i.total))
  const rowHeight = Math.max(1, i.rowHeight)
  const totalHeight = total * rowHeight
  if (total === 0) return { start: 0, end: 0, offsetTop: 0, totalHeight: 0 }
  const top = Math.min(Math.max(0, i.scrollTop), totalHeight)
  const first = Math.min(total - 1, Math.floor(top / rowHeight))
  const visible = Math.ceil(Math.max(0, i.viewport) / rowHeight) + 1
  const over = Math.max(0, Math.floor(i.overscan))
  const start = Math.max(0, first - over)
  const end = Math.min(total, first + visible + over)
  return { start, end, offsetTop: start * rowHeight, totalHeight }
}
