# sugar-it: 호스트 간 공유 폴더 메시 설계

`sugar-it`는 이 프로젝트의 이름이다. 등록된 모든 호스트(피어)의 공유 폴더를 NAS처럼 탐색하고 자료를 옮기는 앱이며, CLI·데스크톱 GUI·WebDAV 마운트를 제공한다.

## 1. 확정된 요구사항

| 항목 | 결정 |
|---|---|
| 네트워크 | 호스트끼리 IP:포트로 직접 도달 가능 (LAN, Tailscale/WireGuard, 포트포워딩) |
| 사용 방식 | 앱에서 탐색·복사·이동·삭제 + WebDAV 드라이브 마운트 |
| 플랫폼 | Linux, macOS, Windows (amd64, arm64) |
| GUI | 네이티브 데스크톱 앱 (Wails v2) |
| 인터페이스 | GUI와 CLI가 같은 기능을 제공하는 하나의 패키지 |

**비범위:** NAT 홀펀칭·릴레이, 폴더 자동 동기화, 다중 사용자·권한 계정, mDNS 자동 발견, 공유 폴더 밖 임의 경로 전송, 코드 서명.

## 2. 아키텍처

- **`sugar-it` (CLI + 데몬):** `CGO_ENABLED=0` 단일 바이너리. 헤드리스 서버에서도 동작한다.
- **`sugar-it-desktop`:** Wails 앱. 로컬 데몬의 제어 API를 호출하는 얇은 클라이언트이며, 데몬이 없으면 프로세스 안에서 띄운다. 이때 창을 닫으면 공유가 멈춘다는 것을 UI에 표시한다.
- **데몬의 리스너 3개**

| 리스너 | 기본 주소 | 인증 | 용도 |
|---|---|---|---|
| Peer API | `:7443` | mTLS (TLS 1.3 이상) | 피어 간 통신 |
| Control API | `127.0.0.1:7444` | Bearer 토큰 (`control.token`) | CLI, GUI |
| WebDAV | `127.0.0.1:7445` | Basic (`sugar-it` / 제어 토큰) | OS 드라이브 마운트 |

설정 경로는 `os.UserConfigDir()/sugar-it`이고, `--config-dir`로 바꿀 수 있다.

## 3. 신원과 신뢰

- 호스트 신원은 자체 서명 Ed25519 인증서다. **ID**는 SPKI의 SHA-256을 소문자 base32(패딩 없음)로 인코딩한 값이다.
- 신뢰 모델은 단일 소유자 클러스터다. 멤버는 서로 완전히 신뢰하며, 어떤 멤버든 새 호스트를 초대하거나 제거할 수 있다.
- **페어링 절차**
  1. A가 `sugar-it invite`로 초대 문자열(`sugar-it1:` + base64url JSON: id, name, addrs, token, expires)을 만든다. 토큰은 128비트 난수, 1회용, TTL 10분이며 메모리에만 저장한다.
  2. B가 `sugar-it peer add <초대>`를 실행한다. A의 인증서를 초대의 ID에 고정해 TLS로 접속한 뒤 `POST /v1/pair`를 보낸다.
  3. A는 토큰을 소비하고 B의 인증서에서 얻은 ID로 멤버를 등록한다. 응답으로 멤버 목록을 돌려주고, 이후 가십으로 전파된다.
- 멤버가 아닌 인증서는 `/v1/pair` 외 모든 경로에서 403을 받는다.

## 4. 멤버십 (풀 메시)

- 한 호스트만 등록해도 모든 멤버와 연결되도록, 멤버 목록을 60초마다와 변경 즉시 가십한다.
- `Member{ID, Name, Addrs, Epoch, Generation, Removed}` 병합 규칙
  - `Epoch`가 큰 쪽이 이긴다. 다음으로 `Generation`이 큰 쪽이 이긴다. 시계에 의존하지 않는다.
  - 둘 다 같으면 `Removed`가 이긴다.
  - 둘 다 활성이면 주소를 합집합한다.
  - 자기 자신의 항목에 대한 원격 변경은 무시한다.
