package wsup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// All app commands use this fake, never Run (which constructs a live FX).
type appTestEngine struct {
	t       *testing.T
	naming  Naming
	names   int
	intents []string
}

func (e *appTestEngine) Name(_ context.Context, intent, context string) (Naming, error) {
	e.names++
	e.intents = append(e.intents, intent)
	if !strings.Contains(context, "main") {
		e.t.Errorf("missing base in naming context: %q", context)
	}
	return e.naming, nil
}
func (e *appTestEngine) Resolve(context.Context, string) (Resolution, error) {
	e.t.Error("unexpected resolution call in clean app flow")
	return Resolution{}, errors.New("unexpected resolution")
}

func appTestFixture(t *testing.T) (*App, *appTestEngine, *bytes.Buffer) {
	t.Helper()
	r := testRepo(t)
	// new stores worktrees next to the main repository, outside testRepo's dir.
	t.Cleanup(func() {
		if err := os.RemoveAll(r.Root + ".worktrees"); err != nil {
			t.Error(err)
		}
	})
	e := &appTestEngine{t: t, naming: Naming{Branch: "topic/app", Summary: "アプリのテスト"}}
	out := &bytes.Buffer{}
	return &App{Repo: r, Engine: e, Out: out}, e, out
}

func appTestExecute(a *App, input string, args ...string) error {
	a.In = bufio.NewReader(strings.NewReader(input))
	return a.Execute(context.Background(), args)
}
func appTestOK(t *testing.T, a *App, input string, args ...string) {
	t.Helper()
	if err := appTestExecute(a, input, args...); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
}
func appTestState(t *testing.T, a *App, kind, stage string) *State {
	t.Helper()
	s, err := a.loadState()
	if err != nil || s == nil || s.Kind != kind || s.Stage != stage {
		t.Fatalf("want %s/%s, got %+v, %v", kind, stage, s, err)
	}
	return s
}
func appTestEntry(t *testing.T, a *App, branch string) Entry {
	t.Helper()
	m, err := a.loadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := m.Entries[branch]
	if !ok {
		t.Fatalf("missing metadata for %s: %+v", branch, m)
	}
	return e
}
func appTestCreate(t *testing.T, a *App) Entry {
	t.Helper()
	appTestOK(t, a, "", "new", "--base", "main", "test intent")
	return appTestEntry(t, a, "topic/app")
}
func appTestCommit(t *testing.T, path string) {
	t.Helper()
	writeTestFile(t, filepath.Join(path, "feature"), "feature data\n")
	testGit(t, path, "add", "feature")
	testGit(t, path, "commit", "-m", "feature")
}

