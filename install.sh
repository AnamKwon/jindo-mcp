#!/usr/bin/env bash
# jindo-mcp installer — build the binary and register it with whichever MCP
# hosts (Claude Code / Codex CLI / agy) are present. Idempotent.
# Usage:  ./install.sh          register for real
#         DRY_RUN=1 ./install.sh   build + detect + smoke only (no config changes)
set -euo pipefail

DRY_RUN="${DRY_RUN:-0}"
ROOT="$(cd "$(dirname "$0")" && pwd)"
BIN="$ROOT/jindo-mcp"

run() { if [ "$DRY_RUN" = 1 ]; then echo "    [dry-run] $*"; else eval "$*"; fi; }

# Canonical jindo usage guidance and the fenced markers used to manage it in a
# host instruction file. Registering the server only makes the TOOLS visible;
# this block is the behavioral policy that makes a host actually PREFER
# delegating to jindo (and route/effort well). It is opt-in (see below).
GUIDANCE_FILE="$ROOT/docs/host-guidance.md"
GUIDANCE_BEGIN="<!-- jindo:begin — managed by jindo install.sh; edits inside are overwritten on reinstall -->"
GUIDANCE_END="<!-- jindo:end -->"

# inject_guidance TARGET — idempotently write the jindo usage block into a host
# instruction file. Any prior jindo block (between the markers) is stripped and a
# fresh one appended, so reinstalling updates it in place instead of duplicating.
# The file is backed up first; DRY_RUN only reports.
inject_guidance() {
  local target="$1"
  if [ "$DRY_RUN" = 1 ]; then
    echo "    [dry-run] would inject jindo guidance block into $target"
    return
  fi
  mkdir -p "$(dirname "$target")"
  [ -f "$target" ] && cp "$target" "$target.bak-jindo-$(date +%Y%m%d-%H%M%S)"
  {
    if [ -f "$target" ]; then
      awk -v b="$GUIDANCE_BEGIN" -v e="$GUIDANCE_END" '
        index($0,b){inblk=1; next}
        inblk && index($0,e){inblk=0; next}
        !inblk{print}
      ' "$target"
    fi
    printf '\n%s\n' "$GUIDANCE_BEGIN"
    cat "$GUIDANCE_FILE"
    printf '%s\n' "$GUIDANCE_END"
  } > "$target.jindo-tmp"
  mv "$target.jindo-tmp" "$target"
  echo "    injected: $target"
}

