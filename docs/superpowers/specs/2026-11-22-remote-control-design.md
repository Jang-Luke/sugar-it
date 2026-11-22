# sugar-it 원격 제어 설계

파일 공유 메시(`2026-11-02-sugar-it-design.md`, 이하 "기본 설계")를 원격 화면 제어로 넓히는 설계다. 구현은 이 문서를 근거로 하며, 구현 중 바뀐 결정은 이 문서의 14절 "결정 기록"에 남기고 해당 절을 고친다. 단계별 작업 목록은 `docs/superpowers/plans/2026-11-22-remote-control.md`에 있다.

## 1. 확정된 요구사항

| 항목 | 결정 |
|---|---|
| 컨트롤러(조작하는 쪽) | Windows Home. 데스크톱 앱 하나로 모든 것을 한다. |
| 대상: Windows | Windows Home, 유선 이더넷. 원격 화면 보기와 조작. **Win+L 잠금 화면에서도** 동작해야 한다. |
| 대상: macOS | 맥북. 덮개가 열려 있거나 클램쉘 모드(전원, 외부 모니터 연결)로 깨어 있을 때 원격 화면 보기와 조작, 잠금 화면 포함. |
| 대상: Linux | 제외한다. Tailscale 위에서 ssh를 그대로 쓴다. |
| 네트워크 | **Tailscale을 거친 연결만** 허용한다. |
| 목표 | 외부 도구(RDP, VNC 뷰어) 없이 sugar-it 앱 하나로 통합한다. Home 에디션에는 RDP 호스트가 없다. |

**비범위:** Linux 대상, 잠자기 상태의 기기 깨우기(Wake-on-LAN), 재부팅 후 로그인 전 화면(1차), 소리 전달, 파일 끌어 놓기 전달, 다중 사용자, 모바일 컨트롤러, Tailscale 이외 경로의 원격 제어, 코드 서명.

## 2. 구조

```
[컨트롤러: Windows 데스크톱 앱]
  뷰어(noVNC, WebView2) + 저수준 키보드 훅
        │ 루프백 WebSocket 브리지 (토큰)
  [컨트롤러 데몬] ── Control API: POST /v1/remote/connect
        │ Peer API mTLS, HTTP/2 스트림 1개 = RFB 연결 1개
        │ Tailscale 주소로만 다이얼
        ▼
  [대상 데몬] ── Peer API: POST /v1/remote/tunnel
        │ 허용 검사 → 접속 기록 → 대상 주소로 TCP 연결
        ▼ 127.0.0.1의 RFB 서버
   ├─ Windows: sugar-it 원격 에이전트 (SYSTEM 서비스 + 세션 에이전트)
   └─ macOS : 시스템 "화면 공유" (127.0.0.1:5900)
```

- **두 대상 모두 RFB(VNC) 프로토콜을 쓴다.** macOS 화면 공유가 RFB이므로 Windows 에이전트도 RFB 서버로 만든다. 뷰어, 터널, 권한 모델이 하나로 통일된다.
- **터널은 바이트를 그대로 나른다.** 데몬은 RFB를 해석하지 않는다. RFB 인증(macOS 계정 암호 등)은 뷰어와 RFB 서버 사이의 일이다.
- **대상 주소는 대상 호스트가 정한다.** 호출자는 주소를 지정할 수 없으므로 터널이 임의의 포트 프록시가 되지 않는다. 대상 주소는 루프백이어야 한다.
- 네트워크와 메시 신원은 기존 사용자 데몬이 맡는다. Win+L은 잠금이지 로그아웃이 아니므로 잠금 중에도 사용자 데몬이 돈다.

## 3. 보안 모델

기본 설계의 "멤버끼리 완전히 신뢰" 모델에서는 멤버 하나가 뚫리면 공유 폴더가 노출된다. 원격 제어는 그 범위를 기기 전체로 넓히므로 별도의 허용을 요구한다. 로그인 화면에서는 동의할 사람이 없으므로 접속마다 묻는 방식 대신 아래 규칙을 쓴다.