func TestAppNewListMergeRemove(t *testing.T) {
	for _, mode := range []string{"fast-forward", "merge-commit"} {
		t.Run(mode, func(t *testing.T) {
			a, engine, out := appTestFixture(t)
			e := appTestCreate(t, a)
			if e.Base != "main" || e.Target != "main" || e.Summary != engine.naming.Summary || e.Path != filepath.Join(a.Repo.Root+".worktrees", "topic-app") {
				t.Fatalf("wrong entry: %+v", e)
			}
			if engine.names != 1 || !reflect.DeepEqual(engine.intents, []string{"test intent"}) {
				t.Fatalf("naming calls: %+v", engine)
			}
			appTestState(t, a, "new", "completed")
			appTestCommit(t, e.Path)
			source := testGit(t, e.Path, "rev-parse", "HEAD")
			target := testGit(t, a.Repo.Root, "rev-parse", "HEAD")
			if mode == "merge-commit" {
				writeTestFile(t, filepath.Join(a.Repo.Root, "target-only"), "target\n")
				testGit(t, a.Repo.Root, "add", "target-only")
				testGit(t, a.Repo.Root, "commit", "-m", "target")
				target = testGit(t, a.Repo.Root, "rev-parse", "HEAD")
			}
			out.Reset()
			appTestOK(t, a, "", "list")
			for _, text := range []string{"BRANCH", "main", e.Branch, e.Summary, e.Path, "clean"} {
				if !strings.Contains(out.String(), text) {
					t.Errorf("list missing %q: %s", text, out)
				}
			}
			// Merge selects the managed entry; remove additionally requires literal yes.
			appTestOK(t, a, "1\n", "merge", "--target", "main")
			appTestState(t, a, "merge", "completed")
			if mode == "fast-forward" {
				if got := testGit(t, a.Repo.Root, "rev-parse", "HEAD"); got != source {
					t.Fatalf("merge did not fast-forward: %s", got)
				}
			} else if got := testGit(t, a.Repo.Root, "rev-list", "--parents", "-n", "1", "HEAD"); !strings.HasSuffix(got, " "+target+" "+source) {
				t.Fatalf("wrong merge parents: %s", got)
			}
			if _, err := os.Stat(e.Path); err != nil || !a.Repo.BranchExists(e.Branch) {
				t.Fatalf("merge removed source: %v", err)
			}
			appTestOK(t, a, "1\nyes\n", "remove")
			appTestState(t, a, "remove", "completed")
			if _, err := os.Stat(e.Path); !errors.Is(err, os.ErrNotExist) || a.Repo.BranchExists(e.Branch) {
				t.Fatalf("source not removed: %v", err)
			}
			m, err := a.loadMetadata()
			if err != nil || len(m.Entries) != 0 {
				t.Fatalf("stale metadata: %+v, %v", m, err)
			}
			if got := testGit(t, a.Repo.Root, "show", "HEAD:feature"); got != "feature data" {
				t.Fatal("removed merged data")
			}
			if err := a.Repo.Clean(a.Repo.Root); err != nil {
				t.Fatal(err)
			}
			if engine.names != 1 {
				t.Fatal("non-new command invoked naming")
			}
		})
	}
}