- `Epoch`는 제거와 재페어링만 올린다. 제거는 tombstone(`Epoch+1`, `Generation+1`, `Removed=true`)이다. 재등록은 새 페어링으로만 가능하며, 초대한 쪽이 기존 항목이 `Removed`일 때만 `Epoch = 기존 값 + 1`로 올리고 `Generation = 기존 값 + 1`, `Removed=false`로 기록한다. 이미 활성인 멤버를 다시 페어링하면 `Epoch`는 그대로 두므로, 활성 항목의 `Epoch`는 항상 짝수이고 tombstone의 `Epoch`는 홀수이다. 따라서 tombstone이 활성 항목과 같은 `Epoch`로 겹치지 않는다. 소유자의 이름·주소 변경은 `Generation`만 올리므로 어떤 `Generation`도 tombstone을 넘을 수 없다.
- 제거한 쪽이 아직 모르는 더 새로운 재페어링(제거 이후 새 초대로 다시 등록된 항목)은 그 제거를 이긴다. 이 경우 운영자는 뷰가 수렴한 뒤 제거를 한 번 더 실행한다.
- 제거된 호스트가 아직 자신이 제거된 줄 모르는 동안에는 초대를 만들 수 있고, 멤버의 운영자가 그 초대로 `peer add`를 실행하면 다시 받아들여진다. 이것은 단일 소유자 신뢰 모델에서 의도된 동작이다.
- 호스트는 자기 항목의 더 높은 `Epoch`를 활성 항목(페어링 응답이나 가십)에서만 받아들이고, tombstone에서는 받아들이지 않는다. 자기 `Generation`을 올릴 때는 같은 `Epoch`의 활성 항목에서 본 가장 높은 값보다 크게 올린다.
- 이름은 그 호스트에서만 바꿀 수 있다. 주소나 이름이 바뀌면 소유자가 자기 `Generation`을 올린다. 호스트 주소가 바뀌어도 신원은 유지된다.
- 이름 규칙은 `[a-z0-9-]{1,32}`이고 `local`은 예약어다. 이름이 겹치면 이 호스트가 자기 이름을 먼저 갖고 나머지는 ID 순으로 정렬해 `-2`, `-3`을 붙인 로컬 별칭을 쓴다.
- 온라인 상태는 15초마다 `GET /v1/info`로 확인한다. 연결 타임아웃은 5초이고, 저장된 주소를 차례로 시도하며 마지막 성공 주소를 먼저 쓴다.
- 주소 자동 감지는 루프백, 링크 로컬, `docker*`, `br-*`, `veth*`, `virbr*`를 제외한다. `advertise_addrs`로 직접 지정할 수 있다.
- 메이저 프로토콜 버전이 다른 피어는 `peer_incompatible`로 표시한다.

## 5. 공유 폴더

- 각 호스트는 자기 공유만 관리한다. `Share{Name, Path, ReadOnly}`이며 원격에서 공유를 추가하거나 변경할 수 없다.
- 파일 접근은 모두 `os.Root`로 격리한다. 공유 밖으로 나가는 심볼릭 링크와 `..`는 거부한다.
- 심볼릭 링크는 목록에 `symlink`로 표시하지만 따라가거나 전송하지 않는다. 전송 시 `skipped_symlink`로 기록한다.
- 같은 호스트의 공유끼리 경로가 겹치면(심볼릭 링크 해석 후, 부모·자식 포함) 등록을 거부한다. 겹치는 공유 사이의 이동은 원본 삭제로 데이터가 사라질 수 있기 때문이다. Windows·macOS에서는 대소문자를 구분하지 않고 비교한다.
- 읽기 전용 공유에 대한 쓰기, 삭제, 이름 변경, 이동의 원본 삭제는 `read_only`로 거부한다. 이동은 시작 전에 거부한다.
- 전송 임시 파일은 공유 루트의 `.sugar-it-part/`에 둔다. 이 경로는 목록에서 숨기고 API 경로로 접근할 수 없다. 활성 작업이 없고 7일 지난 임시 파일은 데몬 시작 시 정리한다.

## 6. Peer API (HTTP/2 + mTLS)

