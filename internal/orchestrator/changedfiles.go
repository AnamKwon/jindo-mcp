package orchestrator

// changedfiles.go computes a best-effort manifest of files a dispatch created,
// modified, or deleted, by diffing two `git status --porcelain` snapshots taken
// around the pipeline (see dispatch in orchestrator.go). This is GIT-ONLY: a
// non-git cwd, or any git failure, yields ok=false and dispatch simply leaves
// Result.Files nil — additive and backward compatible, never a dispatch failure.

import (
	"os/exec"
	"sort"
	"strings"
)

// ChangedFile is one entry in a dispatch's file-change manifest: the
// repo-relative path and a coarse status derived from the git porcelain code.
type ChangedFile struct {
	Path   string
	Status string
}

// gitStatusSnapshot runs `git status --porcelain=v1` in cwd (via exec.Command,
// never a shell) and returns a map of repo-relative path -> the raw 2-character
// porcelain status code. ok is false if cwd is not a git repo or git is
// unavailable/errors, so the caller can treat the whole feature as a no-op.
func gitStatusSnapshot(cwd string) (map[string]string, bool) {
	c := exec.Command("git", "-C", cwd, "status", "--porcelain=v1")
	out, err := c.Output()
	if err != nil {
		return nil, false
	}
	snap := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		code := line[:2]
		path := strings.TrimSpace(line[3:])
		// A rename/copy line has the form "old -> new"; the path we care about
		// (the one that now exists on disk) is the new name.
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		snap[path] = code
	}
	return snap, true
}

// porcelainStatus maps a 2-character git porcelain status code to a coarse,
// caller-facing status string.
func porcelainStatus(code string) string {
	switch {
	case code == "??":
		return "untracked"
	case strings.HasPrefix(code, "A") || strings.HasSuffix(code, "A"):
		return "added"
	case strings.HasPrefix(code, "D") || strings.HasSuffix(code, "D"):
		return "deleted"
	case strings.HasPrefix(code, "R"):
		return "renamed"
	default:
		return "modified"
	}
}

// changedFilesSince takes a fresh git status snapshot of cwd and compares it
// against before (captured earlier in the same pipeline via gitStatusSnapshot),
// returning every path whose status is new or changed, sorted by path for a
// deterministic manifest. Returns nil if the after-snapshot fails (e.g. the repo
// vanished mid-dispatch) or nothing changed.
//
// KNOWN LIMITATION: a path already dirty before the dispatch (present in
// before) that is modified again during the dispatch WITHOUT its porcelain code
// changing (e.g. already " M" and still " M" after further edits) is not
// detected — the snapshot only sees the coarse status code, not content.
func changedFilesSince(cwd string, before map[string]string) []ChangedFile {
	after, ok := gitStatusSnapshot(cwd)
	if !ok {
		return nil
	}
	var out []ChangedFile
	for path, code := range after {
		if prev, existed := before[path]; existed && prev == code {
			continue
		}
		out = append(out, ChangedFile{Path: path, Status: porcelainStatus(code)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// changedFilePaths projects a ChangedFile manifest to just its repo-relative
// paths, preserving order. Returns nil for an empty manifest so callers can pass
// the result straight into an optional field without introducing an empty slice.
func changedFilePaths(files []ChangedFile) []string {
	if len(files) == 0 {
		return nil
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}
