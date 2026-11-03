package wsup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Include durable application data, refs, worktree registration, and each
// worktree's HEAD/index/files, rather than checking only the returned error.
func cancelSnapshot(t *testing.T, a *App) map[string]string {
	t.Helper()
	got := map[string]string{
		"refs":      testGit(t, a.Repo.Root, "show-ref"),
		"worktrees": testGit(t, a.Repo.Root, "worktree", "list", "--porcelain"),
	}
	for _, name := range []string{"operation.json", "metadata.json", "config.json"} {
		data, err := os.ReadFile(a.storage(name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got[name] = string(data)
	}
	trees, err := a.Repo.Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	for _, tree := range trees {
		snapshot, err := a.Repo.Snapshot(tree.Path)
		if err != nil {
			t.Fatal(err)
		}
		got[tree.Path] = snapshot
	}
	return got
}

func cancelAssertUnchanged(t *testing.T, a *App, before map[string]string) {
	t.Helper()
	if after := cancelSnapshot(t, a); !reflect.DeepEqual(before, after) {
		t.Fatalf("cancellation changed application/repository state:\nbefore: %v\nafter: %v", before, after)
	}
}

func cancelAwait(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("canceled command did not return within 2 seconds")
		return nil
	}
}

func TestCancelAppPrompts(t *testing.T) {
	cases := []struct {
		name, prefix string
		args         []string
		setup        string
	}{
		{name: "new-task", args: []string{"new"}},
		{name: "new-base", prefix: "task intent\n", args: []string{"new"}},
		{name: "merge-selection", args: []string{"merge"}, setup: "entry"},
		{name: "remove-selection", args: []string{"remove"}, setup: "entry"},
		{name: "remove-confirmation", prefix: "1\n", args: []string{"remove"}, setup: "entry"},
		{name: "resume-confirmation", args: []string{"resume"}, setup: "planned"},
		{name: "abort-confirmation", args: []string{"abort"}, setup: "planned"},
	}
	for _, key := range []struct{ name, input string }{{"ctrl-c", "\x03"}, {"escape", "\x1b"}} {
		for _, tc := range cases {
			t.Run(key.name+"/"+tc.name, func(t *testing.T) {
				a, engine, _ := appTestFixture(t)
				switch tc.setup {
				case "entry":
					appTestCreate(t, a)
				case "planned":
					appTestPlannedCreate(t, a)
				}
				before := cancelSnapshot(t, a)
				names := engine.names
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				a.In = bufio.NewReader(reader)
				result := make(chan error, 1)
				go func() { result <- a.Execute(context.Background(), tc.args) }()
				// Keep the pipe open: neither key needs Enter or EOF to cancel.
				written := make(chan error, 1)
				go func() {
					_, err := io.WriteString(writer, tc.prefix+key.input)
					written <- err
				}()
				if err := cancelAwait(t, result); !errors.Is(err, ErrCanceled) {
					t.Fatalf("want ErrCanceled, got %v", err)
				}
				if err := cancelAwait(t, written); err != nil {
					t.Fatal(err)
				}
				if engine.names != names {
					t.Fatal("canceled prompt called naming engine")
				}
				cancelAssertUnchanged(t, a, before)
			})
		}
	}
}

// Signal when readPrompt has actually reached the otherwise blocked pipe read.
type cancelStartedReader struct {
	io.Reader
	started chan struct{}
	once    sync.Once
}

func (r *cancelStartedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.Reader.Read(p)
}

func TestCancelAppBlockedPromptContext(t *testing.T) {
	a, engine, _ := appTestFixture(t)
	before := cancelSnapshot(t, a)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	started := make(chan struct{})
	a.In = bufio.NewReader(&cancelStartedReader{Reader: reader, started: started})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- a.Execute(ctx, []string{"new"}) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("command never reached prompt read")
	}
	cancel()
	if err := cancelAwait(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	// Close both ends only after proving Execute returned without their help.
	reader.Close()
	writer.Close()
	if engine.names != 0 {
		t.Fatal("blocked prompt called naming engine")
	}
	cancelAssertUnchanged(t, a, before)
}

type cancelNamingEngine struct {
	cancel context.CancelFunc
	calls  int
}

func (e *cancelNamingEngine) Name(context.Context, string, string) (Naming, error) {
	e.calls++
	e.cancel()
	// Deliberately ignore ctx and return a valid successful model response.
	return Naming{Branch: "topic/canceled", Summary: "Canceled naming"}, nil
}

func (*cancelNamingEngine) Resolve(context.Context, string) (Resolution, error) {
	return Resolution{}, errors.New("unexpected resolution call")
}

func TestCancelAppAfterNaming(t *testing.T) {
	a, _, _ := appTestFixture(t)
	before := cancelSnapshot(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine := &cancelNamingEngine{cancel: cancel}
	a.Engine = engine
	a.In = bufio.NewReader(strings.NewReader(""))
	err := a.Execute(ctx, []string{"new", "--base", "main", "intent"})
	if !errors.Is(err, context.Canceled) || engine.calls != 1 {
		t.Fatalf("err=%v naming calls=%d", err, engine.calls)
	}
	cancelAssertUnchanged(t, a, before)
	if _, err := os.Stat(a.Repo.Root + ".worktrees"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled naming created worktree parent: %v", err)
	}
}

// The descendant inherits the command's stdout pipe. Killing only its parent
// would leave that pipe open until WaitDelay expires instead of returning fast.
func TestCancelCommandProcess(t *testing.T) {
	if os.Getenv("WSUP_CANCEL_TEST_CHILD") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestCancelCommandProcess$", "--", "descendant")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	case "descendant":
		fmt.Printf("wsup-cancel-ready %d\n", os.Getpid())
		time.Sleep(30 * time.Second)
	default:
		t.Fatalf("unexpected child mode: %s", mode)
	}
}

type cancelCommandOutput struct {
	mu     sync.Mutex
	data   bytes.Buffer
	ready  chan int
	cancel context.CancelFunc
	once   sync.Once
}

func (w *cancelCommandOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.data.Write(p)
	text := w.data.String()
	if start := strings.Index(text, "wsup-cancel-ready "); start >= 0 && strings.Contains(text[start:], "\n") {
		var pid int
		if _, scanErr := fmt.Sscanf(text[start:], "wsup-cancel-ready %d", &pid); scanErr == nil {
			w.once.Do(func() {
				w.ready <- pid
				w.cancel()
			})
		}
	}
	return n, err
}

func TestCancelAppConfiguredTests(t *testing.T) {
	a, s, engine, _ := conflictFixture(t)
	// Resolve once with the fake model, but decline completion for now.
	a.In = bufio.NewReader(strings.NewReader("yes\nno\n"))
	if err := a.resolve(context.Background(), s); err == nil || s.Stage != "resolved" {
		t.Fatalf("prepare resolved state: stage=%s err=%v", s.Stage, err)
	}
	before := snapshotTest(t, a.Repo)
	t.Setenv("WSUP_CANCEL_TEST_CHILD", "1")
	argv := []string{os.Args[0], "-test.run=^TestCancelCommandProcess$", "--", "parent"}
	if err := writeJSON(a.storage("config.json"), Config{Test: argv}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &cancelCommandOutput{ready: make(chan int, 1), cancel: cancel}
	a.Out = out
	a.In = bufio.NewReader(strings.NewReader("yes\nyes\n"))
	result := make(chan error, 1)
	go func() { result <- a.Execute(ctx, []string{"resume"}) }()
	select {
	case pid := <-out.ready:
		// Also clean up the descendant if process-group cancellation regresses.
		defer syscall.Kill(pid, syscall.SIGKILL)
	case err := <-result:
		t.Fatalf("command returned before configured test started: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("configured test never became ready")
	}
	if err := cancelAwait(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	appTestState(t, a, "merge", "test-failed")
	if engine.calls != 1 {
		t.Fatal("resume unexpectedly called model")
	}
	if head := testGit(t, a.Repo.Root, "rev-parse", "HEAD"); head != s.TargetHEAD {
		t.Fatalf("cancellation created merge commit: %s", head)
	}
	if after := snapshotTest(t, a.Repo); after != before {
		t.Fatal("canceled tests changed resolved worktree/index")
	}
	if head := testGit(t, a.Repo.Root, "rev-parse", "MERGE_HEAD"); head != s.SourceHEAD {
		t.Fatal("cancellation lost pending merge")
	}
}
