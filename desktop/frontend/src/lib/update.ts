// What the update banner and the 지금 확인 button show. Only versions and codes come
// from the Go side; release text never reaches the window.

import { ko } from '../ko'
import type { UpdateInfo } from './api'

/** The banner shows a newer version unless 나중에 hid that version, and while an update runs. */
export function showBanner(info: UpdateInfo | undefined, dismissed: string): boolean {
  if (!info) return false
  return updateBusy(info) || (info.Available && info.Latest !== dismissed)
}

export function updateBusy(info: UpdateInfo | undefined): boolean {
  return !!info && info.Phase !== ''
}

export function progressText(info: UpdateInfo): string {
  if (info.Phase === 'downloading') {
    const pct = info.Total > 0 ? Math.min(100, Math.floor((info.Done / info.Total) * 100)) : null
    return ko.update.downloading(pct)
  }
  return info.Phase === 'preparing' ? ko.update.preparing : ''
}

export function updateErrorText(code: string): string {
  if (Object.hasOwn(ko.update.errors, code)) return ko.update.errors[code as keyof typeof ko.update.errors]
  return Object.hasOwn(ko.errors, code) ? ko.errors[code as keyof typeof ko.errors] : ko.errors.unknown
}

/** Why the update before this start failed; the helper has no window to say so. */
export function noticeText(code: string): string {
  return Object.hasOwn(ko.update.notices, code)
    ? ko.update.notices[code as keyof typeof ko.update.notices]
    : ko.update.notices.other
}

export function checkMessage(info: UpdateInfo): string {
  return info.Available ? ko.update.newer(info.Latest) : ko.update.upToDate
}
