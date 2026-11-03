package wsup

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// ErrCanceled indicates an explicit Ctrl+C or Escape at a prompt.
var ErrCanceled = errors.New("operation canceled")

const promptInputLimit = 64 * 1024

var (
	errPromptTooLong = errors.New("input must not exceed 64 KiB")
	errPromptBusy    = errors.New("the previous input read has not finished")
	errPromptTimeout = errors.New("prompt input timeout")
	// A canceled pipe read cannot be interrupted without owning/closing its
	// source. Keep its reader reserved until that read exits, so a subsequent
	// prompt never starts a second reader on the same bufio.Reader.
	promptReaders sync.Map
)

type promptResult struct {
	text string
	err  error
}

// readPrompt reads a single answer after the caller has printed its prompt.
// Only the terminal path echoes input. The caller retains ownership of in.
func readPrompt(ctx context.Context, in *bufio.Reader, out io.Writer, terminal *os.File) (text string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if terminal == nil {
		if _, loaded := promptReaders.LoadOrStore(in, struct{}{}); loaded {
			return "", errPromptBusy
		}
		result := make(chan promptResult, 1)
		go func() {
			text, err := readPromptInput(ctx, func(_ bool) (byte, error) {
				return in.ReadByte()
			}, func() bool { return in.Buffered() > 0 }, nil)
			promptReaders.Delete(in)
			result <- promptResult{text, err}
		}()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case r := <-result:
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return r.text, r.err
		}
	}

	fd := int(terminal.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer func() {
		// Raw mode disables OPOST, so emit CRLF explicitly, before restoring.
		_, newlineErr := io.WriteString(out, "\r\n")
		restoreErr := term.Restore(fd, state)
		err = errors.Join(err, newlineErr, restoreErr)
	}()
	read := func(escape bool) (byte, error) {
		deadline := time.Now().Add(40 * time.Millisecond)
		for {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			timeout := 50
			if escape {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return 0, errPromptTimeout
				}
				timeout = int((remaining + time.Millisecond - 1) / time.Millisecond)
			}
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, err := unix.Poll(fds, timeout)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return 0, err
			}
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if n == 0 {
				continue
			}
			if fds[0].Revents&unix.POLLNVAL != 0 {
				return 0, unix.EBADF
			}
			var b [1]byte
			n, err = unix.Read(fd, b[:])
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			if err != nil {
				return 0, err
			}
			if n == 0 {
				return 0, io.EOF
			}
			return b[0], nil
		}
	}
	return readPromptInput(ctx, read, nil, out)
}

// read's bool requests a short Escape lookahead. For nonterminals, available
// restricts that lookahead to bytes already buffered, avoiding a blocking read
// after a standalone Escape. No parser state grows beyond the input limit.
func readPromptInput(ctx context.Context, read func(bool) (byte, error), available func() bool, echo io.Writer) (string, error) {
	var input, pending []byte
	write := func(s string) error {
		if echo == nil {
			return nil
		}
		_, err := io.WriteString(echo, s)
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		b, err := read(false)
		if err != nil {
			return "", err
		}
		switch b {
		case 3:
			return "", ErrCanceled
		case 27:
			pending = pending[:0]
			if available != nil && !available() {
				return "", ErrCanceled
			}
			next, err := read(true)
			if errors.Is(err, errPromptTimeout) || errors.Is(err, io.EOF) {
				return "", ErrCanceled
			}
			if err != nil {
				return "", err
			}
			if next == 3 || (next != '[' && next != 'O') {
				return "", ErrCanceled
			}
			// CSI and SS3 end at the first final byte (including arrows).
			for count := 0; ; count++ {
				if count >= promptInputLimit {
					return "", errPromptTooLong
				}
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if available != nil && !available() {
					break
				}
				c, err := read(true)
				if errors.Is(err, errPromptTimeout) || errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return "", err
				}
				if c == 3 || c == 27 {
					return "", ErrCanceled
				}
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
		case '\r', '\n':
			return strings.TrimSpace(string(input)), nil
		case 4:
			return "", io.EOF
		case 8, 127:
			pending = pending[:0]
			if len(input) > 0 {
				r, size := utf8.DecodeLastRune(input)
				input = input[:len(input)-size]
				if err := write(strings.Repeat("\b \b", cellWidth(string(r)))); err != nil {
					return "", err
				}
			}
		default:
			if b < 32 {
				if b == '\t' {
					if len(input) >= promptInputLimit {
						return "", errPromptTooLong
					}
					input = append(input, b)
					if err := write("\t"); err != nil {
						return "", err
					}
				}
				continue
			}
			pending = append(pending, b)
			for utf8.FullRune(pending) {
				r, size := utf8.DecodeRune(pending)
				if unicode.IsPrint(r) && !(r == utf8.RuneError && size == 1) {
					if len(input)+size > promptInputLimit {
						return "", errPromptTooLong
					}
					input = append(input, pending[:size]...)
					if err := write(string(pending[:size])); err != nil {
						return "", err
					}
				}
				pending = pending[size:]
			}
		}
	}
}
