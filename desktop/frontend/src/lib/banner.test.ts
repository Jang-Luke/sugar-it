import { describe, expect, it } from 'vitest'
import { ko } from '../ko'

describe('in-process banner', () => {
  it('says closing stops sharing only where closing quits', () => {
    expect(ko.banner.inProcess('linux', false)).toContain('앱을 닫으면')
    expect(ko.banner.inProcess('windows', false)).toContain('앱을 닫으면')
  })
  it('says closing the window keeps sharing and quitting stops it where closing hides', () => {
    const w = ko.banner.inProcess('windows', true)
    expect(w).toContain('창을 닫아도 공유는 계속')
    expect(w).toContain('트레이')
    const m = ko.banner.inProcess('darwin', true)
    expect(m).toContain('창을 닫아도 공유는 계속')
    expect(m).not.toContain('트레이')
  })
})