- **기본값은 꺼짐이다.** 원격 제어 허용(`remote.enabled`)과 허용 목록은 대상 호스트의 Control API(루프백과 토큰)로만 바꿀 수 있다. Peer API에는 설정을 바꾸는 경로가 없다.
- **허용 목록은 멤버 ID와 그 멤버의 Epoch 쌍이다.** 목록에 있고, 지금 활성 멤버이며, 현재 Epoch가 저장된 값과 같아야 허용한다. 제거된 뒤 다시 페어링하면 Epoch가 오르므로(기본 설계 4절) 허용이 자동으로 풀리고 다시 허용해야 한다. 가십으로 받은 제거도 똑같이 적용된다.
- **네트워크 제한:** 터널 요청의 원격 주소(TCP 연결의 실제 주소)가 허용 네트워크에 있어야 한다. 기본값은 Tailscale 주소 범위 `100.64.0.0/10`과 `fd7a:115c:a1e0::/48`이고, `remote.networks`로 바꿀 수 있다(테스트는 루프백을 쓴다). 이 검사는 정책이며 주된 인증은 여전히 mTLS다. 컨트롤러도 허용 네트워크에 있는 멤버 주소로만 다이얼하므로 같은 LAN에서도 Tailscale 경로를 탄다. 주소가 호스트 이름이면 다이얼은 하되 대상의 검사에 맡긴다.
- **제거는 즉시 적용된다.** 터널은 기존 `CallerContext`를 따르므로 멤버를 제거하면 열려 있는 원격 세션이 바로 끊긴다.
- **접속 기록:** 대상은 `<설정 경로>/remote.log`(0600, JSON 한 줄씩)에 열림, 닫힘, 거부를 남긴다. 시각, 멤버 ID와 이름, 원격 주소, 거부 이유, 주고받은 바이트, 지속 시간을 적는다. 시작할 때 10 MiB를 넘으면 `remote.log.1`로 바꾼다. RFB 내용과 암호는 기록하지 않는다.
- **동시 세션은 호스트당 4개까지다.** 넘으면 거부하고 기록한다.
- **화면 앞 사용자에게 알림:** 사용자 세션이 있을 때는 접속 중 표시와 끊기 버튼을 띄우고, 다음 로그인 때 그동안의 접속을 알린다(3단계 이후).
- **OS 암호는 그대로 남는다.** 잠금 화면에서 로그인하려면 Windows 계정 암호나 Mac 암호가 필요하다. 이것이 두 번째 인증 단계다.

## 4. 터널 (1단계)

### 4.1 Peer API

| 메서드와 경로 | 동작 |
|---|---|
| `POST /v1/remote/tunnel` | HTTP/2 전이중 스트림. 요청 본문은 컨트롤러→대상, 응답 본문은 대상→컨트롤러 바이트다. |

- HTTP/2가 아니면 `invalid_request`로 거부한다. HTTP/1.1은 전이중을 보장하지 않는다.
- 검사 순서와 오류는 다음과 같다. 모두 기록에 남는다.
  1. 원격 제어가 꺼져 있으면 `peer_unauthorized`("remote control is off")
  2. 호출자가 허용 목록에 없거나 Epoch가 다르면 `peer_unauthorized`
  3. 원격 주소가 허용 네트워크 밖이면 `peer_unauthorized`
  4. 동시 세션이 한도를 넘으면 `conflict`
  5. 대상 주소에 5초 안에 연결하지 못하면 `peer_offline`("the screen service on this host is not running")
- 성공하면 상태 200과 헤더를 즉시 flush한 뒤 양방향으로 복사한다. 대상→컨트롤러 방향은 쓸 때마다 flush한다. 한쪽이 끝나면 다른 쪽도 닫고 닫힘을 기록한다. 호출자 제거, 데몬 종료도 세션을 끝낸다.
- 기본 대상 주소: macOS `127.0.0.1:5900`, Windows `127.0.0.1:7446`(원격 에이전트). 그 밖의 OS는 기본값이 없어 `remote.target`을 지정해야 켤 수 있다.

### 4.2 설정

```json
"remote": {
  "enabled": false,
  "allow": [{"id": "<멤버 ID>", "epoch": 2}],
  "target": "",
  "networks": []
}
```

`target`이 비면 4.1의 OS 기본값, `networks`가 비면 Tailscale 범위다. `target`은 루프백 `host:port`만 받는다.

### 4.3 Control API

