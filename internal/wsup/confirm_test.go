package wsup

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestConfirm(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		want        bool
		err         error
	}{
		{name: "approve", input: "y\n", want: true},
		{name: "default decline", input: "N\n"},
		{name: "no", input: "no\n"},
		{name: "lowercase decline", input: "n\n"},
		{name: "uppercase y", input: "Y\n"},
		{name: "yes", input: "yes\n"},
		{name: "uppercase yes", input: "YES\n"},
		{name: "empty line", input: "\n"},
		{name: "EOF", err: io.EOF},
		{name: "approval without newline", input: "y", err: io.EOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			a := &App{In: bufio.NewReader(strings.NewReader(tt.input)), Out: &out}
			got, err := a.confirm("Continue?")
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Fatalf("got (%v, %v), want (%v, %v)", got, err, tt.want, tt.err)
			}
			if out.String() != "Continue? [y/N]: " {
				t.Fatalf("unexpected prompt: %q", out.String())
			}
		})
	}
}
