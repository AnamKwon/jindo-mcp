// Package policy is the orchestrator's CLI-agnostic dispatch gate for
// sensitive files. It exists because none of the three headless CLIs offer an
// equally reliable path-level write/edit deny: only claude has one
// (--disallowedTools "Write(<pattern>)"), live-confirmed to actually block a
// write (.env was NOT created when passed). codex (`codex exec --help`, full
// listing) and agy (`agy --help`) expose no equivalent — codex only has
// -s/--sandbox (directory-trust scoped, not per-file) and --add-dir; agy only
// has --sandbox (terminal restrictions) and --dangerously-skip-permissions.
// So the only gate that holds for all three CLIs uniformly is a check inside
// the orchestrator itself, run BEFORE any adapter is invoked.
package policy

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SensitivePatterns are filepath.Match glob patterns checked against
// path-like tokens found in task text. Patterns with no "/" are matched
// against a token's base name (so they catch the file regardless of the
// directory it's mentioned under); patterns with a "/" are matched as a
// substring against the whole (lowercased) token, since filepath.Match's "*"
// does not cross path separators the way a directory-prefix check needs to
// (e.g. matching ".ssh/*" inside "~/.ssh/id_rsa").
var SensitivePatterns = []string{
	".env", ".env.*",
	".mcp.json",
	".claude/settings.json", ".claude/settings.local.json",
	"id_rsa", "id_rsa.*", "id_ed25519", "id_ed25519.*",
	"*.pem", "*.key", "*.pfx",
	".npmrc", ".netrc",
	".aws/credentials", ".aws/config",
	".ssh/*",
	"credentials.json",
}

// tokenize splits task text into path-like candidate tokens, trimming common
// surrounding punctuation/quotes so phrasing like "the file `.env`." or
// "(see .mcp.json)" still yields a bare ".env" / ".mcp.json" token. Beyond
// whitespace it also breaks on '=' and ':', because assignment/annotation
// phrasing like ".env=SECRET" or ".env:xxx" would otherwise glue the value
// onto the filename and let it slip past the basename match.
func tokenize(task string) []string {
	fields := strings.FieldsFunc(task, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' ||
			r == '=' || r == ':'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// Note: "." is deliberately excluded from the cutset — these are
		// dotfiles/dotted paths (".env", ".mcp.json"), so trimming leading/
		// trailing dots would strip the very thing being matched. ':' is no
		// longer trimmed here because tokenize now splits on it above.
		f = strings.Trim(f, "`'\",;()[]{}<>")
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Check reports whether task text references a sensitive path, and if so,
// which pattern matched (for diagnostics/error messages). It is a pure,
// deterministic function over the task string only.
func Check(task string) (blocked bool, matched string) {
	for _, tok := range tokenize(task) {
		lower := strings.ToLower(tok)
		// A no-slash pattern is matched against every '/'-separated component of
		// the token, not just its final base name, so a sensitive file named as
		// a mid-path component (e.g. "cfg/.env/notes") is still caught.
		components := strings.Split(lower, "/")
		for _, pat := range SensitivePatterns {
			p := strings.ToLower(pat)
			if !strings.Contains(p, "/") {
				for _, comp := range components {
					if ok, _ := filepath.Match(p, comp); ok {
						return true, pat
					}
				}
				continue
			}
			prefix := strings.TrimSuffix(p, "/*")
			if strings.Contains(lower, prefix) {
				return true, pat
			}
		}
	}
	return false, ""
}

// BlockedError is returned by the orchestrator when Check blocks a task. It
// carries the matched pattern so callers/logs can explain the refusal.
type BlockedError struct {
	Task    string
	Pattern string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("policy: task references sensitive path (matched %q); dispatch refused before any agent ran", e.Pattern)
}

// ClaudeDisallowedToolArgs builds --disallowedTools flags for claude as a
// defense-in-depth layer on top of the orchestrator gate above: the gate only
// inspects the ORIGINAL task text, so it cannot catch an agent that decides,
// mid-task, to write a sensitive file never mentioned in that text. claude's
// --disallowedTools blocks the write/edit at the tool-call layer regardless of
// why the agent attempted it. Only claude gets this; codex/agy have no
// equivalent flag (see package doc).
func ClaudeDisallowedToolArgs() []string {
	args := make([]string, 0, len(SensitivePatterns)*2+1)
	args = append(args, "--disallowedTools")
	for _, pat := range SensitivePatterns {
		args = append(args, fmt.Sprintf("Write(%s)", pat), fmt.Sprintf("Edit(%s)", pat))
	}
	return args
}
