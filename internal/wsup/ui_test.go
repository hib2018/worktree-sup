package wsup

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode"
)

func assertEnglishUI(t *testing.T, text string) {
	t.Helper()
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			t.Fatalf("Japanese UI text remains: %q", text)
		}
	}
}

func TestEnglishUI(t *testing.T) {
	var help bytes.Buffer
	if err := Run(context.Background(), []string{"--help"}, strings.NewReader(""), &help); err != nil {
		t.Fatal(err)
	}
	assertEnglishUI(t, help.String())
	for _, tc := range []struct {
		name, input string
		args        []string
	}{
		{"list", "", []string{"list"}},
		{"status", "", []string{"status"}},
		{"task-prompt", "\x1b", []string{"new"}},
		{"base-prompt", "task\n\x1b", []string{"new"}},
		{"merge-empty", "", []string{"merge"}},
		{"remove-empty", "", []string{"remove"}},
		{"config", "", []string{"config", "test", "--clear"}},
		{"unknown", "", []string{"unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, out := appTestFixture(t)
			err := appTestExecute(a, tc.input, tc.args...)
			assertEnglishUI(t, out.String())
			if err != nil {
				assertEnglishUI(t, err.Error())
			}
			if strings.Contains(out.String(), "Ctrl+C") || strings.Contains(out.String(), "Esc") {
				t.Fatal("unexpected cancellation hint")
			}
			if strings.Contains(tc.name, "prompt") && !errors.Is(err, ErrCanceled) {
				t.Fatalf("cancellation changed: %v", err)
			}
		})
	}
}

func TestEnglishUIPreservesJapaneseSummary(t *testing.T) {
	a, _, out := appTestFixture(t)
	entry := appTestCreate(t, a)
	if !strings.Contains(out.String(), entry.Summary) {
		t.Fatal("creation changed summary")
	}
	assertEnglishUI(t, strings.ReplaceAll(out.String(), entry.Summary, ""))
	out.Reset()
	appTestOK(t, a, "", "list")
	if !strings.Contains(out.String(), entry.Summary) {
		t.Fatal("listing changed summary")
	}
	assertEnglishUI(t, strings.ReplaceAll(out.String(), entry.Summary, ""))
}
