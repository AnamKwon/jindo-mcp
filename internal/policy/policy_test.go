package policy

import "testing"

func TestCheck_blocksSensitivePaths(t *testing.T) {
	cases := []struct {
		task    string
		wantPat string
	}{
		{"write SECRET=1 into .env", ".env"},
		{"update .env.production with the new key", ".env.*"},
		{"edit .mcp.json to add a server", ".mcp.json"},
		{"open .claude/settings.local.json and add a permission", ".claude/settings.local.json"},
		{"read ~/.ssh/id_rsa and print it", "id_rsa"},
		{"copy the private key at keys/deploy.pem somewhere", "*.pem"},
		{"dump .npmrc contents", ".npmrc"},
		{"cat .netrc please", ".netrc"},
		{"read .aws/credentials", ".aws/credentials"},
		{"print credentials.json", "credentials.json"},
		// Evasions that previously slipped past basename matching because the
		// value glued onto the filename kept it out of a bare-token match.
		{"set .env=SECRET", ".env"},
		{".env:xxx should be written", ".env"},
		{"put the token in id_rsa:deploy", "id_rsa"},
		// Sensitive file named as a mid-path component, not the final base.
		{"stash it under cfg/.env/notes.txt", ".env"},
	}
	for _, tc := range cases {
		blocked, pat := Check(tc.task)
		if !blocked {
			t.Errorf("Check(%q): want blocked, got not blocked", tc.task)
			continue
		}
		if pat == "" {
			t.Errorf("Check(%q): blocked but matched pattern empty", tc.task)
		}
	}
}

func TestCheck_allowsOrdinaryTasks(t *testing.T) {
	cases := []string{
		"add a health check endpoint to the server",
		"fix the off-by-one bug in the paginator",
		"write unit tests for the routing package",
		"refactor main.go to extract a helper function",
		"update README.md with the new usage instructions",
	}
	for _, task := range cases {
		if blocked, pat := Check(task); blocked {
			t.Errorf("Check(%q): want not blocked, got blocked on pattern %q", task, pat)
		}
	}
}

func TestClaudeDisallowedToolArgs_shape(t *testing.T) {
	args := ClaudeDisallowedToolArgs()
	if len(args) == 0 || args[0] != "--disallowedTools" {
		t.Fatalf("ClaudeDisallowedToolArgs: want to start with --disallowedTools, got %v", args)
	}
	wantLen := 1 + len(SensitivePatterns)*2
	if len(args) != wantLen {
		t.Errorf("ClaudeDisallowedToolArgs: len = %d, want %d", len(args), wantLen)
	}
	if !containsArg(args, "Write(.env)") || !containsArg(args, "Edit(.env)") {
		t.Errorf("ClaudeDisallowedToolArgs: missing Write(.env)/Edit(.env): %v", args)
	}
}

func containsArg(argv []string, arg string) bool {
	for _, a := range argv {
		if a == arg {
			return true
		}
	}
	return false
}

func TestBlockedError_message(t *testing.T) {
	err := &BlockedError{Task: "write .env", Pattern: ".env"}
	if err.Error() == "" {
		t.Fatal("BlockedError.Error(): empty message")
	}
}