| 메서드와 경로 | 동작 |
|---|---|
| `GET /v1/remote` | 이 호스트의 원격 제어 설정(허용 목록은 이름 포함) |
| `POST /v1/remote/settings` | `{Enabled?, Target?, Networks?}` 변경 |
| `POST /v1/remote/allow`, `/deny` | `{Peer}` 이름을 ID와 현재 Epoch로 바꿔 허용 목록에 넣고 뺀다 |
| `POST /v1/remote/connect` | `{Peer, Listen}`. 로컬 리스너(기본 `127.0.0.1:0`, 루프백만)를 열고 `{"Addr": ...}` 한 줄을 보낸 뒤, **요청이 열려 있는 동안만** 유지한다. 리스너가 받은 TCP 연결마다 터널 하나를 연다. 요청이 끝나면 리스너와 그 터널을 모두 닫는다. 프로세스가 죽어도 남는 포워드가 없다. |

### 4.4 CLI

```
sugar-it remote status
sugar-it remote enable | disable
sugar-it remote allow <peer> | deny <peer>
sugar-it remote target <host:port>       # 루프백만, "" 는 OS 기본값
sugar-it remote connect <peer> [--listen 127.0.0.1:5901]
```

`connect`는 주소를 출력하고 Ctrl+C까지 유지한다. 1단계에서는 아무 VNC 뷰어로 그 주소에 접속해 Mac 화면 공유를 검증할 수 있다.

## 5. macOS 대상 (2단계)

- 시스템 설정 → 일반 → 공유 → "화면 공유"를 켠다. 앱은 켜는 방법을 안내하고, 대상 주소 `127.0.0.1:5900`에 연결해 RFB 인사말이 오는지로 상태를 확인한다.
- sugar-it는 TCC 권한(화면 기록, 손쉬운 사용)이 필요 없다. 화면과 입력은 Apple의 서명된 구성 요소가 처리한다.
- 인증은 macOS 계정 암호(Apple 인증, RFB 보안 유형 30) 또는 "VNC 뷰어가 암호로 화면을 제어할 수 있음"의 VNC 암호(유형 2)다. noVNC가 지원하는 쪽을 스파이크 S3에서 확인한다. 암호는 컨트롤러에서 Windows 자격 증명 관리자에 저장할 수 있다(사용자 선택).
- 깨어 있음: 덮개를 열어 두거나 클램쉘 모드여야 한다. 잠자기 중이면 Tailscale이 끊겨 `peer_offline`으로 보이며, 앱은 "대상이 잠자기 중이거나 꺼져 있음"이라고 안내한다.
- 원격 세션 동안 디스플레이가 잠들지 않도록 대상 데몬이 `caffeinate -d -w <데몬 PID>`를 세션 수명만큼 실행한다(2단계, 순수 Go에서 외부 명령 실행).
- 사용자 LaunchAgent 데몬으로 충분하다. 재부팅 후 로그인 전은 비범위다(FileVault가 켜져 있으면 어차피 불가능하다).

## 6. Windows 대상 (3단계)

### 6.1 구성

| 구성 요소 | 권한 | 역할 |
|---|---|---|
| 사용자 데몬(기존) | 사용자 | 메시, Peer API, 터널. 대상 주소 `127.0.0.1:7446`으로 연결한다. |
| 원격 서비스 `sugar-it-remote` | LocalSystem 서비스 | 활성 콘솔 세션을 추적해 그 세션에 에이전트를 SYSTEM 토큰으로 띄우고, 세션이 바뀌거나 에이전트가 죽으면 다시 띄운다. `SendSAS`를 대신 호출한다. |
| 세션 에이전트 | SYSTEM, 콘솔 세션 | RFB 서버(`127.0.0.1:7446`). 입력 데스크톱(Default ↔ Winlogon)을 따라가며 캡처하고 입력한다. |

- 전부 순수 Go와 syscall로 만든다(`CGO_ENABLED=0`). 실행 파일은 `sugar-it-desktop.exe`를 재사용해 첫 인수로 구분한다(`remote-service`, `remote-agent`).
- 에이전트의 RFB 리스너는 루프백 전용이며, **연결한 프로세스가 사용자 데몬인지 확인**한다(`GetExtendedTcpTable`로 상대 포트의 PID를 찾아 `runtime.json`의 PID와 비교). 같은 PC의 다른 프로그램이 SYSTEM 화면에 붙지 못하게 하기 위해서다. 추가로 서비스가 만든 세션 비밀을 RFB VNC 인증(유형 2) 암호로 쓰고, 사용자 데몬은 그것을 ACL이 걸린 파일에서 읽는다. 세부는 3단계 시작 때 확정한다.

### 6.2 화면과 입력