// Ignored files do not make Clean fail, but are still user data: an incoming
// tracked file at the same path must not silently replace them during merge.
func TestAppMergePreservesIgnoredTargetData(t *testing.T) {
	for _, mode := range []string{"fast-forward", "merge-commit"} {
		t.Run(mode, func(t *testing.T) {
			a, engine, _ := appTestFixture(t)
			e := appTestCreate(t, a)
			appTestCommit(t, e.Path)
			if mode == "merge-commit" {
				writeTestFile(t, filepath.Join(a.Repo.Root, "target-only"), "target\n")
				testGit(t, a.Repo.Root, "add", "target-only")
				testGit(t, a.Repo.Root, "commit", "-m", "target")
			}
			writeTestFile(t, filepath.Join(a.Repo.Common, "info", "exclude"), "feature\n")
			file := filepath.Join(a.Repo.Root, "feature")
			const precious = "ignored local data, not the incoming feature\n"
			writeTestFile(t, file, precious)
			if err := a.Repo.Clean(a.Repo.Root); err != nil {
				t.Fatalf("ignored data should reach the merge safety check: %v", err)
			}
			testGit(t, a.Repo.Root, "check-ignore", "feature")
			before := snapshotTest(t, a.Repo)
			sourceBefore, err := a.Repo.Snapshot(e.Path)
			if err != nil {
				t.Fatal(err)
			}
			err = appTestExecute(a, "1\n", "merge")
			if err == nil || !strings.Contains(err.Error(), "ignoredファイルと衝突") || !strings.Contains(err.Error(), "feature") {
				t.Errorf("want ignored-file collision error identifying feature, got: %v", err)
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != precious {
				t.Fatalf("merge overwrote ignored data: %q, %v", data, err)
			}
			if after := snapshotTest(t, a.Repo); after != before {
				t.Fatal("rejected merge changed target HEAD, index or tracked data")
			}
			sourceAfter, err := a.Repo.Snapshot(e.Path)
			if err != nil || sourceAfter != sourceBefore {
				t.Fatalf("rejected merge changed source: %v", err)
			}
			appTestState(t, a, "merge", "planned")
			appTestEntry(t, a, e.Branch)
			if engine.names != 1 {
				t.Fatal("merge invoked naming")
			}
		})
	}
}

func TestAppNewRejectsNaming(t *testing.T) {
	for name, naming := range map[string]Naming{
		"duplicate":         {Branch: "main", Summary: "valid"},
		"invalid branch":    {Branch: "bad..branch", Summary: "valid"},
		"empty branch":      {Summary: "valid"},
		"blank summary":     {Branch: "topic/app", Summary: " \t"},
		"multiline summary": {Branch: "topic/app", Summary: "one\ntwo"},
		"tab summary":       {Branch: "topic/app", Summary: "one\ttwo"},
		"nul summary":       {Branch: "topic/app", Summary: "one\x00two"},
	} {
		t.Run(name, func(t *testing.T) {
			a, engine, _ := appTestFixture(t)
			engine.naming = naming
			head := testGit(t, a.Repo.Root, "rev-parse", "HEAD")
			if err := appTestExecute(a, "", "new", "--base", "main", "intent"); err == nil {
				t.Fatal("accepted invalid naming")
			}
			trees, err := a.Repo.Worktrees()
			if err != nil || len(trees) != 1 || testGit(t, a.Repo.Root, "rev-parse", "HEAD") != head {
				t.Fatalf("rejected naming mutated repo: %+v, %v", trees, err)
			}
			m, err := a.loadMetadata()
			if err != nil || len(m.Entries) != 0 {
				t.Fatalf("rejected naming persisted metadata: %+v, %v", m, err)
			}
			if s, err := a.loadState(); err != nil || s != nil {
				t.Fatalf("rejected naming saved operation: %+v, %v", s, err)
			}
		})
	}
}

func TestAppMergeRejectsDirtySourceAndTarget(t *testing.T) {
	for _, side := range []string{"source", "target"} {
		for _, kind := range []string{"tracked", "staged", "untracked"} {
			t.Run(side+"/"+kind, func(t *testing.T) {
				a, _, _ := appTestFixture(t)
				e := appTestCreate(t, a)
				appTestCommit(t, e.Path)
				path := e.Path
				if side == "target" {
					path = a.Repo.Root
				}
				file := "tracked"
				if kind == "untracked" {
					file = "untracked"
				}
				writeTestFile(t, filepath.Join(path, file), "user data\n")
				if kind == "staged" {
					testGit(t, path, "add", file)
				}
				beforeSource, err := a.Repo.Snapshot(e.Path)
				if err != nil {
					t.Fatal(err)
				}
				beforeTarget := snapshotTest(t, a.Repo)
				if err := appTestExecute(a, "1\n", "merge"); err == nil {
					t.Fatal("merged dirty worktree")
				}
				afterSource, err := a.Repo.Snapshot(e.Path)
				if err != nil || beforeSource != afterSource || beforeTarget != snapshotTest(t, a.Repo) {
					t.Fatalf("rejected merge changed worktree/index: %v", err)
				}
				appTestState(t, a, "new", "completed")
			})
		}
	}
}

func TestAppSelectionAndExplicitRemovalApproval(t *testing.T) {
	for _, input := range []string{"0\n", "2\n", "no\n", "", "1\nno\n", "1\ny\n", "1\nYES\n", "1\nyes"} {
		t.Run(strings.ReplaceAll(input, "\n", "_"), func(t *testing.T) {
			a, _, _ := appTestFixture(t)
			e := appTestCreate(t, a) // Already an ancestor of main; safe to remove only with yes.
			if err := appTestExecute(a, input, "remove"); err == nil {
				t.Fatal("removed without valid selection and explicit yes")
			}
			if _, err := os.Stat(e.Path); err != nil || !a.Repo.BranchExists(e.Branch) {
				t.Fatalf("lost source: %v", err)
			}
			appTestEntry(t, a, e.Branch)
			appTestState(t, a, "new", "completed")
		})
	}
}

func TestAppRemoveRefusesUnmergedAndIgnoredData(t *testing.T) {
	for _, kind := range []string{"unmerged", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			a, _, _ := appTestFixture(t)
			e := appTestCreate(t, a)
			file := filepath.Join(e.Path, "feature")
			want := "feature data\n"
			if kind == "unmerged" {
				appTestCommit(t, e.Path)
			} else {
				writeTestFile(t, filepath.Join(a.Repo.Common, "info", "exclude"), "precious\n")
				file = filepath.Join(e.Path, "precious")
				want = "ignored user data\n"
				writeTestFile(t, file, want)
			}
			err := appTestExecute(a, "1\nyes\n", "remove")
			if err == nil {
				t.Fatal("unsafe removal accepted")
			}
			if kind == "ignored" && !strings.Contains(err.Error(), "ignored") {
				t.Fatalf("wrong refusal: %v", err)
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != want {
				t.Fatalf("lost data: %q, %v", data, err)
			}
			if !a.Repo.BranchExists(e.Branch) {
				t.Fatal("deleted source branch")
			}
			appTestEntry(t, a, e.Branch)
		})
	}
}

