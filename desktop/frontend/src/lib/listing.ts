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
  loading = false
  private seq = 0

  constructor(private onChange: () => void) {}

  /** Empty the list and drop any reply still in flight. */
  invalidate() {
    this.seq++
    this.entries = []
    this.selected = []
    this.loading = false
  }

  async load(fetch: () => Promise<Entry[] | null>): Promise<void> {
    const my = ++this.seq
    this.entries = []
    this.selected = []
    this.error = null
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
