import { describe, expect, it } from 'vitest'
import { ko } from '../ko'
import type { MountResult } from './api'
import { describeMount, mountFailure, serviceFailure } from './mount'

const base: MountResult = { Notes: null, Output: '', NeedsToken: false, Drive: '', Blocked: false, AdminCommands: null }

describe('describeMount', () => {
  it('a blocked mount carries the commands and is not a failure', () => {
    const v = describeMount({ ...base, Blocked: true, AdminCommands: ['reg add X', 'net stop webclient'] })
    expect(v).toEqual({ kind: 'blocked', commands: ['reg add X', 'net stop webclient'] })
  })
  it('reports the assigned drive', () => {
    expect(describeMount({ ...base, Drive: 'Y:' })).toEqual({ kind: 'mounted', message: ko.hosts.mountedOn('Y:') })
  })
  it('falls back to the plain done message', () => {
    expect(describeMount(base)).toEqual({ kind: 'mounted', message: ko.hosts.mountDone })
  })
})

describe('mountFailure', () => {
  it('shows the mount-failed heading and the detail, not the internal error text', () => {
    const f = mountFailure(new Error('{"code":"internal","message":"Windows rejected the credentials (system error 86)"}'))
    expect(f.heading).toBe(ko.hosts.mountFailed)
    expect(f.heading).not.toBe(ko.errors.internal)
    expect(f.detail).toBe('Windows rejected the credentials (system error 86)')
  })
  it('a plain string error keeps its text', () => {
    expect(mountFailure('gio failed').detail).toBe('gio failed')
  })
  it('keeps the known error code text for real coded errors', () => {
    const f = mountFailure('{"code":"peer_offline","message":"x"}')
    expect(f.heading).toBe(ko.errors.peer_offline)
  })
})

describe('serviceFailure', () => {
  it('shows the service-failed heading and the command detail, not the internal error', () => {
    const f = serviceFailure('{"code":"internal","message":"reg add X: exit status 1"}')
    expect(f.heading).toBe(ko.hosts.serviceFailed)
    expect(f.heading).not.toBe(ko.errors.internal)
    expect(f.detail).toBe('reg add X: exit status 1')
  })
  it('says on Windows that the service starts at the next logon', () => {
    expect(ko.hosts.serviceNextLogon).toContain('다음 로그인')
  })
})
