package wsup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBatchRemoveSelection(t *testing.T) {
	for _, selection := range []string{"all", "2 1", "1", "1 1", "1 3", "", "all 1", "1 bad"} {
		t.Run(selection, func(t *testing.T) {
			a, _, out := appTestFixture(t)
			dir := t.TempDir()
			entries := []Entry{
				batchMergeEntry(t, a, "first", filepath.Join(dir, "a-first"), "main"),
				batchMergeEntry(t, a, "second", filepath.Join(dir, "b-second"), "main"),
			}
			err := appTestExecute(a, selection+"\ny\ny\n", "remove")
			valid := selection == "all" || selection == "2 1" || selection == "1"
			if valid && err != nil {
				t.Fatal(err)
			}
			if !valid && (err == nil || !strings.Contains(err.Error(), "invalid selection")) {
				t.Fatalf("want invalid selection, got %v", err)
			}
			m, err := a.loadMetadata()
			if err != nil {
				t.Fatal(err)
			}
			for i, e := range entries {
				removed := valid && (selection != "1" || i == 0)
				_, statErr := os.Stat(e.Path)
				if removed != os.IsNotExist(statErr) || a.Repo.BranchExists(e.Branch) == removed {
					t.Fatalf("%s: removed=%v, stat=%v", e.Branch, removed, statErr)
				}
				if _, exists := m.Entries[e.Branch]; exists == removed {
					t.Fatalf("%s: incorrect metadata", e.Branch)
				}
			}
			if selection == "2 1" && strings.Index(out.String(), "Branch: second") > strings.Index(out.String(), "Branch: first") {
				t.Fatal("removal order changed")
			}
		})
	}
}

func TestBatchRemoveStops(t *testing.T) {
	for _, reason := range []string{"declined", "missing approval", "dirty", "ignored", "unmerged", "canceled"} {
		t.Run(reason, func(t *testing.T) {
			a, _, out := appTestFixture(t)
			dir := t.TempDir()
			first := batchMergeEntry(t, a, "first", filepath.Join(dir, "a-first"), "main")
			second := batchMergeEntry(t, a, "second", filepath.Join(dir, "b-second"), "main")
			third := batchMergeEntry(t, a, "third", filepath.Join(dir, "c-third"), "main")
			input := "all\ny\ny\ny\n"
			wantError := ""
			switch reason {
			case "declined":
				input = "all\ny\nN\n"
				wantError = "removal canceled"
			case "missing approval":
				input = "all\ny\n"
			case "canceled":
				input = "all\ny\n\x1b"
			case "dirty":
				writeTestFile(t, filepath.Join(second.Path, "untracked"), "keep\n")
				wantError = "dirty"
			case "ignored":
				writeTestFile(t, filepath.Join(second.Path, "ignored"), "keep\n")
				writeTestFile(t, filepath.Join(a.Repo.Common, "info", "exclude"), "ignored\n")
				wantError = "ignored files remain"
			case "unmerged":
				appTestCommit(t, second.Path)
				wantError = "cannot confirm the branch is merged"
			}
			err := appTestExecute(a, input, "remove")
			if err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("want stop %q, got %v", wantError, err)
			}
			if a.Repo.BranchExists(first.Branch) {
				t.Fatal("completed removal lost")
			}
			for _, e := range []Entry{second, third} {
				if !a.Repo.BranchExists(e.Branch) {
					t.Fatalf("%s removed after stop", e.Branch)
				}
				if _, err := os.Stat(e.Path); err != nil {
					t.Fatal(err)
				}
			}
			if !strings.Contains(out.String(), "Batch stopped.") || strings.Contains(out.String(), "Branch: third") {
				t.Fatal(out.String())
			}
		})
	}
}
