# jindo-mcp를 Codex CLI에 등록하기

`~/.codex/config.toml`에 jindo MCP 서버를 등록하는 방법. Claude Code의 `.mcp.json`
등록(`docs/jindo-mcp.md` 참고)과 별개로, Codex CLI는 전역 TOML 설정 파일 하나에
모든 MCP 서버를 정의한다.

## 1. 바이너리 빌드

```bash
cd <repo-root>
go build -o /Users/anamkwon/jindo-mcp-test/jindo-mcp ./cmd/jindo-mcp
```

## 2. 등록 (권장: `codex mcp add`)

```bash
codex mcp add jindo \
  --env CLAUDE_PROJECT_DIR=/Users/anamkwon/jindo-mcp-test \
  -- /Users/anamkwon/jindo-mcp-test/jindo-mcp
```

이 명령은 `[mcp_servers.jindo]` 블록만 추가하고 기존 설정(다른 MCP 서버, projects,
plugins 등)은 건드리지 않는다. `codex mcp add`는 `startup_timeout_sec` /
`tool_timeout_sec`를 설정하지 않으므로, 기존 `code-assistant-peers` 블록과 동일한
값을 jindo 블록에 직접 추가해야 한다 (아래 3번).

등록/수정 전에 반드시 백업:

```bash
cp ~/.codex/config.toml ~/.codex/config.toml.bak-jindo
```

## 3. 최종 TOML 스니펫

`codex mcp add` 실행 후 `~/.codex/config.toml`에 아래 블록이 생기며,
`startup_timeout_sec`/`tool_timeout_sec` 두 줄만 수동으로 추가한 상태:

```toml
[mcp_servers.jindo]
command = "/Users/anamkwon/jindo-mcp-test/jindo-mcp"
startup_timeout_sec = 30.0
tool_timeout_sec = 1800.0

[mcp_servers.jindo.env]
CLAUDE_PROJECT_DIR = "/Users/anamkwon/jindo-mcp-test"
```

기존에 `[mcp_servers.jindo]` 블록이 이미 있다면, 그 블록만 교체하고 나머지
파일 내용(다른 `[mcp_servers.*]`, `[projects.*]`, `[plugins.*]` 등)은 그대로 둔다.

## 왜 `CLAUDE_PROJECT_DIR`이 필수인가

Codex는 MCP stdio 서버를 임의의 작업 디렉터리(cwd)에서 실행시킬 수 있다.
`cmd/jindo-mcp/main.go`의 `run()`은 시작 시 `CLAUDE_PROJECT_DIR`이 설정돼 있으면
그 경로로 `os.Chdir`하여 프로세스를 프로젝트 루트에 재고정한다:

```go
if dir := os.Getenv("CLAUDE_PROJECT_DIR"); dir != "" {
    if err := os.Chdir(dir); err != nil {
        return fmt.Errorf("chdir %s: %w", dir, err)
    }
}
```

jindo는 `.jindo` 공유 메모리 저장소, 에이전트 서브프로세스, tmux 창 등 모든 것을
"현재 디렉터리 기준 상대 경로"로 다룬다. cwd가 어긋나면 `.jindo`가 엉뚱한 위치에
생기거나 아예 접근하지 못한다. 따라서 이 env 변수 없이 등록하면 Codex가 jindo를
실행할 때마다 cwd가 무엇이든 상관없이 실패하거나 상태가 어긋난다 — 필수값이다.

## 타임아웃 값을 왜 이렇게 잡았나

- `startup_timeout_sec = 30.0`: 서버 프로세스 기동 자체는 정적 바이너리 실행이라
  30초면 충분하다. `code-assistant-peers`와 동일한 값을 그대로 사용.
- `tool_timeout_sec = 1800.0`: 이건 "툴 콜 하나"에 대한 고정 타임아웃이다.
  jindo의 동기 `dispatch` 툴은 하위 에이전트 작업이 끝날 때까지 블로킹하는데,
  실제 코딩 작업은 30분을 넘기는 경우가 드물지 않다. 반면 이 타임아웃은
  MCP 프로토콜 레벨의 고정값이라 작업별로 조정할 수 없으므로, 긴 디스패치도
  안전하게 끝날 수 있도록 넉넉하게 잡아야 한다. 그럼에도 "타임아웃보다 더 오래
  걸리는 작업"은 여전히 가능하므로, 오래 걸릴 것으로 예상되는 작업은 아래처럼
  비동기 디스패치를 쓰는 게 근본적인 해법이다.

## Codex에서 비동기 디스패치 사용하기

동기 `dispatch`는 `tool_timeout_sec`(1800초)에 걸리면 강제 종료된다. 이를 피하려면
`dispatch_async` → `job_status` 폴링 패턴을 쓴다 (`internal/mcp/mcp.go`,
`internal/mcp/mcp_test.go` 참고):

1. `dispatch_async`를 호출하면 즉시 `job_id`와 `status: "running"`을 반환하고,
   실제 작업은 백그라운드에서 진행된다.
2. `job_status`를 그 `job_id`로 반복 호출해 상태가 `"done"` 또는 `"error"`가 될
   때까지 폴링한다. `"running"` 상태를 결과로 착각해 진행하면 안 된다 — 이는
   `dispatch_async` 툴 설명에 명시된 폴링 계약이다.
3. `job_status`는 `wait_sec` 인자로 롱폴링을 지원한다 (예: `wait_sec: 5`) —
   호출할 때마다 그 초만큼 상태 변화를 기다렸다가 응답하므로, 짧은 간격으로
   무한정 재요청하는 대신 서버 쪽에서 대기시켜 호출 횟수를 줄일 수 있다.

즉, 1800초짜리 고정 툴 타임아웃 안에서 끝나는 것은 `dispatch_async` 호출과 각
`job_status` 폴링 콜 자체이지, 기저 작업의 총 소요 시간이 아니다. 오래 걸리는
디스패치는 항상 비동기 경로를 쓰는 게 안전하다.

## 필수: `--dangerously-bypass-approvals-and-sandbox`로 실행

실측 검증(`docs/codex-live-verification.md`)에서 확인된 운영상 필수 조건이다.
Codex의 기본 exec 샌드박스(`sandbox: workspace-write`, `approval: never`)에서
`jindo/dispatch`(또는 `dispatch_async`)를 호출하면 `user cancelled MCP tool call`
로 실패하고, 서버 쪽에도 아무것도 남지 않는다(`dispatch.log` 미증가, job 미생성).

원인은 jindo가 등록/설정 문제가 아니라 jindo의 동작 방식 자체에 있다: dispatch는
`claude`/`gemini` 같은 하위 에이전트 프로세스를 실제로 스폰해 파일을 쓰고
작업하는데, 이 서브프로세스들이 Codex의 workspace-write 샌드박스 범위 밖에서
동작하므로 제한된 샌드박스 안에서는 호출 자체가 살아남지 못한다.

해결: `codex exec`를 다음 플래그로 실행한다 (샌드박스가 `danger-full-access`로 바뀐다).

```bash
codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check "..."
```

`--skip-git-repo-check`는 작업 디렉터리가 git 저장소가 아닐 때(예:
`/Users/anamkwon/jindo-mcp-test`) 필요하다. jindo TOML 설정(바이너리 경로, env,
`tool_timeout_sec`)은 그대로 맞아도 이 샌드박스 조건 없이는 dispatch가 항상
취소된다 — jindo를 통해 다른 에이전트를 디스패치하는 모든 codex exec 호출에
적용해야 하는 조건이다.
