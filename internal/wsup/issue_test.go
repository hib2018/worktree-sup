package wsup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fakeIssueGH(t *testing.T, data string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
[ "$*" = 'api --method GET --paginate --slurp repos/{owner}/{repo}/issues?state=open&per_page=100' ] || exit 91
[ -z "$GH_REPO" ] || exit 92
[ -z "$GIT_DIR$GIT_WORK_TREE$GIT_CONFIG_COUNT" ] || exit 95
[ "$GH_PROMPT_DISABLED" = 1 ] || exit 93
[ "$PWD" = "$WSUP_TEST_ROOT" ] || exit 94
if [ "$WSUP_TEST_GH_FAIL" = 1 ]; then echo 'authentication or fetch failed' >&2; exit 1; fi
cat "$WSUP_TEST_ISSUES"
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(dir, "issues.json")
	if err := os.WriteFile(dataPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WSUP_TEST_ISSUES", dataPath)
	t.Setenv("GH_REPO", "wrong/repository")
	t.Setenv("WSUP_TEST_GH_FAIL", "")
}

const issuePages = `[[{"number":1,"title":"元のタイトル","html_url":"https://github.com/owner/repo/issues/1"},{"number":99,"title":"PR","pull_request":{"url":"x"}}],[{"number":2,"title":"Second issue","html_url":"https://github.com/owner/repo/issues/2"}]]`

func issueFixture(t *testing.T, data string) (*App, *appTestEngine, *bytes.Buffer) {
	t.Helper()
	a, e, out := appTestFixture(t)
	fakeIssueGH(t, data)
	t.Setenv("WSUP_TEST_ROOT", a.Repo.Root)
	return a, e, out
}

func TestIssuePagesAndRerun(t *testing.T) {
	a, engine, out := issueFixture(t, issuePages)
	appTestOK(t, a, "main\n", "issue")
	if strings.Count(out.String(), "Base branch:") != 1 {
		t.Fatal(out.String())
	}
	for _, number := range []int{1, 2} {
		branch := "issue-1"
		title := "元のタイトル"
		if number == 2 {
			branch = "issue-2"
			title = "Second issue"
		}
		e := appTestEntry(t, a, branch)
		if e.IssueNumber != number || e.Summary != title || e.IssueURL != "https://github.com/owner/repo/issues/"+strings.TrimPrefix(branch, "issue-") || e.Base != "main" {
			t.Fatalf("wrong metadata: %+v", e)
		}
		if err := a.verifyEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	if a.Repo.BranchExists("issue-99") || engine.names != 0 {
		t.Fatal("PR or model was used")
	}
	before := cancelSnapshot(t, a)
	out.Reset()
	appTestOK(t, a, "", "issue", "--base", "main")
	cancelAssertUnchanged(t, a, before)
	if strings.Count(out.String(), "Skipped:") != 2 {
		t.Fatal(out.String())
	}
	out.Reset()
	appTestOK(t, a, "", "list")
	if !strings.Contains(out.String(), "元のタイトル") {
		t.Fatal(out.String())
	}
}

func TestIssueFetchBeforeChanges(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		fail       bool
		wantErr    bool
	}{
		{name: "empty", data: `[[]]`},
		{name: "PR-only", data: `[[{"pull_request":{},"number":4}]]`},
		{name: "fetch-error", data: issuePages, fail: true, wantErr: true},
		{name: "malformed-later-page", data: `[[{"number":1,"title":"one","html_url":"https://github.com/o/r/issues/1"}],`, wantErr: true},
		{name: "invalid-later-issue", data: `[[{"number":1,"title":"one","html_url":"https://github.com/o/r/issues/1"}],[{"number":2}]]`, wantErr: true},
		{name: "null", data: `null`, wantErr: true},
		{name: "duplicate", data: `[[{"number":1,"title":"one","html_url":"https://github.com/o/r/issues/1"},{"number":1,"title":"one","html_url":"https://github.com/o/r/issues/1"}]]`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, engine, _ := issueFixture(t, tc.data)
			if tc.fail {
				t.Setenv("WSUP_TEST_GH_FAIL", "1")
			}
			before := cancelSnapshot(t, a)
			err := appTestExecute(a, "", "issue")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error: %v", err)
			}
			cancelAssertUnchanged(t, a, before)
			if _, err := os.Stat(a.Repo.Root + ".worktrees"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination created: %v", err)
			}
			if engine.names != 0 {
				t.Fatal("model called")
			}
		})
	}
}

