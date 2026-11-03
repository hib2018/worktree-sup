package wsup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type conflictFakeEngine struct {
	resolve func(string) (Resolution, error)
	calls   int
}

func (e *conflictFakeEngine) Name(context.Context, string, string) (Naming, error) {
	return Naming{}, errors.New("unexpected naming call")
}
func (e *conflictFakeEngine) Resolve(_ context.Context, prompt string) (Resolution, error) {
	e.calls++
	return e.resolve(prompt)
}

func conflictFixture(t *testing.T) (*App, *State, *conflictFakeEngine, *bytes.Buffer) {
	t.Helper()
	r := testRepo(t)
	testGit(t, r.Root, "checkout", "-b", "source")
	writeTestFile(t, filepath.Join(r.Root, "tracked"), "source\n")
	testGit(t, r.Root, "commit", "-am", "source")
	source := testGit(t, r.Root, "rev-parse", "HEAD")
	testGit(t, r.Root, "checkout", "main")
	writeTestFile(t, filepath.Join(r.Root, "tracked"), "target\n")
	testGit(t, r.Root, "commit", "-am", "target")
	target := testGit(t, r.Root, "rev-parse", "HEAD")
	if _, err := r.Git(r.Root, "merge", "source"); err == nil {
		t.Fatal("expected conflict")
	}
	s := &State{Kind: "merge", Stage: "conflicted", Source: "source", Target: "main", SourcePath: r.Root, TargetPath: r.Root, SourceHEAD: source, TargetHEAD: target}
	engine := &conflictFakeEngine{}
	engine.resolve = func(prompt string) (Resolution, error) {
		var data struct {
			Source string         `json:"source_branch"`
			Target string         `json:"target_branch"`
			Files  []conflictFile `json:"files"`
		}
		if err := json.Unmarshal([]byte(prompt), &data); err != nil {
			t.Fatal(err)
		}
		if data.Source != "source" || data.Target != "main" || len(data.Files) != 1 {
			t.Fatalf("bad context: %s", prompt)
		}
		f := data.Files[0]
		if f.Base == nil || *f.Base != "initial\n" || f.Ours == nil || *f.Ours != "target\n" || f.Theirs == nil || *f.Theirs != "source\n" {
			t.Fatalf("bad stages: %+v", f)
		}
		old := strings.Split(strings.TrimSuffix(f.Current, "\n"), "\n")
		patch := "diff --git a/tracked b/tracked\n--- a/tracked\n+++ b/tracked\n@@ -1," + strconv.Itoa(len(old)) + " +1 @@\n"
		for _, line := range old {
			patch += "-" + line + "\n"
		}
		patch += "+resolved\n"
		return Resolution{Intent: "resolve intent", Plan: "resolve plan", Patch: patch}, nil
	}
	out := &bytes.Buffer{}
	a := &App{Repo: r, Engine: engine, In: bufio.NewReader(strings.NewReader("y\ny\n")), Out: out}
	return a, s, engine, out
}

func TestConflictResolveAndFinish(t *testing.T) {
	a, s, e, out := conflictFixture(t)
	if err := a.resolve(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.Stage != "completed" || e.calls != 1 {
		t.Fatalf("stage=%s calls=%d", s.Stage, e.calls)
	}
	if got := testGit(t, a.Repo.Root, "show", "HEAD:tracked"); got != "resolved" {
		t.Fatal(got)
	}
	if got := testGit(t, a.Repo.Root, "rev-list", "--parents", "-n", "1", "HEAD"); len(strings.Fields(got)) != 3 {
		t.Fatalf("not a merge commit: %s", got)
	}
	if !strings.Contains(out.String(), "Actual diff:") || !strings.Contains(out.String(), "resolve intent") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(a.Repo.Common, "wsup", "operation.json")); err != nil {
		t.Fatal(err)
	}
}

func TestConflictDeclineAndFreshResolution(t *testing.T) {
	a, s, e, _ := conflictFixture(t)
	before := snapshotTest(t, a.Repo)
	a.In = bufio.NewReader(strings.NewReader("N\n"))
	if err := a.resolve(context.Background(), s); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("decline: %v", err)
	}
	if s.Stage != "awaiting-approval" || s.Resolution == nil || s.Snapshot != before {
		t.Fatalf("unsaved approval state: %+v", s)
	}
	if after := snapshotTest(t, a.Repo); after != before {
		t.Fatal("decline mutated worktree/index")
	}
	a.In = bufio.NewReader(strings.NewReader("N\n"))
	_ = a.resolve(context.Background(), s)
	if e.calls != 2 {
		t.Fatal("reused cached resolution")
	}
}

func TestConflictModelMutationRejected(t *testing.T) {
	for _, mutation := range []string{"file", "source"} {
		t.Run(mutation, func(t *testing.T) {
			a, s, e, _ := conflictFixture(t)
			original := e.resolve
			e.resolve = func(prompt string) (Resolution, error) {
				result, err := original(prompt)
				if mutation == "file" {
					writeTestFile(t, filepath.Join(a.Repo.Root, "tracked"), "changed during model\n")
				} else {
					testGit(t, a.Repo.Root, "update-ref", "refs/heads/source", s.TargetHEAD)
				}
				return result, err
			}
			if err := a.resolve(context.Background(), s); err == nil {
				t.Fatal("accepted stale result")
			}
			if s.Stage == "completed" {
				t.Fatal("committed stale result")
			}
		})
	}
}

