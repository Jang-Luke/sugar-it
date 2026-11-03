import { afterEach, describe, expect, it, vi } from 'vitest'
import { wailsApi } from './api'

afterEach(() => vi.unstubAllGlobals())

describe('wailsApi', () => {
  it('reaches the host rename and service bindings', async () => {
    const RenameSelf = vi.fn(async () => undefined)
    const UninstallService = vi.fn(async () => ({ Output: 'removed' }))
    vi.stubGlobal('window', { go: { main: { App: { RenameSelf, UninstallService } } } })
    const api = wailsApi()
    await api.renameSelf('new-name')
    expect(RenameSelf).toHaveBeenCalledWith('new-name')
    expect(await api.uninstallService()).toEqual({ Output: 'removed' })
  })
})