func TestIssueCollisionKeepsEarlierWorktrees(t *testing.T) {
	for _, mode := range []string{"branch", "path", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			a, _, out := issueFixture(t, issuePages)
			switch mode {
			case "branch":
				testGit(t, a.Repo.Root, "branch", "issue-2")
			case "path":
				path := filepath.Join(a.Repo.Root+".worktrees", "issue-2")
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(path, "keep"), "user data")
			case "metadata":
				if err := writeJSON(a.storage("metadata.json"), Metadata{Entries: map[string]Entry{"issue-2": {Branch: "issue-2"}}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := appTestExecute(a, "", "issue", "--base", "main"); err == nil {
				t.Fatal("collision accepted")
			}
			if err := a.verifyEntry(appTestEntry(t, a, "issue-1")); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Created: #1") {
				t.Fatal(out.String())
			}
			if mode == "path" {
				data, err := os.ReadFile(filepath.Join(a.Repo.Root+".worktrees", "issue-2", "keep"))
				if err != nil || string(data) != "user data" {
					t.Fatalf("lost user data: %q %v", data, err)
				}
			}
		})
	}
}

type issueCancelWriter struct {
	out     *bytes.Buffer
	cancel  context.CancelFunc
	trigger string
}

func (w issueCancelWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	if strings.Contains(string(p), w.trigger) {
		w.cancel()
	}
	return n, err
}

func TestIssueCancellationAndRecovery(t *testing.T) {
	for _, stage := range []string{"before-create", "after-first"} {
		t.Run(stage, func(t *testing.T) {
			a, _, out := issueFixture(t, issuePages)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			trigger := "Creation path:"
			if stage == "after-first" {
				trigger = "Created:"
			}
			a.Out = issueCancelWriter{out, cancel, trigger}
			a.In = bufio.NewReader(strings.NewReader(""))
			err := a.Execute(ctx, []string{"issue", "--base", "main"})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("not canceled: %v", err)
			}
			if a.Repo.BranchExists("issue-2") || !strings.Contains(out.String(), "Unprocessed: #2") {
				t.Fatal(out.String())
			}
			a.Out = out
			if stage == "before-create" {
				appTestState(t, a, "new", "planned")
				appTestOK(t, a, "yes\n", "resume")
			}
			if err := a.verifyEntry(appTestEntry(t, a, "issue-1")); err != nil {
				t.Fatal(err)
			}
			appTestOK(t, a, "", "issue", "--base", "main")
			if err := a.verifyEntry(appTestEntry(t, a, "issue-2")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIssueAbortKeepsResources(t *testing.T) {
	a, _, out := issueFixture(t, issuePages)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Out = issueCancelWriter{out, cancel, "Creation path:"}
	a.In = bufio.NewReader(strings.NewReader(""))
	if err := a.Execute(ctx, []string{"issue", "--base", "main"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	before := cancelSnapshot(t, a)
	a.Out = out
	appTestOK(t, a, "yes\n", "abort")
	after := cancelSnapshot(t, a)
	delete(before, "operation.json")
	delete(after, "operation.json")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("abort changed resources")
	}
}

func TestIssueSkipRequiresMatchingLiveWorktree(t *testing.T) {
	for _, mode := range []string{"missing", "other-URL"} {
		t.Run(mode, func(t *testing.T) {
			a, _, _ := issueFixture(t, issuePages)
			appTestOK(t, a, "", "issue", "--base", "main")
			e := appTestEntry(t, a, "issue-1")
			if mode == "missing" {
				testGit(t, a.Repo.Root, "worktree", "remove", e.Path)
			} else {
				m, err := a.loadMetadata()
				if err != nil {
					t.Fatal(err)
				}
				e.IssueURL = "https://github.com/other/repo/issues/1"
				m.Entries[e.Branch] = e
				if err := writeJSON(a.storage("metadata.json"), m); err != nil {
					t.Fatal(err)
				}
			}
			before := cancelSnapshot(t, a)
			if err := appTestExecute(a, "", "issue", "--base", "main"); err == nil {
				t.Fatal("unverified entry accepted")
			}
			cancelAssertUnchanged(t, a, before)
		})
	}
}

func TestIssueFetchEnvironmentAndMissingCLI(t *testing.T) {
	a, _, _ := issueFixture(t, `[[]]`)
	t.Setenv("GIT_DIR", "/not/this/repository")
	t.Setenv("GIT_WORK_TREE", "/not/this/worktree")
	t.Setenv("GIT_CONFIG_COUNT", "99")
	if _, err := a.openIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := a.openIssues(context.Background()); err == nil {
		t.Fatal("missing gh accepted")
	}
}

func TestIssueCancelFetch(t *testing.T) {
	a, _, _ := appTestFixture(t)
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	script := "#!/bin/sh\nsleep 30 &\necho ready > \"$WSUP_TEST_READY\"\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WSUP_TEST_READY", ready)
	before := cancelSnapshot(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- a.Execute(ctx, []string{"issue", "--base", "main"}) }()
	deadline := time.After(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("gh did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := cancelAwait(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("not canceled: %v", err)
	}
	cancelAssertUnchanged(t, a, before)
}

func TestIssueMetadataRoundTrip(t *testing.T) {
	data, err := json.Marshal(Entry{IssueNumber: 1, IssueURL: "https://github.com/o/r/issues/1"})
	if err != nil || !strings.Contains(string(data), `"issue_number":1`) {
		t.Fatalf("%s %v", data, err)
	}
}