| 메서드와 경로 | 동작 |
|---|---|
| `GET /v1/info` | id, name, 프로토콜 버전 |
| `POST /v1/pair` | 페어링 (멤버 인증 예외) |
| `POST /v1/members/sync` | 멤버 목록 교환 |
| `GET /v1/shares` | 공유 목록 (로컬 경로는 노출하지 않음) |
| `GET /v1/shares/{s}/entries?path=` | 디렉터리 목록 |
| `GET /v1/shares/{s}/stat?path=` | 메타데이터 |
| `GET /v1/shares/{s}/file?path=` | 다운로드 (Range, If-Range에 ETag 사용) |
| `PUT /v1/shares/{s}/file?path=` | 업로드 (임시 파일 후 rename) |
| `GET /v1/shares/{s}/hash?path=` | SHA-256. 헤더를 먼저 flush해 긴 해시에도 타임아웃되지 않음 |
| `POST /v1/shares/{s}/mkdir`, `/remove`, `/rename` | 파일 조작 |
| `POST /v1/jobs`, `GET /v1/jobs?origin=`, `POST /v1/jobs/{id}/cancel`, `/retry` | 전송 작업 |

오류 응답은 `{"code","message"}`이다. ETag는 `size(hex)-mtimeNanos(hex)`이다.

**오류 코드:** `not_found`, `exists`, `not_empty`, `read_only`, `invalid_path`, `invalid_name`, `invalid_request`, `share_not_found`, `share_overlap`, `peer_not_found`, `peer_offline`, `peer_unauthorized`, `peer_incompatible`, `changed`, `disk_full`, `hash_mismatch`, `conflict`, `cross_host_rename`, `canceled`, `internal`.

## 7. 전송

- **작업은 항상 목적지 호스트에서 실행한다.** A에서 B→C 이동을 요청하면 C가 B에서 직접 가져오므로 A의 대역폭을 쓰지 않는다. C가 B에 닿지 않으면 A를 경유하지 않고 작업을 `waiting_peer`로 표시한다.
- `Spec{Op: copy|move, Src, Dst: {Peer, Share, Path}, OnConflict: fail|skip|overwrite|rename}`. 기본 충돌 정책은 `fail`이며, 항목별 실패로 처리하고 나머지는 계속한다.
- **목적지 해석은 `cp`와 같다.** 기존 디렉터리나 `/`로 끝나는 경로면 그 아래에 원본 이름으로 넣고, 아니면 그 이름으로 만든다. 디렉터리는 자동으로 재귀 처리한다.
- **파일 하나의 처리 순서**
  1. 원본 stat
  2. 충돌 판정
  3. 임시 파일의 etag가 같으면 이어받기, 다르면 새로 시작
  4. `Open(offset, etag)`로 스트리밍
  5. fsync
  6. 양쪽 SHA-256 비교 및 원본 etag 재확인
  7. mtime과 Unix 권한 비트 보존
  8. 원자적 rename
  9. 이동이면 원본 삭제
- **이동의 안전 규칙**
  - 원본 파일은 해당 파일의 검증과 커밋이 끝난 뒤에만 삭제한다.
  - 실패·건너뜀 항목의 원본은 그대로 둔다.
  - 빈 디렉터리는 마지막에 비재귀로 제거한다.
  - 같은 호스트·같은 공유 내 이동은 원자적 rename이다.
  - 자기 자신이나 자기 하위로의 복사·이동은 `invalid_request`로 거부한다.
- **상태:** `queued → running ↔ waiting_peer → done | partial | failed | canceled`
- **실패 처리**
  - `peer_offline`: 임시 파일을 유지하고 지수 백오프(1초~60초)로 무기한 재시도한다.
  - `disk_full`: 작업을 즉시 `failed`로 끝낸다. `retry`로 재개할 수 있다.
  - 원본 변경 시: etag 불일치면 임시 파일을 버리고 한 번 재시작하며, 다시 바뀌면 항목 실패 `changed`로 기록한다.
  - 그 외 오류: 항목 실패로 기록하고 계속한다.
