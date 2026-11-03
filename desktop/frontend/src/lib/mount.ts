import { ko } from '../ko'
import { parseError, type MountResult } from './api'

export type MountView =
  | { kind: 'blocked'; commands: string[] }
  | { kind: 'mounted'; message: string }

/** What the Hosts view shows for a mount that returned without an error. */
export function describeMount(r: MountResult): MountView {
  if (r.Blocked) return { kind: 'blocked', commands: r.AdminCommands ?? [] }
  return { kind: 'mounted', message: r.Drive ? ko.hosts.mountedOn(r.Drive) : ko.hosts.mountDone }
}

/**
 * A failed mount. Errors that carry a known code keep that code's text; the
 * rest, which the host reports as "internal" for a failing OS command, get the
 * mount-failed heading plus the detail instead of "internal error".
 */
export function mountFailure(e: unknown): { heading: string; detail: string } {
  return commandFailure(e, ko.hosts.mountFailed)
}

/** A failed service install or uninstall, shown like a failed mount. */
export function serviceFailure(e: unknown): { heading: string; detail: string } {
  return commandFailure(e, ko.hosts.serviceFailed)
}

function commandFailure(e: unknown, failedHeading: string): { heading: string; detail: string } {
  const { code, message } = parseError(e)
  if (code !== 'internal' && code in ko.errors) {
    return { heading: ko.errors[code as keyof typeof ko.errors], detail: '' }
  }
  return { heading: failedHeading, detail: message }
}