type conflictMutatingReader struct {
	action func()
	text   *strings.Reader
}

func (r *conflictMutatingReader) Read(p []byte) (int, error) {
	if r.action != nil {
		action := r.action
		r.action = nil
		action()
	}
	return r.text.Read(p)
}
func TestConflictApprovalMutationRejected(t *testing.T) {
	a, s, _, _ := conflictFixture(t)
	a.In = bufio.NewReader(&conflictMutatingReader{action: func() { writeTestFile(t, filepath.Join(a.Repo.Root, "tracked"), "changed during approval\n") }, text: strings.NewReader("y\n")})
	if err := a.resolve(context.Background(), s); err == nil || !strings.Contains(err.Error(), "snapshot changed") {
		t.Fatalf("approval mutation: %v", err)
	}
	if got := testGit(t, a.Repo.Root, "ls-files", "--unmerged"); got == "" {
		t.Fatal("staged stale resolution")
	}
}

func TestConflictPatchRestrictions(t *testing.T) {
	a, s, e, _ := conflictFixture(t)
	names := []string{"tracked"}
	prompt, err := conflictContext(s, names)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := e.resolve(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateConflictPatch(a.Repo.Root, resolution.Patch, names); err != nil {
		t.Fatal(err)
	}
	for name, patch := range map[string]string{
		"empty":     "",
		"outside":   strings.ReplaceAll(resolution.Patch, "tracked", "elsewhere"),
		"mode":      resolution.Patch + "diff --git a/tracked b/tracked\nold mode 100644\nnew mode 100755\n",
		"delete":    "diff --git a/tracked b/tracked\ndeleted file mode 100644\n--- a/tracked\n+++ /dev/null\n@@ -1 +0,0 @@\n-target\n",
		"binary":    "diff --git a/tracked b/tracked\nBinary files a/tracked and b/tracked differ\n",
		"rename":    "diff --git a/tracked b/elsewhere\nsimilarity index 100%\nrename from tracked\nrename to elsewhere\n",
		"duplicate": resolution.Patch + resolution.Patch,
		"traversal": strings.ReplaceAll(resolution.Patch, "tracked", "../tracked"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateConflictPatch(a.Repo.Root, patch, names); err == nil {
				t.Fatal("accepted restricted patch")
			}
		})
	}
}

