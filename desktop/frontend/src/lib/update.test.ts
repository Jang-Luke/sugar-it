import { describe, expect, it } from 'vitest'
import { ko } from '../ko'
import type { UpdateInfo } from './api'
import { checkMessage, noticeText, showBanner, updateBusy, updateErrorText, progressText } from './update'

const info = (over: Partial<UpdateInfo> = {}): UpdateInfo => ({
  Current: '1.2.3',
  Latest: '1.2.4',
  Available: true,
  OneClick: true,
  Phase: '',
  Done: 0,
  Total: 0,
  Notice: '',
  NoticeDetail: '',
  ...over,
})

describe('the update banner', () => {
  it('shows a newer version', () => {
    expect(showBanner(info(), '')).toBe(true)
  })
  it('stays away when nothing is newer or nothing is known', () => {
    expect(showBanner(info({ Available: false }), '')).toBe(false)
    expect(showBanner(undefined, '')).toBe(false)
  })
  it('나중에 hides that version only', () => {
    expect(showBanner(info(), '1.2.4')).toBe(false)
    expect(showBanner(info({ Latest: '1.2.5' }), '1.2.4')).toBe(true)
  })
  it('stays while an update runs, even if that version was dismissed', () => {
    expect(showBanner(info({ Phase: 'downloading' }), '1.2.4')).toBe(true)
  })
})

describe('banner text', () => {
  it('names the version on every system', () => {
    for (const p of ['windows', 'darwin', 'linux']) expect(ko.update.available('1.2.4', p, true)).toContain('1.2.4')
  })
  it('says macOS and Linux do not install in place, and why', () => {
    for (const p of ['darwin', 'linux']) {
      const t = ko.update.available('1.2.4', p, false)
      expect(t).toContain('릴리스 페이지')
      expect(t).toContain('서명')
    }
  })
  it('says when a Windows release has no installer yet', () => {
    expect(ko.update.available('1.2.4', 'windows', false)).toContain('설치 파일')
    expect(ko.update.available('1.2.4', 'windows', true)).not.toContain('릴리스 페이지')
  })
})

describe('the confirmation', () => {
  it('says the app restarts and sharing pauses', () => {
    const t = ko.update.confirmMessage(0)
    expect(t).toContain('다시 시작')
    expect(t).toContain('공유')
    expect(t).toContain('잠시')
  })
  it('counts active transfers, which resume', () => {
    const t = ko.update.confirmMessage(3)
    expect(t).toContain('3건')
    expect(t).toContain('이어')
    expect(ko.update.confirmMessage(0)).not.toContain('건')
  })
})

describe('progress', () => {
  it('is busy only in a phase', () => {
    expect(updateBusy(info())).toBe(false)
    expect(updateBusy(undefined)).toBe(false)
    expect(updateBusy(info({ Phase: 'downloading' }))).toBe(true)
  })
  it('shows a percentage while downloading', () => {
    expect(progressText(info({ Phase: 'downloading', Done: 50, Total: 200 }))).toContain('25%')
    expect(progressText(info({ Phase: 'downloading', Done: 500, Total: 200 }))).toContain('100%')
  })
  it('copes with an unknown size', () => {
    const t = progressText(info({ Phase: 'downloading', Done: 50, Total: 0 }))
    expect(t).toBe(ko.update.downloading(null))
  })
  it('names the next stage', () => {
    expect(progressText(info({ Phase: 'preparing' }))).toBe(ko.update.preparing)
    expect(progressText(info())).toBe('')
  })
})

describe('update errors', () => {
  it('has text for known codes', () => {
    expect(updateErrorText('update_checksum_mismatch')).toBe(ko.update.errors.update_checksum_mismatch)
  })
  it('falls back to the general errors, and never prints a code', () => {
    expect(updateErrorText('peer_offline')).toBe(ko.errors.peer_offline)
    expect(updateErrorText('toString')).toBe(ko.errors.unknown)
    expect(updateErrorText('whatever')).toBe(ko.errors.unknown)
  })
  it('explains a failure from before the restart', () => {
    expect(noticeText('update_denied')).toBe(ko.update.notices.update_denied)
    expect(noticeText('hasOwnProperty')).toBe(ko.update.notices.other)
  })
})

describe('지금 확인', () => {
  it('says the app is current', () => {
    expect(checkMessage(info({ Available: false }))).toBe(ko.update.upToDate)
  })
  it('names a newer version', () => {
    expect(checkMessage(info())).toContain('1.2.4')
  })
})
