import { parseError, type ApiError, type Entry } from './api'

const collator = new Intl.Collator('ko')

/** Folders first, then by Korean collation. One collator: building one per comparison is slow. */
export function sortEntries(es: Entry[]): Entry[] {
  const rank = (e: Entry) => (e.Type === 'dir' ? 0 : 1)
  return [...es].sort((a, b) => rank(a) - rank(b) || collator.compare(a.Name, b.Name))
}

/**
 * The rows of one folder. Starting a load empties the rows and the selection at
 * once, so nothing from the previous folder can be acted on while the new one
 * loads or if it fails, and only the latest request may fill the rows.
 */
export class EntryLoader {
  entries: Entry[] = []
  selected: Entry[] = []
  error: ApiError | null = null
  /** The last background refresh failed; rows and selection were kept. */
  bgError: ApiError | null = null
  loading = false
  private seq = 0

  constructor(private onChange: () => void) {}

  /** Empty the list and drop any reply still in flight. */
  invalidate() {
    this.seq++
    this.entries = []
    this.selected = []
    this.bgError = null
    this.loading = false
  }

  /**
   * A silent reload of the same folder. It never blanks anything: rows are
   * replaced only if the listing changed, the selection is kept by name, and a
   * failure leaves rows and selection alone (see bgError). A reply is dropped
   * ('discarded') if a navigation, load or invalidate happened since the
   * request, or if `isCurrent` says the pane is no longer on the folder asked
   * for; so it can never bring back rows of a folder the user has left.
   * `getSelected` is read when the reply arrives. `apply`, also read when the
   * reply arrives, says whether rows may change now: while a drag or a dialog
   * or menu holds them it is false and the reply is dropped whole ('discarded');
   * the next poll fetches it again.
   */
  async refresh(
    fetch: () => Promise<Entry[] | null>,
    getSelected: () => Entry[],
    isCurrent: () => boolean = () => true,
    apply: () => boolean = () => true,
  ): Promise<'replaced' | 'same' | 'failed' | 'discarded'> {
    if (this.loading) return 'discarded'
    const my = this.seq
    let es: Entry[]
    try {
      es = sortEntries((await fetch()) ?? [])
    } catch (e) {
      if (my !== this.seq || !isCurrent() || !apply()) return 'discarded'
      this.selected = getSelected()
      if (this.error === null) this.bgError = parseError(e)
      this.onChange()
      return 'failed'
    }
    if (my !== this.seq || !isCurrent() || !apply()) return 'discarded'
    this.selected = getSelected()
    const hadError = this.error !== null || this.bgError !== null
    this.error = null
    this.bgError = null
    if (sameListing(this.entries, es)) {
      if (hadError) this.onChange()
      return 'same'
    }
    this.entries = es
    this.selected = reconcileSelection(getSelected(), es)
    this.onChange()
    return 'replaced'
  }

  async load(fetch: () => Promise<Entry[] | null>): Promise<void> {
    const my = ++this.seq
    this.entries = []
    this.selected = []
    this.error = null
    this.bgError = null
    this.loading = true
    this.onChange()
    try {
      const es = sortEntries((await fetch()) ?? [])
      if (my !== this.seq) return
      this.entries = es
    } catch (e) {
      if (my !== this.seq) return
      this.error = parseError(e)
    }
    this.loading = false
    this.onChange()
  }
}

/** Whether two sorted listings show the same thing. */
export function sameListing(a: Entry[], b: Entry[]): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) {
    const x = a[i], y = b[i]
    if (
      x.Name !== y.Name || x.Type !== y.Type || x.Size !== y.Size ||
      x.ModTime !== y.ModTime || x.Mode !== y.Mode || x.ETag !== y.ETag
    ) return false
  }
  return true
}

/**
 * The rows of `rows` whose names were selected, in the selection's order. A row
 * whose type differs from the selected entry's is a different thing that took
 * the name (a file replaced by a folder), so it is not selected: a delete must
 * never turn recursive behind the user's back. Size or ETag changes of the
 * same type keep the selection.
 */
export function reconcileSelection(selected: Entry[], rows: Entry[]): Entry[] {
  if (selected.length === 0) return []
  const byName = new Map(rows.map((r) => [r.Name, r]))
  const seen = new Set<string>()
  const out: Entry[] = []
  for (const s of selected) {
    const r = byName.get(s.Name)
    if (r && r.Type === s.Type && !seen.has(r.Name)) {
      seen.add(r.Name)
      out.push(r)
    }
  }
  return out
}