- 캡처: DXGI Desktop Duplication(COM 호출을 syscall로). 데스크톱이 바뀌거나 `DXGI_ERROR_ACCESS_LOST`이면 `OpenInputDesktop` → `SetThreadDesktop` 후 다시 만든다. 실패하면 GDI `BitBlt`으로 대체한다.
- 입력: `SendInput`. SYSTEM 프로세스가 입력 데스크톱에 붙어 있으므로 잠금 화면, UAC 창, 관리자 권한 창에도 들어간다.
- Ctrl+Alt+Del: 뷰어의 전용 버튼 → 에이전트 → 서비스의 `SendSAS(FALSE)`. 설치 프로그램이 `HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System\SoftwareSASGeneration = 1`을 설정한다. Home에서 동작하는지 S1에서 확인한다.
- 인코딩: RFB Raw, CopyRect, Tight(JPEG, zlib)부터. 변경된 영역만 보낸다. 화질 개선은 4단계.
- 깨어 있음: 세션 동안 `SetThreadExecutionState(ES_CONTINUOUS | ES_SYSTEM_REQUIRED | ES_DISPLAY_REQUIRED)`. 대상 PC의 전원 설정은 "전원 연결 시 절전 안 함"이어야 한다고 안내한다. 잠금은 절전을 막지 않는다.
- 클립보드: RFB `ClientCutText`/`ServerCutText`(텍스트만). 잠금 화면에서는 전달하지 않는다.

### 6.3 설치와 업데이트

- NSIS 설치 파일이 서비스를 등록하고(`sc create`에 해당하는 SCM API 호출은 앱 자신이 `remote-service install`로 수행) 시작한다. 제거 때는 멈추고 지운다.
- 업데이트 도우미(기본 설계 11절)는 설치 전에 서비스를 멈추고 설치 뒤 다시 시작한다. 서비스 실행 파일도 같은 exe이기 때문이다.
- 기본 설계의 "모두 사용자 권한으로 실행" 원칙의 유일한 예외다. 서비스는 화면과 입력만 다루고 네트워크를 열지 않는다.

## 7. 뷰어 (2단계)

- 데스크톱 앱에 noVNC(MPL-2.0, 정확한 버전 고정, 파일로 포함)를 넣는다. WebView2는 TCP를 열 수 없으므로 데스크톱 앱의 Go 쪽이 **루프백 WebSocket 브리지**를 연다: 임의 포트, 세션마다 새 토큰, `Origin` 검사. 브리지는 Control API `connect`를 앱 프로세스 안에서 열어 둔다.
- 키보드: 뷰어 창이 포커스를 가진 동안 `SetWindowsHookEx(WH_KEYBOARD_LL)`로 Win 키, Alt+Tab, Alt+F4 등을 가로채 원격에 보낸다. Ctrl+Alt+Del은 로컬에서 가로챌 수 없으므로 버튼으로 보낸다.
- 키 전달은 물리 키(스캔 코드) 기준 RFB keysym으로 한다. 한글 IME 조합은 원격의 IME가 하도록 로컬 IME는 뷰어에서 끈다. 한/영 키는 `Hangul` keysym(0xff31)으로, 한자 키는 `Hangul_Hanja`(0xff34)로 보낸다. Mac 대상에서는 Windows 키(또는 설정에 따라 Ctrl)를 Cmd로 매핑한다.
- 연결 상태에 Tailscale 경로가 직접 연결인지 DERP 중계인지 표시한다(`tailscale status --json`이 있으면 사용). 중계는 대역폭이 낮아 화면이 끊길 수 있다.

## 8. 한계 (확정)

| 한계 | 내용 |
|---|---|
| 재부팅 후 로그인 전 | 1차 비범위. 사용자 데몬이 없다. FileVault와 TPM+PIN BitLocker는 사전 부팅 화면에서 원격으로 아무것도 할 수 없다. |
| 맥북 잠자기 | 덮개를 닫으면(클램쉘 제외) 연결할 수 없다. |
| Windows 절전 | 전원 설정이 절전으로 가면 연결이 끊긴다. 사용자가 설정해야 한다. |
| 서명 없음 | 서명 없는 SYSTEM 서비스의 입력 주입은 Defender에 RAT/PUA로 탐지될 수 있다. S1에서 확인한다. |
| Tailscale 중계 | DERP로 넘어가면 화질과 지연이 크게 나빠진다. |
| Mac 화질 | Apple RFB 서버의 성능에 묶인다. |
| 단축키 | Ctrl+Alt+Del은 버튼으로만 보낸다. |

