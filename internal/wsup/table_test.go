package wsup

import (
	"bufio"
	"bytes"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWriteTable(t *testing.T) {
	var out bytes.Buffer
	err := writeTable(&out, [][]string{{"BRANCH", "SUMMARY", "PATH"}, {"main", "要約", "."}, {"topic", "e\u0301", "../topic"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "BRANCH  SUMMARY  PATH\n" + strings.Repeat("─", 25) + "\n" +
		"main    要約     .\n" +
		"topic   e\u0301        ../topic\n"
	if out.String() != want {
		t.Fatalf("table:\n%s\nwant:\n%s", out.String(), want)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("trailing whitespace: %q", line)
		}
	}
	if strings.Count(out.String(), "─") != 25 {
		t.Fatal("expected only one header separator")
	}
}

func TestTableBoldHeadersOnly(t *testing.T) {
	var out bytes.Buffer
	if err := writeTableStyled(&out, [][]string{{"BRANCH", "SUMMARY"}, {"main", "要約"}}, true, 0); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[1mBRANCH\x1b[22m  \x1b[1mSUMMARY\x1b[22m\n" + strings.Repeat("─", 15) + "\nmain    要約\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	var plain bytes.Buffer
	if err := writeTable(&plain, [][]string{{"BRANCH"}, {"main"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "\x1b") {
		t.Fatal("non-terminal output contains ANSI escapes")
	}
}

func TestClipCell(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		want  string
	}{
		{"abcdef", 0, ""}, {"abcdef", 1, "…"}, {"abcdef", 4, "abc…"},
		{"日本語", 3, "日…"}, {"日本語", 2, "…"}, {"日本語", 6, "日本語"},
		{"e\u0301abcdef", 2, "e\u0301…"}, {"e\u0301", 1, "e\u0301"}, {"🙂abc", 3, "🙂…"},
	} {
		if got := clipCell(tc.text, tc.width); got != tc.want {
			t.Errorf("clipCell(%q, %d) = %q, want %q", tc.text, tc.width, got, tc.want)
		}
	}
}

func TestTableFitsScreen(t *testing.T) {
	rows := [][]string{
		{"BRANCH", "SUMMARY", "PATH", "STATE"},
		{"feature/" + strings.Repeat("long-", 12), strings.Repeat("日本語", 40), "../" + strings.Repeat("directory/", 12), "dirty / operation in progress / unreadable"},
		{"main", "e\u0301", ".", "clean"},
	}
	for _, width := range []int{1, 2, 3, 4, 6, 10, 20, 28, 40, 60, 80, 120, 200} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			for _, bold := range []bool{false, true} {
				var out bytes.Buffer
				if err := writeTableStyled(&out, rows, bold, width); err != nil {
					t.Fatal(err)
				}
				plain := strings.NewReplacer("\x1b[1m", "", "\x1b[22m", "").Replace(out.String())
				lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
				if len(lines) != len(rows)+1 {
					t.Fatalf("unexpected rows: %q", plain)
				}
				for _, line := range lines {
					if cells := cellWidth(line); cells > width {
						t.Errorf("line has %d cells, screen has %d: %q", cells, width, line)
					}
					if strings.TrimRight(line, " ") != line {
						t.Errorf("trailing spaces: %q", line)
					}
				}
				if width >= 28 {
					for _, header := range rows[0] {
						if !strings.Contains(lines[0], header) {
							t.Errorf("header lost: %q", lines[0])
						}
					}
				}
				if !strings.Contains(plain, "…") {
					t.Fatal("missing truncation marker")
				}
				if strings.ContainsAny(plain, "│┌┬┐├┼┤└┴┘") {
					t.Fatal("unexpected borders")
				}
			}
		})
	}
	var full bytes.Buffer
	if err := writeTable(&full, rows); err != nil {
		t.Fatal(err)
	}
	for _, value := range rows[1] {
		if !strings.Contains(full.String(), value) {
			t.Errorf("nonterminal output lost %q", value)
		}
	}
}

func TestTableEscapesControls(t *testing.T) {
	var out bytes.Buffer
	if err := writeTable(&out, [][]string{{"PATH"}, {"foo\n\t\x1b[31m\rbar\u202e"}}); err != nil {
		t.Fatal(err)
	}
	for _, control := range []string{"\x1b", "\r", "\t", "\u202e"} {
		if strings.Contains(out.String(), control) {
			t.Errorf("unescaped control %q", control)
		}
	}
	if !strings.Contains(out.String(), `foo\n\t\x1b[31m\rbar\u202e`) {
		t.Fatal(out.String())
	}
}

type tableErrorWriter struct{}

func (tableErrorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func TestTableWriterError(t *testing.T) {
	if err := writeTable(tableErrorWriter{}, [][]string{{"header"}}); err == nil {
		t.Fatal("lost writer error")
	}
}

func TestListPathsRelativeToPrimaryWorktree(t *testing.T) {
	a, _, out := appTestFixture(t)
	e := appTestCreate(t, a)
	external := filepath.Join(t.TempDir(), "external")
	testGit(t, a.Repo.Root, "worktree", "add", "-b", "outside", external)
	detached := filepath.Join(t.TempDir(), "detached")
	testGit(t, a.Repo.Root, "worktree", "add", "--detach", detached)
	linkedRepo, err := Discover(e.Path)
	if err != nil {
		t.Fatal(err)
	}
	for name, repo := range map[string]*Repo{"primary": a.Repo, "linked": linkedRepo} {
		t.Run(name, func(t *testing.T) {
			out.Reset()
			listed := &App{Repo: repo, Out: out, In: bufio.NewReader(strings.NewReader(""))}
			if err := listed.list(); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			for _, path := range []string{a.Repo.Root, e.Path, external, detached} {
				path, err := filepath.EvalSymlinks(path)
				if err != nil {
					t.Fatal(err)
				}
				relative, err := filepath.Rel(a.Repo.Root, path)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(text, "  "+filepath.ToSlash(relative)) {
					t.Errorf("missing relative path %q:\n%s", relative, text)
				}
				if strings.Contains(text, path) {
					t.Errorf("absolute path leaked: %s", path)
				}
			}
			if strings.ContainsAny(text, "│┌┬┐├┼┤└┴┘") {
				t.Fatal("unexpected table borders")
			}
			for _, want := range []string{"─", "unregistered", "(detached)"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q", want)
				}
			}
		})
	}
}

func TestListBareRepositoryRelativePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bare.git")
	testGit(t, "", "init", "--bare", root)
	repo, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &App{Repo: repo, Out: &out}
	if err := a.list(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), root) || !strings.Contains(out.String(), "  . ") || !strings.Contains(out.String(), "bare") {
		t.Fatal(out.String())
	}
}
