package wsup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func batchMergeEntry(t *testing.T, a *App, branch, path, base string) Entry {
	t.Helper()
	testGit(t, a.Repo.Root, "worktree", "add", "-b", branch, path, base)
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	e := Entry{Branch: branch, Path: path, Base: "main", Target: "main", Summary: branch}
	m, err := a.loadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	m.Entries[branch] = e
	if err := writeJSON(a.storage("metadata.json"), m); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestBatchMergeSelection(t *testing.T) {
	for _, selection := range []string{"all", "2 1", "1", "1 1", "1 3", "", "all 1", "1 bad"} {
		t.Run(selection, func(t *testing.T) {
			a, _, out := appTestFixture(t)
			dir := t.TempDir()
			entries := []Entry{
				batchMergeEntry(t, a, "first", filepath.Join(dir, "a-first"), "main"),
				batchMergeEntry(t, a, "second", filepath.Join(dir, "b-second"), "main"),
			}
			for _, e := range entries {
				writeTestFile(t, filepath.Join(e.Path, e.Branch), e.Branch+"\n")
				testGit(t, e.Path, "add", e.Branch)
				testGit(t, e.Path, "commit", "-m", e.Branch)
			}
			before := testGit(t, a.Repo.Root, "rev-parse", "HEAD")
			err := appTestExecute(a, selection+"\n", "merge")
			valid := selection == "all" || selection == "2 1" || selection == "1"
			if !valid {
				if err == nil || !strings.Contains(err.Error(), "invalid selection") {
					t.Fatalf("invalid selection accepted: %v", err)
				}
				if got := testGit(t, a.Repo.Root, "rev-parse", "HEAD"); got != before {
					t.Fatal("invalid selection changed target")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for i, e := range entries {
				_, err := a.Repo.Git(a.Repo.Root, "merge-base", "--is-ancestor", e.Branch, "main")
				wantMerged := selection != "1" || i == 0
				if (err == nil) != wantMerged {
					t.Fatalf("%s merged=%v, want %v", e.Branch, err == nil, wantMerged)
				}
			}
			if selection == "2 1" {
				if strings.Index(out.String(), "Source: second") > strings.Index(out.String(), "Source: first") {
					t.Fatal("did not preserve selected order")
				}
			}
			appTestState(t, a, "merge", "completed")
		})
	}
}

func TestBatchMergeStopsOnDirtySource(t *testing.T) {
	a, _, out := appTestFixture(t)
	dir := t.TempDir()
	first := batchMergeEntry(t, a, "first", filepath.Join(dir, "a-first"), "main")
	second := batchMergeEntry(t, a, "second", filepath.Join(dir, "b-second"), "main")
	third := batchMergeEntry(t, a, "third", filepath.Join(dir, "c-third"), "main")
	appTestCommit(t, first.Path)
	appTestCommit(t, third.Path)
	writeTestFile(t, filepath.Join(second.Path, "untracked"), "keep\n")
	err := appTestExecute(a, "all\n", "merge")
	if err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("want dirty error, got %v", err)
	}
	if got := testGit(t, a.Repo.Root, "rev-parse", "HEAD"); got != testGit(t, first.Path, "rev-parse", "HEAD") {
		t.Fatal("completed merge lost or later merge executed")
	}
	if !strings.Contains(out.String(), "Batch stopped.") || strings.Contains(out.String(), "Source: third") {
		t.Fatal(out.String())
	}
	appTestState(t, a, "merge", "completed")
}

func TestBatchMergeConflictRequiresUserDecision(t *testing.T) {
	for _, tc := range []struct {
		name, input, stage string
		engineFailure      bool
		continued          bool
	}{
		{name: "decline patch", input: "N\n", stage: "awaiting-approval"},
		{name: "decline completion", input: "y\nN\n", stage: "resolved"},
		{name: "decline next merge", input: "y\ny\nN\n", stage: "completed"},
		{name: "missing continuation approval", input: "y\ny\n", stage: "completed"},
		{name: "approve next merge", input: "y\ny\ny\n", stage: "completed", continued: true},
		{name: "engine failure", stage: "conflicted", engineFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, engine, out := conflictFixture(t)
			testGit(t, a.Repo.Root, "merge", "--abort")
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "a-source")
			testGit(t, a.Repo.Root, "worktree", "add", sourcePath, "source")
			sourcePath, err := filepath.EvalSymlinks(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(a.storage("metadata.json"), Metadata{Entries: map[string]Entry{
				"source": {Branch: "source", Path: sourcePath, Base: "main", Target: "main"},
			}}); err != nil {
				t.Fatal(err)
			}
			next := batchMergeEntry(t, a, "next", filepath.Join(dir, "b-next"), "main")
			appTestCommit(t, next.Path)
			if tc.engineFailure {
				engine.resolve = func(string) (Resolution, error) { return Resolution{}, errors.New("model failed") }
			}
			err = appTestExecute(a, "1 2\n"+tc.input, "merge")
			if (err == nil) != tc.continued {
				t.Fatalf("continued=%v, error=%v", tc.continued, err)
			}
			if engine.calls != 1 {
				t.Fatalf("resolution calls=%d", engine.calls)
			}
			appTestState(t, a, "merge", tc.stage)
			_, ancestorErr := a.Repo.Git(a.Repo.Root, "merge-base", "--is-ancestor", "next", "main")
			if (ancestorErr == nil) != tc.continued {
				t.Fatalf("next merge ran without approval: %v", ancestorErr)
			}
			if !tc.continued && !strings.Contains(out.String(), "Batch stopped.") {
				t.Fatal(out.String())
			}
			if tc.stage == "completed" && !strings.Contains(out.String(), "Conflict resolved. Continue with the next selected merge?") {
				t.Fatal("missing continuation prompt")
			}
		})
	}
}

func TestBatchMergeTargets(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded targets", true: "shared override"}[override], func(t *testing.T) {
			a, _, _ := appTestFixture(t)
			dir := t.TempDir()
			releasePath := filepath.Join(dir, "release")
			testGit(t, a.Repo.Root, "worktree", "add", "-b", "release", releasePath, "main")
			first := batchMergeEntry(t, a, "first", filepath.Join(dir, "a-first"), "main")
			second := batchMergeEntry(t, a, "second", filepath.Join(dir, "b-second"), "main")
			for _, e := range []Entry{first, second} {
				writeTestFile(t, filepath.Join(e.Path, e.Branch), "feature\n")
				testGit(t, e.Path, "add", e.Branch)
				testGit(t, e.Path, "commit", "-m", e.Branch)
			}
			m, err := a.loadMetadata()
			if err != nil {
				t.Fatal(err)
			}
			second.Target = "release"
			m.Entries[second.Branch] = second
			if err := writeJSON(a.storage("metadata.json"), m); err != nil {
				t.Fatal(err)
			}
			args := []string{"merge"}
			if override {
				args = append(args, "--target", "release")
			}
			appTestOK(t, a, "all\n", args...)
			for _, e := range []Entry{first, second} {
				target := e.Target
				if override {
					target = "release"
				}
				if _, err := a.Repo.Git(a.Repo.Root, "merge-base", "--is-ancestor", e.Branch, target); err != nil {
					t.Fatalf("%s not merged into %s: %v", e.Branch, target, err)
				}
				if got := appTestEntry(t, a, e.Branch).Target; got != target {
					t.Fatalf("recorded target=%s, want %s", got, target)
				}
			}
		})
	}
}

func TestBatchMergeEntryCanceled(t *testing.T) {
	a, _, _ := appTestFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.mergeEntry(ctx, Entry{}, "main", &State{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
}