- **영속성:** `jobs/<id>/job.json`을 진행 중 1초 간격으로 원자적 저장하고, 완료 항목은 `jobs/<id>/done.log`에 추가한다. 실패 항목은 1,000개까지 저장하고 개수는 별도로 센다. 데몬이 재시작되면 `queued`, `running`, `waiting_peer` 작업을 재개한다.
- 동시 작업은 기본 2개이고, 작업 내 파일은 순차 처리한다.
- **이름 호환성:** 목적지 OS에서 쓸 수 없는 이름(Windows의 `<>:"|?*`, 제어 문자, 끝의 점·공백, `CON`, `NUL` 등 예약어)은 `invalid_name` 항목 실패로 기록한다. 파일명 바이트는 변환하지 않는다(NFC/NFD 포함).

## 8. WebDAV

- `golang.org/x/net/webdav`로 `/{peer}/{share}/{path}` 트리를 제공한다. `/`와 `/{peer}`는 읽기 전용 가상 디렉터리다.
- 읽기는 Seek 때 Range로 다시 열고, 쓰기는 로컬 임시 파일에 받은 뒤 Close에서 `Put`한다. 목록 결과는 2초 캐시해 PROPFIND의 Stat 폭주를 막는다.
- 호스트나 공유를 넘는 MOVE는 `cross_host_rename` 오류(HTTP 502)로 거부한다. 이런 이동은 앱에서 수행한다.
- `sugar-it mount`는 OS별 마운트 명령을 실행하며, **토큰을 명령행 인자에 넣지 않는다.**
- **Windows 제약:** WebClient의 기본 설정은 HTTP Basic을 막고(`BasicAuthLevel`) 파일 크기를 약 50MB로 제한한다(`FileSizeLimitInBytes`). `sugar-it mount`가 이 값을 읽어 부족하면 관리자 권한으로 실행할 명령을 안내한다. 이 레지스트리 값의 상한으로 약 4GB가 넘는 파일은 Windows 마운트에서 다룰 수 없으므로 앱을 쓴다.

## 9. CLI

```
sugar-it daemon | status
sugar-it invite [--ttl 10m]
sugar-it peer add <invite> | list | remove <name> | rename <new-name>   # rename은 이 호스트의 이름을 바꾼다
sugar-it share add <name> <path> [--read-only] | list | remove <name>
sugar-it ls <peer>[:<share>[/path]]
sugar-it cp|mv <src> <dst> [--on-conflict fail|skip|overwrite|rename] [--detach]
sugar-it rm [-r] <target>  |  sugar-it mkdir <target>  |  sugar-it rename <target> <new-name>
sugar-it jobs list | show <id> | cancel <id> | retry <id> | watch [<id>]
sugar-it mount [--drive Z:] | unmount
sugar-it service install | uninstall
```

대상 표기는 `peer:share/path`이고 로컬은 `local:share/path`이다. 모든 조회 명령은 `--json`을 지원한다. 종료 코드는 성공 0, `partial` 또는 `failed` 작업 1, 사용 오류 2다.

## 10. GUI

- **파일:** 양쪽 패널 각각에서 피어와 공유를 선택한다. 복사, 이동, 삭제, 이름 변경, 새 폴더, 패널 간 드래그, 충돌 정책 선택을 제공한다. 긴 목록은 가상 스크롤로 표시한다.
- **전송:** 진행률, 속도, 상태, 취소, 재시도, 실패 항목을 보여주며 1초마다 갱신한다.
- **호스트·공유:** 온라인 상태, 초대 생성·참여, 호스트 제거, 네이티브 폴더 선택으로 공유 추가·제거, 드라이브 마운트, 서비스 설치를 제공한다.
- GUI 문구는 한국어, CLI 문구는 영어다.

## 11. 패키징

- `sugar-it`: GoReleaser로 3개 OS × 2개 아키텍처 아카이브와 `.deb`, `.rpm`을 만든다.
- `sugar-it-desktop`: OS별 러너에서 `wails build`로 macOS `.app`, Windows NSIS 설치 파일, Linux 바이너리를 만든다.
- `sugar-it service install`
  - Linux: `systemd --user` 유닛과 `loginctl enable-linger`
  - macOS: LaunchAgent
  - Windows: 로그온 시 실행되는 예약 작업
  - 사용자의 파일과 설정에 접근하도록 모두 사용자 권한으로 실행한다.
