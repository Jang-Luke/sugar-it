// Types of the Go bindings (encoding/json field names) and the Api the views use.

export type EntryType = 'file' | 'dir' | 'symlink' | 'other'

export interface Entry {
  Name: string
  Path: string
  Type: EntryType
  Size: number
  ModTime: string
  Mode: number
  ETag: string
}

export interface PeerStatus {
  ID: string
  Name: string
  Addrs: string[] | null
  Online: boolean
  Incompatible: boolean
  LastSeen: string
}

export interface Status {
  ID: string
  Name: string
  Peers: PeerStatus[] | null
  Version: string
  InProcess: boolean
}

export interface AppState {
  InProcess: boolean
  StartError: string
  Failure: string
  Platform: string
}

export interface ShareInfo {
  Name: string
  ReadOnly: boolean
}

export interface ItemError {
  Path: string
  Code: string
  Message: string
}

export interface Endpoint {
  Peer: string
  Share: string
  Path: string
}

export type JobState = 'queued' | 'running' | 'waiting_peer' | 'done' | 'partial' | 'failed' | 'canceled'
export type Conflict = 'fail' | 'skip' | 'overwrite' | 'rename'

export interface Job {
  ID: string
  Host: string
  Origin: string
  Spec: { Op: string; Src: Endpoint; Dst: Endpoint; OnConflict: string }
  State: JobState
  Error: ItemError | null
  TotalFiles: number
  DoneFiles: number
  SkippedFiles: number
  FailedFiles: number
  TotalBytes: number
  DoneBytes: number
  BytesPerSec: number
  Failures: ItemError[] | null
  Created: string
  Updated: string
}

export interface MountResult {
  Notes: string[] | null
  Output: string
  NeedsToken: boolean
}

export interface ServiceResult {
  Output: string
}

export interface ApiError {
  code: string
  message: string
}

export interface Api {
  status(): Promise<Status>
  appState(): Promise<AppState>
  invite(ttlSeconds: number): Promise<string>
  joinPeer(invite: string): Promise<PeerStatus>
  removePeer(name: string): Promise<void>
  renameSelf(newName: string): Promise<void>
  listShares(peer: string): Promise<ShareInfo[]>
  addShare(name: string, path: string, readOnly: boolean): Promise<void>
  removeShare(name: string): Promise<void>
  list(peer: string, share: string, path: string): Promise<Entry[]>
  mkdir(peer: string, share: string, path: string): Promise<void>
  remove(peer: string, share: string, path: string, recursive: boolean): Promise<void>
  rename(peer: string, share: string, path: string, newName: string): Promise<void>
  startJob(op: 'copy' | 'move', src: string, dst: string, onConflict: Conflict): Promise<Job>
  jobs(): Promise<Job[]>
  cancelJob(host: string, id: string): Promise<void>
  retryJob(host: string, id: string): Promise<void>
  pickFolder(title: string): Promise<string>
  mount(drive: string): Promise<MountResult>
  unmount(drive: string): Promise<MountResult>
  /** Opens a native dialog with the token; the token never reaches this code. */
  showMountToken(title: string, message: string): Promise<void>
  installService(): Promise<ServiceResult>
  uninstallService(): Promise<ServiceResult>
}

/** Binding errors are JSON {"code","message"}; anything else is an internal error. */
export function parseError(e: unknown): ApiError {
  const raw = typeof e === 'string' ? e : e instanceof Error ? e.message : String(e)
  try {
    const v = JSON.parse(raw)
    if (v && typeof v.code === 'string' && v.code !== '') {
      return { code: v.code, message: typeof v.message === 'string' ? v.message : '' }
    }
  } catch {
    // not JSON
  }
  return { code: 'internal', message: raw }
}

type Bound = Record<string, (...args: unknown[]) => Promise<unknown>>

function bound(): Bound {
  const w = window as unknown as { go?: { main?: { App?: Bound } } }
  const app = w.go?.main?.App
  if (!app) throw new Error('Wails bindings are not available')
  return app
}

/** The Api backed by the Wails bindings of the Go App. */
export function wailsApi(): Api {
  const c = <T>(name: string, ...args: unknown[]) => bound()[name](...args) as Promise<T>
  return {
    status: () => c('Status'),
    appState: () => c('AppState'),
    invite: (ttl) => c('Invite', ttl),
    joinPeer: (inv) => c('JoinPeer', inv),
    removePeer: (n) => c('RemovePeer', n),
    renameSelf: (n) => c('RenameSelf', n),
    listShares: (p) => c('ListShares', p),
    addShare: (n, p, ro) => c('AddShare', n, p, ro),
    removeShare: (n) => c('RemoveShare', n),
    list: (p, s, path) => c('List', p, s, path),
    mkdir: (p, s, path) => c('Mkdir', p, s, path),
    remove: (p, s, path, r) => c('Remove', p, s, path, r),
    rename: (p, s, path, n) => c('Rename', p, s, path, n),
    startJob: (op, src, dst, oc) => c('StartJob', op, src, dst, oc),
    jobs: () => c('Jobs'),
    cancelJob: (h, id) => c('CancelJob', h, id),
    retryJob: (h, id) => c('RetryJob', h, id),
    pickFolder: (t) => c('PickFolder', t),
    mount: (d) => c('Mount', d),
    unmount: (d) => c('Unmount', d),
    showMountToken: (t, m) => c('ShowMountToken', t, m),
    installService: () => c('InstallService'),
    uninstallService: () => c('UninstallService'),
  }
}
