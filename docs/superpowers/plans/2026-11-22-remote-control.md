# sugar-it 원격 제어 구현 계획

**Spec:** `docs/superpowers/specs/2026-11-22-remote-control-design.md` (이하 "스펙"). 값이 충돌하면 스펙을 따르고, 스펙을 바꿔야 하면 스펙 14절에 결정을 남긴 뒤 고친다.

**Goal:** Windows 컨트롤러가 Tailscale을 거쳐 Windows(잠금 화면 포함)와 macOS의 화면을 sugar-it 앱 하나로 보고 조작한다.

**진행 표시:** 끝난 항목은 `- [x]`로 바꾸고, 한 줄로 결과(커밋, 미검증 사항)를 붙인다.

## Global Constraints

- 기본 계획(`2026-11-02-sugar-it.md`)의 Global Constraints를 그대로 따른다. 특히 루트 모듈 `CGO_ENABLED=0 GOOS={linux,darwin,windows} go build ./...`가 매 태스크 끝에 통과해야 한다.
- 오류 코드는 기본 설계 6절의 것만 쓴다.
- 원격 제어 설정은 Control API로만 바꾼다. Peer API에 설정 경로를 만들지 않는다.
- 터널 대상 주소는 대상 호스트 설정에서만 오며 루프백이어야 한다.
- 접속 기록에 RFB 내용, 암호, 토큰을 남기지 않는다.
- Windows·macOS 고유 동작은 이 컨테이너(Linux)에서 실행할 수 없다. 크로스 빌드까지 확인하고, 실행 확인은 사용자에게 `docs/manual-test.md` 절차로 넘기며 미검증으로 표시한다.

## Review Focus

1. 허용되지 않은 멤버, 재페어링한 멤버, Tailscale 밖 주소가 터널을 열 수 없어야 한다. (Task 1.1, 1.2)
2. 멤버 제거, 데몬 종료, `connect` 요청 종료 때 터널과 리스너가 남지 않아야 한다. (Task 1.2, 1.3)
3. 터널이 대상 설정 밖의 주소로 연결되지 않아야 한다. (Task 1.1, 1.2)

---

## 1단계: 기반 (스펙 4절)

- [x] **Task 1.1 설정과 정책** — `internal/remote` 패키지: `Config`(enabled, allow{id,epoch}, target, networks), OS 기본 대상, 루프백 대상 검증, 허용 네트워크 파싱(기본 Tailscale), `Allowed(member, addr)` 판정. `config.Config.Remote` 필드. 단위 테스트.
- [x] **Task 1.2 Peer 터널** — `POST /v1/remote/tunnel` 서버(HTTP/2 확인, 검사 순서, 동시 4개, 대상 다이얼 5초, 양방향 복사와 flush, 호출자 제거 시 종료), 접속 기록 `remote.log`(회전 포함), `peerapi.Client.Tunnel`, 허용 네트워크 주소만 다이얼하는 원격 제어용 클라이언트. 두 데몬 + 에코 서버 테스트: 왕복, 꺼짐, 허용 안 됨, Epoch 불일치, 네트워크 밖, 대상 없음, 제거 시 끊김.
- [x] **Task 1.3 Control API와 CLI** — `GET /v1/remote`, `POST /v1/remote/settings`, `/allow`, `/deny`, `/connect`(요청 수명 동안만 리스너 유지). `sugar-it remote status|enable|disable|allow|deny|target|connect`. CLI 테스트와 README, 기본 설계에 원격 제어 링크.
- [x] **Task 1.4 수동 확인 절차** — `docs/manual-test.md`에 Tailscale 위에서 Mac 화면 공유를 `sugar-it remote connect` + VNC 뷰어로 여는 절차(S3 일부). 사용자 실행 대기.

**1단계 결과:** Linux에서 `go test -race ./...` 통과, 3개 OS × 2개 아키텍처 `CGO_ENABLED=0` 빌드 통과, 실제 바이너리 두 개로 거부, 허용, 왕복, 접속 기록, Ctrl+C 종료 확인. **미검증:** 실제 Tailscale과 macOS 화면 공유(`docs/manual-test.md`의 "Remote control tunnel to a Mac" 절차, 사용자 실행 대기). 테스트 위치: `internal/remote`, `internal/peerapi/tunnel_test.go`, `internal/control/remote_test.go`, `internal/cli/remote_test.go`.

## 0단계 스파이크 (스펙 9절)

- [ ] **S1 Windows 잠금 화면 PoC** — `cmd/remote-spike`(Windows 전용 빌드 태그): 서비스 설치·제거, 콘솔 세션에 SYSTEM 에이전트 실행, 입력 데스크톱 추적, 주기적 스크린샷 파일 저장, 지정 문자열 입력, `SendSAS`. 크로스 빌드 후 사용자가 실행하고 결과를 스펙 9절과 14절에 기록.
- [ ] **S2 RFB 성능** — Task 3.2 초안으로 측정.
- [ ] **S3 Mac 화면 공유 + noVNC** — Task 1.4 이후 noVNC로 반복.
- [ ] **S4 키보드** — Task 2.3 초안으로 측정.

## 2단계: 뷰어와 Mac (스펙 5, 7절)

- [ ] **Task 2.1** 데스크톱 앱의 루프백 WebSocket 브리지(토큰, Origin 검사)와 Wails 바인딩
- [ ] **Task 2.2** noVNC 포함(버전 고정, 라이선스 고지)과 원격 화면 뷰(호스트 목록 → 연결, 상태, 끊기, Ctrl+Alt+Del 버튼)
- [ ] **Task 2.3** Windows 저수준 키보드 훅, 한/영·한자 키, Mac Cmd 매핑
- [ ] **Task 2.4** Mac 대상 상태 확인(RFB 인사말), 화면 공유 켜기 안내, 세션 동안 `caffeinate`
- [ ] **Task 2.5** 원격 제어 설정 UI(허용, 허용 목록, 접속 기록 보기)

## 3단계: Windows 에이전트 (스펙 6절, S1 통과 후)

- [ ] **Task 3.1** 원격 서비스: SCM 등록·제거, 콘솔 세션 추적, SYSTEM 에이전트 실행과 재시작, `SendSAS`
- [ ] **Task 3.2** RFB 서버(순수 Go): 핸드셰이크, Raw/CopyRect/Tight, 포인터·키 이벤트, 클립보드
- [ ] **Task 3.3** 캡처(DXGI, GDI 대체)와 입력 데스크톱 전환, `SendInput`
- [ ] **Task 3.4** 연결 상대 확인(스펙 6.1), 깨어 있음 유지
- [ ] **Task 3.5** NSIS 설치·제거와 업데이트 도우미의 서비스 중지·재시작

## 4단계: 품질

- [ ] 다중 모니터, 화질 적응, 접속 알림 UI, 재부팅 후 로그인 화면 검토
