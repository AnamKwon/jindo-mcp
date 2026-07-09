package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitStatusSnapshot_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	_, ok := gitStatusSnapshot(dir)
	if ok {
		t.Fatalf("expected ok=false for a non-git directory")
	}
}

func TestChangedFilesSince(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "test")

	// initial committed file, so we can modify/delete it later.
	keepPath := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keepPath, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	delPath := filepath.Join(dir, "del.txt")
	if err := os.WriteFile(delPath, []byte("gone soon"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "init")

	before, ok := gitStatusSnapshot(dir)
	if !ok {
		t.Fatalf("expected ok=true for a git repo")
	}
	if len(before) != 0 {
		t.Fatalf("expected clean before-snapshot, got %v", before)
	}

	// Simulate a dispatch: add a new file, modify an existing one, delete another.
	newPath := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(newPath, []byte("added"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepPath, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(delPath); err != nil {
		t.Fatal(err)
	}

	got := changedFilesSince(dir, before)
	want := []ChangedFile{
		{Path: "del.txt", Status: "deleted"},
		{Path: "keep.txt", Status: "modified"},
		{Path: "new.txt", Status: "untracked"},
	}
	if len(got) != len(want) {
		t.Fatalf("changedFilesSince() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
