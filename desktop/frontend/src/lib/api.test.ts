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

  it('reaches the update bindings', async () => {
    const CheckUpdate = vi.fn(async () => ({ Current: '1.2.3' }))
    const ApplyUpdate = vi.fn(async () => undefined)
    const OpenReleasePage = vi.fn(async () => undefined)
    const DismissUpdateNotice = vi.fn(async () => undefined)
    vi.stubGlobal('window', { go: { main: { App: { CheckUpdate, ApplyUpdate, OpenReleasePage, DismissUpdateNotice } } } })
    const api = wailsApi()
    expect((await api.checkUpdate()).Current).toBe('1.2.3')
    await api.applyUpdate()
    await api.openReleasePage()
    await api.dismissUpdateNotice()
    expect(ApplyUpdate).toHaveBeenCalledTimes(1)
    expect(OpenReleasePage).toHaveBeenCalledWith()
    expect(DismissUpdateNotice).toHaveBeenCalledTimes(1)
  })
})