## 9. 스파이크와 진행 판단 (0단계)

| ID | 확인할 것 | 어디서 | 실패하면 |
|---|---|---|---|
| S1 | SYSTEM 서비스가 콘솔 세션에 띄운 에이전트가 Win+L 잠금 화면을 캡처하고, 암호 입력과 `SendSAS`가 되는가. 서명 없는 빌드를 Defender가 막는가. | 실제 Windows Home | 3단계를 다시 설계한다. 탐지되면 배포 방식(예외 등록 안내, 서명)을 정한다. |
| S2 | 순수 Go RFB 서버 + noVNC로 1080p에서 Tailscale 경유 FPS, 지연, CPU | Windows | 인코딩 방식을 바꾼다. |
| S3 | macOS 화면 공유를 1단계 터널과 noVNC로 접속, 인증 유형, 잠금 화면, 클램쉘 | Mac + Windows | 인증을 직접 구현하거나 외부 뷰어 안내로 물러선다. |
| S4 | 저수준 키보드 훅과 한글 입력, Mac Cmd 매핑 | Windows | 매핑 표를 고친다. |

1단계 터널은 S3의 전제이므로 스파이크와 함께 먼저 만든다. S1은 3단계 착수 전에 끝낸다.

## 10. 단계

| 단계 | 내용 | 완료 기준 |
|---|---|---|
| 1. 기반 | 4절 전체: 설정, 허용 목록, 네트워크 제한, Peer 터널, 접속 기록, Control `connect`, CLI | Linux CI에서 두 데몬 사이 터널 왕복, 거부 경로, 제거 시 끊김 테스트 통과. 실제 Tailscale에서 VNC 뷰어로 Mac 접속(S3 일부). |
| 2. 뷰어와 Mac | 7절 뷰어, WebSocket 브리지, 5절 Mac 안내와 깨어 있음 | 앱 안에서 Mac 잠금 화면 해제와 조작 |
| 3. Windows 에이전트 | 6절 전체 | 앱 안에서 Windows 잠금 화면 해제와 조작 |
| 4. 품질 | 다중 모니터, 화질 적응, 접속 알림 UI, 재부팅 후 로그인 화면 검토 | — |

## 11. 프로토콜 호환성

- 터널은 새 경로만 추가하므로 `ProtocolMajor`는 그대로 1이다. 옛 피어는 `not_found`를 돌려주고, 컨트롤러는 "대상의 sugar-it가 원격 제어를 지원하지 않는 버전"으로 안내한다.

## 12. 테스트 방침

- 네트워크 제한은 설정으로 바꿀 수 있어, 테스트는 `127.0.0.0/8`을 허용하고 기본값(Tailscale)에서 루프백이 거부되는 것도 확인한다.
- 대상 RFB 서버 대신 에코 TCP 서버를 쓴다.
- Windows와 macOS 고유 부분은 해당 OS에서 사람이 확인하고 `docs/manual-test.md`에 절차와 결과를 적는다.

## 13. 미결정 사항

- 에이전트 RFB 연결의 상대 확인 방식(6.1)의 최종 형태
- noVNC가 Apple 인증(유형 30)을 지원하는지(S3)
- 재부팅 후 로그인 화면(서비스 고유 신원)을 지원할지(4단계)

## 14. 결정 기록

- D1 RFB로 통일: macOS 화면 공유를 재사용하고 Windows 에이전트도 RFB로 만든다. 대안(자체 프로토콜 + H.264)은 뷰어가 둘이 되고 Mac에 CGO와 TCC가 필요하다.
- D2 터널은 HTTP/2 스트림: 기존 mTLS, 멤버 확인, 제거 시 끊김을 그대로 쓴다. QUIC/WebRTC는 4단계에서 필요할 때 검토한다.
- D3 대상 주소는 대상이 정하고 루프백만 허용: 터널이 범용 프록시가 되지 않게 한다. 그래서 Linux ssh 포워딩은 제공하지 않는다.
- D4 허용 목록에 Epoch 포함: 제거 후 재페어링이 허용을 되살리지 않게 한다.
- D5 `connect`는 요청이 열려 있는 동안만 유지: 남는 포워드가 없다.
- D6 Windows SYSTEM 서비스는 화면과 입력만: 네트워크와 신원은 사용자 데몬에 남긴다. 재부팅 후 로그인 전은 비범위.
