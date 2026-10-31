package wsup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRaw(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func testRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	testGit(t, dir, "init", "--initial-branch=main")
	testGit(t, dir, "config", "user.name", "Test")
	testGit(t, dir, "config", "user.email", "test@example.invalid")
	testGit(t, dir, "config", "core.autocrlf", "false")
	testGit(t, dir, "config", "core.splitIndex", "false")
	writeTestFile(t, filepath.Join(dir, "tracked"), "initial\n")
	testGit(t, dir, "add", ".")
	testGit(t, dir, "commit", "-m", "initial")
	r, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func samePath(t *testing.T, got, want string) {
	t.Helper()
	g, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := filepath.EvalSymlinks(want)
	if err != nil || g != w {
		t.Fatalf("path %q != %q: %v", g, w, err)
	}
}

func TestDiscoverAndWorktrees(t *testing.T) {
	r := testRepo(t)
	sub := filepath.Join(r.Root, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	found, err := Discover(sub)
	if err != nil {
		t.Fatal(err)
	}
	samePath(t, found.Root, r.Root)
	if !filepath.IsAbs(found.Common) {
		t.Fatal("common directory is not absolute")
	}
	linked := filepath.Join(t.TempDir(), "linked\n worktree ")
	testGit(t, r.Root, "worktree", "add", "-b", "feature", linked)
	linkedRepo, err := Discover(linked)
	if err != nil {
		t.Fatal(err)
	}
	samePath(t, linkedRepo.Root, linked)
	samePath(t, linkedRepo.Common, r.Common)
	worktrees, err := linkedRepo.Worktrees()
	if err != nil || len(worktrees) != 2 {
		t.Fatalf("worktrees: %+v, %v", worktrees, err)
	}
	if worktrees[0].Branch != "main" || worktrees[1].Branch != "feature" || worktrees[1].Path != linkedRepo.Root {
		t.Fatalf("wrong worktrees: %+v", worktrees)
	}
	testGit(t, linked, "checkout", "--detach")
	worktrees, err = r.Worktrees()
	if err != nil || !worktrees[1].Detached || worktrees[1].Branch != "" || worktrees[1].HEAD == "" {
		t.Fatalf("detached worktree: %+v, %v", worktrees, err)
	}
	if _, err := Discover(t.TempDir()); err == nil {
		t.Fatal("discovered a nonrepository")
	}
}

func TestBareRepository(t *testing.T) {
	dir := t.TempDir()
	testGit(t, dir, "init", "--bare")
	r, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	samePath(t, r.Root, dir)
	worktrees, err := r.Worktrees()
	if err != nil || len(worktrees) != 1 || !worktrees[0].Bare {
		t.Fatalf("bare worktrees: %+v, %v", worktrees, err)
	}
}

func TestBranchesHeadAndGit(t *testing.T) {
	r := testRepo(t)
	for _, branch := range []string{"", "HEAD", "@", "-bad", "--help", "@{-1}", "a..b", "a b", "a\nb", "a.lock", "a~b", "a:b", "a?b", "a\\b", "a/", "/a", "a\x00b"} {
		if ValidBranch(branch) == nil || r.BranchExists(branch) {
			t.Errorf("accepted invalid branch %q", branch)
		}
	}
	for _, branch := range []string{"main", "topic/sub", "日本語", "feature-1"} {
		if err := ValidBranch(branch); err != nil {
			t.Errorf("rejected valid branch %q: %v", branch, err)
		}
	}
	if !r.BranchExists("main") || r.BranchExists("missing") {
		t.Fatal("branch existence mismatch")
	}
	head, err := r.Head(r.Root)
	if err != nil || head != testGit(t, r.Root, "rev-parse", "HEAD") {
		t.Fatalf("HEAD: %q, %v", head, err)
	}
	if _, err := r.Git(r.Root, "not-a-git-command"); err == nil || !strings.Contains(err.Error(), "not a git command") {
		t.Fatalf("missing stderr: %v", err)
	}
	// Inherited environment must not redirect repository or index selection.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "nonexistent"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "index"))
	if got, err := r.Head(r.Root); err != nil || got != head {
		t.Fatalf("environment redirected HEAD: %q, %v", got, err)
	}
}

func TestGitDisablesHooksAndSigning(t *testing.T) {
	r := testRepo(t)
	hooks := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(hooks, "pre-commit")
	writeTestFile(t, hook, "#!/bin/sh\nprintf ran > '"+marker+"'\nexit 1\n")
	if err := os.Chmod(hook, 0755); err != nil {
		t.Fatal(err)
	}
	testGit(t, r.Root, "config", "core.hooksPath", hooks)
	testGit(t, r.Root, "config", "commit.gpgsign", "true")
	testGit(t, r.Root, "config", "gpg.program", "/nonexistent-gpg")
	if _, err := r.Git(r.Root, "commit", "--allow-empty", "-m", "safe"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hook ran: %v", err)
	}
}

func TestClean(t *testing.T) {
	r := testRepo(t)
	if err := r.Clean(r.Root); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(r.Root, "untracked\nfile"), "new")
	if err := r.Clean(r.Root); err == nil {
		t.Fatal("accepted untracked file")
	}
	if err := os.Remove(filepath.Join(r.Root, "untracked\nfile")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(r.Root, "tracked"), "dirty")
	if err := r.Clean(r.Root); err == nil {
		t.Fatal("accepted dirty file")
	}
	testGit(t, r.Root, "add", "tracked")
	if err := r.Clean(r.Root); err == nil {
		t.Fatal("accepted staged change")
	}
	testGit(t, r.Root, "commit", "-m", "change")
	linked := filepath.Join(t.TempDir(), "linked")
	testGit(t, r.Root, "worktree", "add", "-b", "linked", linked)
	for _, name := range operationPaths {
		t.Run(name, func(t *testing.T) {
			p, err := r.gitPath(linked, name)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(name, "rebase-") || name == "sequencer" {
				if err := os.Mkdir(p, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTestFile(t, p, "operation")
			}
			if err := r.Clean(linked); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("accepted ongoing %s: %v", name, err)
			}
			if err := r.Clean(r.Root); err != nil {
				t.Fatalf("operation leaked into main worktree: %v", err)
			}
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func snapshotTest(t *testing.T, r *Repo) string {
	t.Helper()
	s, err := r.Snapshot(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 64 {
		t.Fatalf("invalid fingerprint: %q", s)
	}
	return s
}

func TestSnapshotContentAndIndex(t *testing.T) {
	r := testRepo(t)
	last := snapshotTest(t, r)
	if got := snapshotTest(t, r); got != last {
		t.Fatal("unchanged snapshot differs")
	}
	changed := func() {
		t.Helper()
		next := snapshotTest(t, r)
		if next == last {
			t.Fatal("snapshot missed a change")
		}
		last = next
	}
	writeTestFile(t, filepath.Join(r.Root, "tracked"), "dirty A\n")
	changed()
	writeTestFile(t, filepath.Join(r.Root, "tracked"), "dirty B\n")
	changed() // Same status and size, different working content.
	testGit(t, r.Root, "add", "tracked")
	changed()
	writeTestFile(t, filepath.Join(r.Root, "odd\n name \t"), "untracked A")
	changed()
	writeTestFile(t, filepath.Join(r.Root, "odd\n name \t"), "untracked B")
	changed()
	if err := os.Remove(filepath.Join(r.Root, "tracked")); err != nil {
		t.Fatal(err)
	}
	changed()
	testGit(t, r.Root, "commit", "--allow-empty", "-m", "head change")
	changed()
	writeTestFile(t, filepath.Join(r.Root, "nested", "file"), "nested")
	changed()
	if err := os.Chmod(filepath.Join(r.Root, "nested", "file"), 0755); err != nil {
		t.Fatal(err)
	}
	changed()
	index, err := r.gitPath(r.Root, "index")
	if err != nil {
		t.Fatal(err)
	}
	// A valid index extension generated by Git must also affect the snapshot.
	testGit(t, r.Root, "update-index", "--index-version=4")
	changed()
	if _, err := os.Stat(index); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotSymlinks(t *testing.T) {
	r := testRepo(t)
	outside := filepath.Join(t.TempDir(), "outside")
	writeTestFile(t, outside, "secret A")
	link := filepath.Join(r.Root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	before := snapshotTest(t, r)
	writeTestFile(t, outside, "secret B")
	if got := snapshotTest(t, r); got != before {
		t.Fatal("snapshot traversed a symlink")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside+"-missing", link); err != nil {
		t.Fatal(err)
	}
	if got := snapshotTest(t, r); got == before {
		t.Fatal("snapshot missed symlink target change")
	}
	writeTestFile(t, filepath.Join(r.Root, "nested", "file"), "tracked")
	testGit(t, r.Root, "add", "nested/file")
	if err := os.RemoveAll(filepath.Join(r.Root, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(r.Root, "nested")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Snapshot(r.Root); err == nil {
		t.Fatal("accepted symlinked parent of tracked path")
	}
}

func TestSnapshotConflict(t *testing.T) {
	r := testRepo(t)
	testGit(t, r.Root, "checkout", "-b", "other")
	writeTestFile(t, filepath.Join(r.Root, "tracked"), "other\n")
	testGit(t, r.Root, "commit", "-am", "other")
	testGit(t, r.Root, "checkout", "main")
	writeTestFile(t, filepath.Join(r.Root, "tracked"), "main\n")
	testGit(t, r.Root, "commit", "-am", "main")
	if _, err := r.Git(r.Root, "merge", "other"); err == nil {
		t.Fatal("expected merge conflict")
	}
	before := snapshotTest(t, r)
	writeTestFile(t, filepath.Join(r.Root, "tracked"), "<<<<<<< changed conflict markers\n")
	if after := snapshotTest(t, r); after == before {
		t.Fatal("snapshot missed conflict contents")
	}
}

func TestSnapshotUnborn(t *testing.T) {
	dir := t.TempDir()
	testGit(t, dir, "init", "--initial-branch=main")
	r, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTest(t, r)
	writeTestFile(t, filepath.Join(dir, "new"), "new")
	if after := snapshotTest(t, r); after == before {
		t.Fatal("missed unborn worktree change")
	}
}

func TestSnapshotRejectsSpecialFiles(t *testing.T) {
	r := testRepo(t)
	fifo := filepath.Join(r.Root, "tracked")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Snapshot(r.Root); err == nil {
		t.Fatal("accepted FIFO")
	}
}

func TestLock(t *testing.T) {
	r := testRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	testGit(t, r.Root, "worktree", "add", "-b", "linked", linked)
	other, err := Discover(linked)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := r.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	lockPath := filepath.Join(r.Common, "wsup.lock")
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if release, err := other.Lock(); err == nil {
		release()
		t.Fatal("allowed simultaneous locks")
	}
	unlock()
	unlock() // Release is idempotent.
	release, err := other.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	after, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("lock inode replaced or removed: %v", err)
	}
}

func TestLockProcessExit(t *testing.T) {
	if common := os.Getenv("WSUP_TEST_LOCK_COMMON"); common != "" {
		r := &Repo{Common: common}
		if _, err := r.Lock(); err != nil {
			t.Fatal(err)
		}
		return // No unlock: process termination must release the lock.
	}
	r := testRepo(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockProcessExit$")
	cmd.Env = append(os.Environ(), "WSUP_TEST_LOCK_COMMON="+r.Common)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %s: %v", out, err)
	}
	release, err := r.Lock()
	if err != nil {
		t.Fatal(err)
	}
	release()
}
