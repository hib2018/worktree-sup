package wsup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestPromptWithoutCancellationHint(t *testing.T) {
	for _, input := range []string{"task\n", "\x03", "\x1b"} {
		var out bytes.Buffer
		a := &App{In: bufio.NewReader(strings.NewReader(input)), Out: &out}
		_, err := a.ask("Task")
		if input == "task\n" && err != nil {
			t.Fatal(err)
		}
		if input != "task\n" && !errors.Is(err, ErrCanceled) {
			t.Fatalf("expected cancellation: %v", err)
		}
		if out.String() != "Task: " {
			t.Fatalf("unexpected prompt: %q", out.String())
		}
	}
}

func TestReadPrompt(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
		err               error
	}{
		{name: "trim", input: " \t 日本語 answer \t \n", want: "日本語 answer"},
		{name: "empty line", input: "\n"},
		{name: "carriage return", input: "yes\r", want: "yes"},
		{name: "partial EOF", input: " yes ", err: io.EOF},
		{name: "approval without newline", input: "yes", err: io.EOF},
		{name: "EOF", err: io.EOF},
		{name: "CtrlD", input: "\x04", err: io.EOF},
		{name: "CtrlD after text", input: "yes\x04", err: io.EOF},
		{name: "CtrlD before newline", input: "yes\x04\n", err: io.EOF},
		{name: "CtrlC no newline", input: "\x03", err: ErrCanceled},
		{name: "CtrlC after text", input: "yes\x03", err: ErrCanceled},
		{name: "Escape no newline", input: "\x1b", err: ErrCanceled},
		{name: "Escape after text", input: "yes\x1b", err: ErrCanceled},
		{name: "CSI arrows", input: "a\x1b[A\x1b[B\x1b[C\x1b[Db\n", want: "ab"},
		{name: "modified arrow", input: "a\x1b[1;5Db\n", want: "ab"},
		{name: "SS3 arrows", input: "a\x1bOA\x1bOB\x1bOC\x1bODb\n", want: "ab"},
		{name: "CtrlC inside CSI", input: "\x1b[\x03", err: ErrCanceled},
		{name: "backspace", input: "abc\b\x7fd\n", want: "ad"},
		{name: "rune backspace", input: "a日本\x7f語\n", want: "a日語"},
		{name: "empty backspace", input: "\b\x7fok\n", want: "ok"},
		{name: "invalid UTF8", input: "a\xffb\n", want: "ab"},
		{name: "limit", input: strings.Repeat("a", promptInputLimit) + "\n", want: strings.Repeat("a", promptInputLimit)},
		{name: "over limit", input: strings.Repeat("a", promptInputLimit+1), err: errPromptTooLong},
		{name: "multibyte over limit", input: strings.Repeat("a", promptInputLimit-1) + "日", err: errPromptTooLong},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := readPrompt(context.Background(), bufio.NewReader(strings.NewReader(tt.input)), &out, nil)
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, err, tt.want, tt.err)
			}
			if out.Len() != 0 {
				t.Fatalf("nonterminal echoed %q", out.String())
			}
		})
	}
}

func TestReadPromptLeavesNextLine(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("one\ntwo\n"))
	for _, want := range []string{"one", "two"} {
		got, err := readPrompt(context.Background(), in, io.Discard, nil)
		if err != nil || got != want {
			t.Fatalf("got (%q, %v), want %q", got, err, want)
		}
	}
}

type promptStartedReader struct {
	io.Reader
	started chan struct{}
}

func (r *promptStartedReader) Read(p []byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	return r.Reader.Read(p)
}

func TestReadPromptPipeContextCancellation(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	started := make(chan struct{})
	in := bufio.NewReader(&promptStartedReader{Reader: pr, started: started})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := readPrompt(ctx, in, io.Discard, nil)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for pipe input")
	}
	// The canceled worker is still blocked. Do not launch a second reader.
	if _, err := readPrompt(context.Background(), in, io.Discard, nil); !errors.Is(err, errPromptBusy) {
		t.Fatalf("got %v, want busy reader", err)
	}
	pw.Close()
	deadline := time.Now().Add(time.Second)
	for {
		if _, active := promptReaders.Load(in); !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader did not exit after pipe closed")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestReadPromptPipeControlsWithoutNewline(t *testing.T) {
	for _, control := range []string{"\x03", "\x1b"} {
		t.Run(control, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pr.Close()
			defer pw.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			written := make(chan error, 1)
			go func() {
				_, err := io.WriteString(pw, control)
				written <- err
			}()
			_, err := readPrompt(ctx, bufio.NewReader(pr), io.Discard, nil)
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("got %v, want ErrCanceled", err)
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadPromptAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := bufio.NewReader(strings.NewReader("answer\n"))
	if _, err := readPrompt(ctx, in, io.Discard, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if got, err := in.ReadString('\n'); err != nil || got != "answer\n" {
		t.Fatalf("canceled prompt consumed input: %q, %v", got, err)
	}
}

func TestReadPromptInputEcho(t *testing.T) {
	for _, tt := range []struct {
		name, input, want, echo string
	}{
		{name: "ASCII", input: "ab\x7fc\n", want: "ac", echo: "ab\b \bc"},
		{name: "wide Japanese", input: "a日\x7fb\x1b[A\n", want: "ab", echo: "a日\b \b\b \bb"},
		{name: "combining zero width", input: "e\u0301\x7f\n", want: "e", echo: "e\u0301"},
		{name: "combining then base", input: "e\u0301\x7f\x7f\n", want: "", echo: "e\u0301\b \b"},
		{name: "wide and combining", input: "日\u0301\b\b\n", want: "", echo: "日\u0301\b \b\b \b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := bufio.NewReader(strings.NewReader(tt.input))
			var out bytes.Buffer
			got, err := readPromptInput(context.Background(), func(bool) (byte, error) { return in.ReadByte() }, nil, &out)
			if err != nil || got != tt.want || out.String() != tt.echo {
				t.Fatalf("answer=%q, err=%v, echo=%q; want answer=%q, echo=%q", got, err, out.String(), tt.want, tt.echo)
			}
		})
	}
}

func TestReadPromptInputEscapeTimeout(t *testing.T) {
	calls := 0
	_, err := readPromptInput(context.Background(), func(escape bool) (byte, error) {
		calls++
		if calls == 1 {
			return 27, nil
		}
		if !escape {
			t.Fatal("Escape did not request timed lookahead")
		}
		return 0, errPromptTimeout
	}, nil, io.Discard)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want ErrCanceled", err)
	}
}

func TestReadPromptInputEchoError(t *testing.T) {
	want := errors.New("write failed")
	in := bufio.NewReader(strings.NewReader("x\n"))
	_, err := readPromptInput(context.Background(), func(bool) (byte, error) { return in.ReadByte() }, nil, promptFailWriter{want})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

type promptFailWriter struct{ err error }

func (w promptFailWriter) Write([]byte) (int, error) { return 0, w.err }