# Codex request-timeout fix. `codex mcp add` does NOT set per-server timeouts, so
# Codex's default MCP tool timeout is short — but a jindo `dispatch` spawns a
# sub-agent that can run for minutes, tripping the timeout and failing the call.
# ensure_codex_timeouts adds startup_timeout_sec/tool_timeout_sec to the
# [mcp_servers.jindo] table if missing (idempotent; leaves any other keys, incl.
# an env sub-table, untouched). Values are overridable via env.
JINDO_CODEX_STARTUP_TIMEOUT="${JINDO_CODEX_STARTUP_TIMEOUT:-30}"
JINDO_CODEX_TOOL_TIMEOUT="${JINDO_CODEX_TOOL_TIMEOUT:-1800}"
ensure_codex_timeouts() {
  local cfg="$HOME/.codex/config.toml"
  [ -f "$cfg" ] || { echo "    (no ~/.codex/config.toml yet; skipping timeout setup)"; return 0; }
  if [ "$DRY_RUN" = 1 ]; then
    echo "    [dry-run] would ensure startup_timeout_sec=${JINDO_CODEX_STARTUP_TIMEOUT}/tool_timeout_sec=${JINDO_CODEX_TOOL_TIMEOUT} in [mcp_servers.jindo]"
    return
  fi
  cp "$cfg" "$cfg.bak-jindo-$(date +%Y%m%d-%H%M%S)"
  awk -v st="$JINDO_CODEX_STARTUP_TIMEOUT" -v tt="$JINDO_CODEX_TOOL_TIMEOUT" '
    function flush() {
      if (injindo) {
        if (!have_start) print "startup_timeout_sec = " st ".0"
        if (!have_tool)  print "tool_timeout_sec = " tt ".0"
      }
      injindo=0; have_start=0; have_tool=0
    }
    /^\[/ {
      flush()
      if ($0=="[mcp_servers.jindo]") injindo=1
      print; next
    }
    injindo && /^[[:space:]]*startup_timeout_sec[[:space:]]*=/ {have_start=1}
    injindo && /^[[:space:]]*tool_timeout_sec[[:space:]]*=/  {have_tool=1}
    {print}
    END{ flush() }
  ' "$cfg" > "$cfg.jindo-tmp" && mv "$cfg.jindo-tmp" "$cfg"
  echo "    ensured [mcp_servers.jindo] startup_timeout_sec=${JINDO_CODEX_STARTUP_TIMEOUT}s, tool_timeout_sec=${JINDO_CODEX_TOOL_TIMEOUT}s"
}

echo "==> Building jindo-mcp"
( cd "$ROOT" && go build -o "$BIN" ./cmd/jindo-mcp )
echo "    built: $BIN"

# The agent CLIs jindo dispatches TO (author/reviewer). jindo can only route to
# the ones installed here — a missing CLI makes any dispatch to it fail.
echo "==> Sub-agent CLIs on PATH (jindo routes tasks to these):"
for a in claude codex agy; do
  if command -v "$a" >/dev/null 2>&1; then
    echo "    [x] $a"
  else
    echo "    [ ] $a   (not found — jindo cannot route to \"$a\" until installed)"
  fi
done

# Register the jindo MCP server with each host that is present.
echo "==> Registering jindo with detected MCP hosts:"
registered=0
if command -v claude >/dev/null 2>&1; then
  echo "  - Claude Code"
  # Touch only Claude's private local registration. A project may intentionally
  # carry a shared .mcp.json entry; an unscoped remove can erase that file.
  run "claude mcp remove jindo -s local >/dev/null 2>&1 || true"
  run "claude mcp add jindo -s local -- \"$BIN\""
  registered=1
fi
if command -v codex >/dev/null 2>&1; then
  echo "  - Codex CLI"
  run "codex mcp remove jindo >/dev/null 2>&1 || true"
  run "codex mcp add jindo -- \"$BIN\""
  # Fix the request timeout: dispatch runs sub-agents for minutes, longer than
  # Codex's default MCP tool timeout, so set generous per-server timeouts.
  ensure_codex_timeouts
  echo "    NOTE: run codex with a danger-full-access sandbox (or a profile whose"
  echo "          sandbox_mode=\"danger-full-access\"); dispatch spawns sub-agents. See INSTALL.md."
  registered=1
fi
if command -v agy >/dev/null 2>&1; then
  echo "  - agy (Antigravity/Gemini): register manually under mcpServers in your"
  echo "    ~/.gemini MCP config (see INSTALL.md):"
  echo "      { \"mcpServers\": { \"jindo\": { \"command\": \"$BIN\", \"args\": [] } } }"
fi
[ "$registered" = 0 ] && echo "    (no auto-registering host CLI found; see INSTALL.md for manual setup)"

# Optionally inject the jindo usage guidance into host instruction files so a
# host doesn't merely SEE the tools but is told to prefer delegating to jindo
# (and to pin model/effort well). Opt-in: silently editing a user's global
# instruction files is surprising, so this only runs when explicitly requested.
if [ "${JINDO_WRITE_GUIDANCE:-0}" = 1 ]; then
  echo "==> Injecting jindo usage guidance (JINDO_WRITE_GUIDANCE=1)"
  command -v codex  >/dev/null 2>&1 && inject_guidance "$HOME/.codex/AGENTS.md"
  command -v claude >/dev/null 2>&1 && inject_guidance "$HOME/.claude/CLAUDE.md"
else
  echo "==> Guidance injection skipped."
  echo "    Re-run with JINDO_WRITE_GUIDANCE=1 to add a jindo usage block to"
  echo "    ~/.codex/AGENTS.md and ~/.claude/CLAUDE.md (so hosts prefer delegating"
  echo "    to jindo, not just see the tools). Idempotent; backs up each file first."
fi

# Confirm the binary actually speaks MCP.
echo "==> Smoke test (MCP handshake)"
mcp_smoke="$(printf '%s\n%s\n' \
     '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
     '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | "$BIN")"
if grep -q '"jindo-mcp"' <<<"$mcp_smoke"; then
  echo "    OK: jindo-mcp responds and lists its tools"
else
  echo "    FAIL: no MCP response from $BIN" >&2
  exit 1
fi

echo "==> Done. Open a NEW host session to pick up the jindo server."
