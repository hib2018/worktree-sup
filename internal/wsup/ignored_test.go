package wsup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIgnoredFilesystemAliases(t *testing.T) {
	for _, pair := range [][2]string{{"Secret.env", "secret.env"}, {"caf\u00e9.env", "cafe\u0301.env"}} {
		t.Run(pair[0], func(t *testing.T) {
			a, _, _ := appTestFixture(t)
			entry := appTestCreate(t, a)
			writeTestFile(t, filepath.Join(entry.Path, pair[0]), "incoming\n")
			testGit(t, entry.Path, "add", "--", pair[0])
			testGit(t, entry.Path, "commit", "-m", "incoming alias")
			writeTestFile(t, filepath.Join(a.Repo.Root, ".gitignore"), "*.env\n")
			testGit(t, a.Repo.Root, "add", ".gitignore")
			testGit(t, a.Repo.Root, "commit", "-m", "diverge and ignore")
			original := filepath.Join(a.Repo.Root, pair[1])
			writeTestFile(t, original, "local secret\n")
			one, err := os.Lstat(original)
			if err != nil {
				t.Fatal(err)
			}
			two, err := os.Lstat(filepath.Join(a.Repo.Root, pair[0]))
			if err != nil || !os.SameFile(one, two) {
				t.Skip("filesystem does not alias these names")
			}
			err = appTestExecute(a, "1\n", "merge")
			if err == nil || !strings.Contains(err.Error(), "ignored") {
				t.Fatalf("expected ignored alias rejection, got %v", err)
			}
			data, err := os.ReadFile(original)
			if err != nil || string(data) != "local secret\n" {
				t.Fatalf("local file changed: %q %v", data, err)
			}
		})
	}
}