func TestAppSharedMetadataAndConfig(t *testing.T) {
	a, engine, _ := appTestFixture(t)
	c, err := a.loadConfig()
	if err != nil || len(c.Test) != 0 {
		t.Fatalf("default config: %+v, %v", c, err)
	}
	e := appTestCreate(t, a)
	r, err := Discover(e.Path)
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	linked := &App{Repo: r, Engine: engine, Out: out}
	if linked.storage("metadata.json") != a.storage("metadata.json") {
		t.Fatal("metadata is not repository-shared")
	}
	if got := appTestEntry(t, linked, e.Branch); got != e {
		t.Fatalf("linked entry: %+v", got)
	}
	appTestOK(t, linked, "", "list")
	if !strings.Contains(out.String(), e.Summary) {
		t.Fatal(out.String())
	}
	argv := []string{"go", "test", "./...", "literal; no shell"}
	appTestOK(t, linked, "", append([]string{"config", "test", "--"}, argv...)...)
	c, err = a.loadConfig()
	if err != nil || !reflect.DeepEqual(c.Test, argv) {
		t.Fatalf("shared argv config: %+v, %v", c, err)
	}
	for _, args := range [][]string{{"config"}, {"config", "test"}, {"config", "test", "go"}, {"config", "test", "--"}} {
		if err := appTestExecute(a, "", args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
		c, err = linked.loadConfig()
		if err != nil || !reflect.DeepEqual(c.Test, argv) {
			t.Fatalf("invalid command changed config: %+v, %v", c, err)
		}
	}
	appTestOK(t, a, "", "config", "test", "--clear")
	c, err = linked.loadConfig()
	if err != nil || len(c.Test) != 0 {
		t.Fatalf("clear config: %+v, %v", c, err)
	}
}

// Seed durable state to simulate termination without timing-dependent subprocess kills.
func appTestPlannedCreate(t *testing.T, a *App) *State {
	t.Helper()
	e := Entry{Branch: "topic/app", Summary: "復旧", Base: "main", Target: "main", Path: filepath.Join(a.Repo.Root+".worktrees", "topic-app")}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0700); err != nil {
		t.Fatal(err)
	}
	s := &State{Kind: "new", Stage: "planned", Source: e.Branch, Target: e.Target, SourcePath: e.Path, TargetHEAD: testGit(t, a.Repo.Root, "rev-parse", "HEAD"), Entry: &e}
	if err := a.saveState(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAppInterruptedCreateRecovery(t *testing.T) {
	for _, stage := range []string{"planned", "added-before-save", "worktree-created"} {
		t.Run(stage, func(t *testing.T) {
			a, engine, out := appTestFixture(t)
			s := appTestPlannedCreate(t, a)
			if stage != "planned" {
				testGit(t, a.Repo.Root, "worktree", "add", "-b", s.Source, s.SourcePath, s.TargetHEAD)
			}
			if stage == "worktree-created" {
				s.Stage = stage
				if err := a.saveState(s); err != nil {
					t.Fatal(err)
				}
			}
			appTestOK(t, a, "", "status")
			if !strings.Contains(out.String(), "new / "+s.Stage) {
				t.Fatal(out.String())
			}
			if err := appTestExecute(a, "", "new", "--base", "main", "another"); err == nil {
				t.Fatal("did not block pending operation")
			}
			if err := appTestExecute(a, "no\n", "resume"); err == nil {
				t.Fatal("resumed without approval")
			}
			appTestState(t, a, "new", s.Stage)
			appTestOK(t, a, "yes\n", "resume")
			appTestState(t, a, "new", "completed")
			if got := appTestEntry(t, a, s.Source); got != *s.Entry {
				t.Fatalf("recovered entry: %+v", got)
			}
			if engine.names != 0 {
				t.Fatal("resume regenerated naming")
			}
			if err := appTestExecute(a, "yes\n", "resume"); err == nil {
				t.Fatal("resumed completed operation")
			}
		})
	}
}

func TestAppInterruptedCreateRejectsChangedBase(t *testing.T) {
	a, _, _ := appTestFixture(t)
	s := appTestPlannedCreate(t, a)
	testGit(t, a.Repo.Root, "commit", "--allow-empty", "-m", "base advanced")
	if err := appTestExecute(a, "yes\n", "resume"); err == nil {
		t.Fatal("resumed against changed base")
	}
	if a.Repo.BranchExists(s.Source) {
		t.Fatal("created branch despite changed base")
	}
	appTestState(t, a, "new", "planned")
}

func TestAppInterruptedRemoveRecovery(t *testing.T) {
	for _, stage := range []string{"planned", "removed-before-save", "worktree-removed", "branch-deleted"} {
		t.Run(stage, func(t *testing.T) {
			a, _, _ := appTestFixture(t)
			e := appTestCreate(t, a)
			s := &State{Kind: "remove", Stage: "planned", Source: e.Branch, Target: e.Target, SourcePath: e.Path, SourceHEAD: testGit(t, e.Path, "rev-parse", "HEAD"), Entry: &e}
			if stage != "planned" {
				testGit(t, a.Repo.Root, "worktree", "remove", e.Path)
			}
			if stage == "worktree-removed" || stage == "branch-deleted" {
				s.Stage = "worktree-removed"
			}
			if stage == "branch-deleted" {
				testGit(t, a.Repo.Root, "update-ref", "-d", "refs/heads/"+e.Branch, s.SourceHEAD)
			}
			if err := a.saveState(s); err != nil {
				t.Fatal(err)
			}
			if err := appTestExecute(a, "no\n", "resume"); err == nil {
				t.Fatal("resumed without approval")
			}
			appTestOK(t, a, "yes\n", "resume")
			appTestState(t, a, "remove", "completed")
			if a.Repo.BranchExists(e.Branch) {
				t.Fatal("branch survived removal")
			}
			if _, err := os.Stat(e.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("path survived: %v", err)
			}
			m, err := a.loadMetadata()
			if err != nil || len(m.Entries) != 0 {
				t.Fatalf("metadata survived: %+v, %v", m, err)
			}
		})
	}
}

func TestAppResumedRemoveRefusesCheckedOutSource(t *testing.T) {
	for _, location := range []string{"main-worktree", "another-linked-worktree", "original-path"} {
		t.Run(location, func(t *testing.T) {
			a, engine, _ := appTestFixture(t)
			e := appTestCreate(t, a)
			head := testGit(t, e.Path, "rev-parse", "HEAD")
			testGit(t, a.Repo.Root, "worktree", "remove", e.Path)
			s := &State{Kind: "remove", Stage: "worktree-removed", Source: e.Branch, Target: e.Target, SourcePath: e.Path, SourceHEAD: head, Entry: &e}
			if err := a.saveState(s); err != nil {
				t.Fatal(err)
			}
			// Before resume, someone checks out the still-existing source ref.
			path := a.Repo.Root
			switch location {
			case "main-worktree":
				testGit(t, path, "checkout", e.Branch)
			case "another-linked-worktree":
				path = filepath.Join(t.TempDir(), "reused-source")
				testGit(t, a.Repo.Root, "worktree", "add", path, e.Branch)
			case "original-path":
				path = e.Path
				testGit(t, a.Repo.Root, "worktree", "add", path, e.Branch)
			}
			writeTestFile(t, filepath.Join(path, "user-data"), "keep me\n")
			before, err := a.Repo.Snapshot(path)
			if err != nil {
				t.Fatal(err)
			}
			trees := testGit(t, a.Repo.Root, "worktree", "list", "--porcelain")
			err = appTestExecute(a, "yes\n", "resume")
			if err == nil || !strings.Contains(err.Error(), "worktree") {
				t.Fatalf("resume did not refuse a checked-out source: %v", err)
			}
			if got := testGit(t, a.Repo.Root, "rev-parse", "refs/heads/"+e.Branch); got != head {
				t.Fatalf("source ref changed: %s", got)
			}
			if got := testGit(t, path, "symbolic-ref", "--short", "HEAD"); got != e.Branch {
				t.Fatalf("source checkout changed: %s", got)
			}
			after, err := a.Repo.Snapshot(path)
			if err != nil || before != after {
				t.Fatalf("resume changed the active source worktree: %v", err)
			}
			if got := testGit(t, a.Repo.Root, "worktree", "list", "--porcelain"); got != trees {
				t.Fatal("resume changed worktree registrations")
			}
			appTestState(t, a, "remove", "worktree-removed")
			if got := appTestEntry(t, a, e.Branch); got != e {
				t.Fatal("resume changed metadata on refusal")
			}
			if engine.names != 1 {
				t.Fatal("resume invoked naming")
			}
		})
	}
}

func TestAppResumedCreateRejectsReplacedParent(t *testing.T) {
	for _, stage := range []string{"planned", "worktree-created"} {
		t.Run(stage, func(t *testing.T) {
			a, engine, _ := appTestFixture(t)
			s := appTestPlannedCreate(t, a)
			if stage == "worktree-created" {
				testGit(t, a.Repo.Root, "worktree", "add", "-b", s.Source, s.SourcePath, s.TargetHEAD)
				s.Stage = stage
				if err := a.saveState(s); err != nil {
					t.Fatal(err)
				}
			}
			// Preserve any created worktree, but replace its parent with a symlink.
			// Both stages must revalidate the parent before creating or registering.
			parent := filepath.Dir(s.SourcePath)
			redirected := filepath.Join(t.TempDir(), "redirected-parent")
			if err := os.Rename(parent, redirected); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(redirected, parent); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(redirected, "precious"), "outside user data\n")
			before := appTestDiskImage(t, redirected)
			trees := testGit(t, a.Repo.Root, "worktree", "list", "--porcelain")
			err := appTestExecute(a, "yes\n", "resume")
			if err == nil || !strings.Contains(err.Error(), "保存先") {
				t.Fatalf("resume did not reject a noncanonical parent: %v", err)
			}
			if !reflect.DeepEqual(before, appTestDiskImage(t, redirected)) {
				t.Fatal("resume wrote through the replaced parent")
			}
			if got := testGit(t, a.Repo.Root, "worktree", "list", "--porcelain"); got != trees {
				t.Fatal("resume changed worktree registrations")
			}
			if exists := a.Repo.BranchExists(s.Source); exists != (stage == "worktree-created") {
				t.Fatal("resume created or deleted the source ref")
			}
			appTestState(t, a, "new", stage)
			m, err := a.loadMetadata()
			if err != nil || len(m.Entries) != 0 {
				t.Fatalf("resume registered unsafe worktree: %+v, %v", m, err)
			}
			if engine.names != 0 {
				t.Fatal("resume invoked naming")
			}
		})
	}
}

func TestAppAbortPreservesCreatedWorktree(t *testing.T) {
	for _, kind := range []string{"new", "remove"} {
		t.Run(kind, func(t *testing.T) {
			a, _, _ := appTestFixture(t)
			e := appTestCreate(t, a)
			s := &State{Kind: kind, Stage: "planned", Source: e.Branch, Target: e.Target, SourcePath: e.Path, Entry: &e}
			if kind == "new" {
				s.Stage = "worktree-created"
			}
			if err := a.saveState(s); err != nil {
				t.Fatal(err)
			}
			if err := appTestExecute(a, "no\n", "abort"); err == nil {
				t.Fatal("aborted without approval")
			}
			appTestState(t, a, kind, s.Stage)
			appTestOK(t, a, "yes\n", "abort")
			appTestState(t, a, kind, "aborted")
			if _, err := os.Stat(e.Path); err != nil || !a.Repo.BranchExists(e.Branch) {
				t.Fatalf("abort deleted work: %v", err)
			}
			appTestEntry(t, a, e.Branch)
		})
	}
}

// Include contents, modes and mtimes, so a rewrite with identical bytes is detected.
func appTestDiskImage(t *testing.T, root string) map[string]string {
	t.Helper()
	image := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String() + " " + info.ModTime().String()
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += " " + string(data)
		}
		image[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func TestAppListReadOnlyWithoutEngine(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pristine", true: "managed"}[managed], func(t *testing.T) {
			a, engine, out := appTestFixture(t)
			if managed {
				appTestCreate(t, a)
			}
			writeTestFile(t, filepath.Join(a.Repo.Root, "tracked"), "dirty user file\n")
			before := appTestDiskImage(t, a.Repo.Root)
			var worktrees map[string]string
			if managed {
				worktrees = appTestDiskImage(t, a.Repo.Root+".worktrees")
			}
			calls := engine.names
			a.Engine = nil // list must not require or construct an engine.
			out.Reset()
			for i := 0; i < 2; i++ {
				appTestOK(t, a, "", "list")
			}
			if !strings.Contains(out.String(), "dirty") || !strings.Contains(out.String(), "未登録") {
				t.Fatal(out.String())
			}
			if !reflect.DeepEqual(before, appTestDiskImage(t, a.Repo.Root)) {
				t.Fatal("list wrote repository, index, lock or metadata")
			}
			if managed && !reflect.DeepEqual(worktrees, appTestDiskImage(t, a.Repo.Root+".worktrees")) {
				t.Fatal("list wrote linked worktree")
			}
			if engine.names != calls {
				t.Fatal("list called engine")
			}
		})
	}
}

func TestAppStoreRejectsMalformedDocuments(t *testing.T) {
	for _, tc := range []struct{ file, data, command string }{
		{"metadata.json", `{"entries":{},"unknown":true}`, "list"},
		{"metadata.json", `{"entries":{}} {}`, "list"},
		{"metadata.json", `{`, "list"},
		{"operation.json", `{"Kind":"unknown","Stage":"planned"}`, "status"},
		{"operation.json", `{"Kind":"new","Stage":"planned","unknown":true}`, "status"},
	} {
		t.Run(tc.file+"/"+tc.data, func(t *testing.T) {
			a, _, _ := appTestFixture(t)
			writeTestFile(t, a.storage(tc.file), tc.data)
			before := appTestDiskImage(t, a.Repo.Root)
			if err := appTestExecute(a, "", tc.command); err == nil {
				t.Fatal("accepted malformed storage")
			}
			if !reflect.DeepEqual(before, appTestDiskImage(t, a.Repo.Root)) {
				t.Fatal("read command rewrote malformed storage")
			}
		})
	}
}