func TestConflictRejectsNonTextAndSymlink(t *testing.T) {
	for _, kind := range []string{"binary", "symlink", "gitlink"} {
		t.Run(kind, func(t *testing.T) {
			a, s, e, _ := conflictFixture(t)
			switch kind {
			case "binary":
				writeTestFile(t, filepath.Join(a.Repo.Root, "tracked"), "bad\x00text")
			case "symlink":
				if err := os.Remove(filepath.Join(a.Repo.Root, "tracked")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("outside", filepath.Join(a.Repo.Root, "tracked")); err != nil {
					t.Fatal(err)
				}
			case "gitlink":
				// Replace stage 2 using index-info, retaining an unmerged entry.
				input := "160000 " + s.SourceHEAD + " 2\ttracked\n"
				if _, err := gitInput(a.Repo.Root, input, "update-index", "--index-info"); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.resolve(context.Background(), s); err == nil {
				t.Fatal("accepted unsupported conflict")
			}
			if e.calls != 0 {
				t.Fatal("sent unsupported content to model")
			}
		})
	}
}

func TestConflictContextLimit(t *testing.T) {
	a, s, e, _ := conflictFixture(t)
	writeTestFile(t, filepath.Join(a.Repo.Root, "tracked"), strings.Repeat("x", conflictContextLimit+1))
	if err := a.resolve(context.Background(), s); err == nil {
		t.Fatal("accepted oversized context")
	}
	if e.calls != 0 {
		t.Fatal("called engine")
	}
}

func TestConflictResolvedResumeRequiresSnapshotAndApproval(t *testing.T) {
	a, s, _, _ := conflictFixture(t)
	// Approve applying, decline finishing, then resume: no cached approval.
	a.In = bufio.NewReader(strings.NewReader("y\nN\n"))
	if err := a.resolve(context.Background(), s); err == nil {
		t.Fatal("expected finish decline")
	}
	if s.Stage != "resolved" {
		t.Fatal(s.Stage)
	}
	a.In = bufio.NewReader(strings.NewReader("N\n"))
	if err := a.resolve(context.Background(), s); err == nil {
		t.Fatal("resume reused approval")
	}
	writeTestFile(t, filepath.Join(a.Repo.Root, "tracked"), "manual correction\n")
	a.In = bufio.NewReader(strings.NewReader("y\n"))
	if err := a.resolve(context.Background(), s); err == nil {
		t.Fatal("accepted corrected resolved state")
	}
	if testGit(t, a.Repo.Root, "rev-parse", "HEAD") != s.TargetHEAD {
		t.Fatal("unexpected commit")
	}
}

// Invoked as a real child process by conflictRunTests; argv is not shell text.
func TestConflictCommandProcess(t *testing.T) {
	if os.Getenv("WSUP_CONFLICT_TEST_CHILD") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "fail":
		os.Exit(17)
	case "mutate-file":
		if err := os.WriteFile("tracked", []byte("test changed tracked file\n"), 0644); err != nil {
			t.Fatal(err)
		}
	case "mutate-index":
		if _, err := gitRaw(".", "update-index", "--index-version=4"); err != nil {
			t.Fatal(err)
		}
	case "mutate-source":
		if _, err := gitRaw(".", "update-ref", "refs/heads/source", "HEAD"); err != nil {
			t.Fatal(err)
		}
	case "remove-merge-head":
		r, err := Discover(".")
		if err != nil {
			t.Fatal(err)
		}
		p, err := r.gitPath(".", "MERGE_HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Remove(p); err != nil {
			t.Fatal(err)
		}
	case "literal; touch must-not-exist":
		if data, err := os.ReadFile("tracked"); err != nil || string(data) != "resolved\n" {
			t.Fatalf("wrong test cwd: %q %v", data, err)
		}
		t.Log("literal argv test passed")
	default:
		t.Fatal("unexpected child arguments")
	}
}

func TestConflictTestCommandGuards(t *testing.T) {
	for _, mode := range []string{"fail", "mutate-file", "mutate-index", "mutate-source", "remove-merge-head", "literal; touch must-not-exist"} {
		t.Run(mode, func(t *testing.T) {
			a, s, _, out := conflictFixture(t)
			a.In = bufio.NewReader(strings.NewReader("y\nN\n"))
			if err := a.resolve(context.Background(), s); err == nil || s.Stage != "resolved" {
				t.Fatalf("prepare resolved state: %s %v", s.Stage, err)
			}
			t.Setenv("WSUP_CONFLICT_TEST_CHILD", "1")
			argv := []string{os.Args[0], "-test.run=^TestConflictCommandProcess$", "-test.v", "--", mode}
			err := a.conflictRunTests(s, argv)
			if mode == "literal; touch must-not-exist" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), "literal argv test passed") {
					t.Fatal(out.String())
				}
				if _, err := os.Stat(filepath.Join(s.TargetPath, "must-not-exist")); !os.IsNotExist(err) {
					t.Fatal("shell executed test argument")
				}
			} else {
				if err == nil || s.Stage != "test-failed" {
					t.Fatalf("unsafe tests accepted: %s %v", s.Stage, err)
				}
				data, readErr := os.ReadFile(filepath.Join(a.Repo.Common, "wsup", "operation.json"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				var saved State
				if err := json.Unmarshal(data, &saved); err != nil {
					t.Fatal(err)
				}
				if saved.Stage != "test-failed" {
					t.Fatal("test failure not persisted")
				}
			}
			if head := testGit(t, a.Repo.Root, "rev-parse", "HEAD"); head != s.TargetHEAD {
				t.Fatal("tests caused a merge commit")
			}
		})
	}
}

func TestConflictCleanCompletionDoesNotCommit(t *testing.T) {
	r := testRepo(t)
	head := testGit(t, r.Root, "rev-parse", "HEAD")
	s := &State{Source: "main", Target: "main", SourcePath: r.Root, TargetPath: r.Root, SourceHEAD: head, TargetHEAD: head}
	a := &App{Repo: r, In: bufio.NewReader(strings.NewReader("y\n")), Out: &bytes.Buffer{}}
	if err := a.finishMerge(s); err != nil {
		t.Fatal(err)
	}
	if s.Stage != "completed" || testGit(t, r.Root, "rev-parse", "HEAD") != head {
		t.Fatal("created an unnecessary commit")
	}
}

func TestConflictUnmergedRequiredForNewResolution(t *testing.T) {
	a, s, e, _ := conflictFixture(t)
	testGit(t, a.Repo.Root, "add", "--", "tracked")
	s.Stage = "awaiting-approval"
	if err := a.resolve(context.Background(), s); err == nil {
		t.Fatal("accepted manually staged resolution")
	}
	if e.calls != 0 {
		t.Fatal("unexpected model call")
	}
}

func TestConflictPatchNULDelimitedNames(t *testing.T) {
	r := testRepo(t)
	for _, name := range []string{"tab\tand\nnewline", "literal[abc]*", "space name"} {
		writeTestFile(t, filepath.Join(r.Root, name), "old\n")
		old, newName := strconv.Quote("a/"+name), strconv.Quote("b/"+name)
		patch := "diff --git " + old + " " + newName + "\n--- " + old + "\n+++ " + newName + "\n@@ -1 +1 @@\n-old\n+new\n"
		if err := validateConflictPatch(r.Root, patch, []string{name}); err != nil {
			t.Fatalf("path %q: %v", name, err)
		}
		if err := validateConflictPatch(r.Root, patch, []string{"tracked"}); err == nil {
			t.Fatalf("allowed unapproved path %q", name)
		}
	}
}
