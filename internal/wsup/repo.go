// Package wsup provides Git repository operations for the worktree manager.
package wsup

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

type Repo struct {
	Root, Common string
}

type Worktree struct {
	Path, Branch, HEAD string
	Bare, Detached     bool
}

// gitRaw preserves Git's output, including NUL delimiters and trailing newlines.
// Discard inherited Git environment overrides so they cannot redirect a command
// to a different repository, index, object store, or injected configuration.
func gitRaw(dir string, args ...string) ([]byte, error) {
	flags := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "core.fsmonitor=false"}
	cmd := exec.Command("git", append(flags, args...)...)
	cmd.Dir = dir
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (r *Repo) Git(dir string, args ...string) (string, error) {
	out, err := gitRaw(dir, args...)
	return strings.TrimSpace(string(out)), err
}

func Discover(dir string) (*Repo, error) {
	common, err := gitRaw(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	// Remove only Git's line terminator: paths may themselves contain whitespace.
	r := &Repo{Common: strings.TrimSuffix(string(common), "\n")}
	bare, err := r.Git(dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return nil, err
	}
	if bare == "true" {
		r.Root = r.Common
	} else {
		root, err := gitRaw(dir, "rev-parse", "--show-toplevel")
		if err != nil {
			return nil, err
		}
		r.Root = strings.TrimSuffix(string(root), "\n")
	}
	return r, nil
}

func (r *Repo) Worktrees() ([]Worktree, error) {
	out, err := gitRaw(r.Root, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var result []Worktree
	var current *Worktree
	for _, field := range bytes.Split(out, []byte{0}) {
		s := string(field)
		switch {
		case s == "":
			current = nil
		case strings.HasPrefix(s, "worktree "):
			result = append(result, Worktree{Path: strings.TrimPrefix(s, "worktree ")})
			current = &result[len(result)-1]
		case current == nil:
			return nil, fmt.Errorf("invalid git worktree record: %q", s)
		case strings.HasPrefix(s, "HEAD "):
			current.HEAD = strings.TrimPrefix(s, "HEAD ")
		case strings.HasPrefix(s, "branch "):
			current.Branch = strings.TrimPrefix(strings.TrimPrefix(s, "branch "), "refs/heads/")
		case s == "bare":
			current.Bare = true
		case s == "detached":
			current.Detached = true
		}
	}
	return result, nil
}

var operationPaths = []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG", "sequencer"}

func (r *Repo) gitPath(path, name string) (string, error) {
	out, err := gitRaw(path, "rev-parse", "--path-format=absolute", "--git-path", name)
	return strings.TrimSuffix(string(out), "\n"), err
}

func (r *Repo) Clean(path string) error {
	for _, name := range operationPaths {
		p, err := r.gitPath(path, name)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("ongoing Git operation: %s", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	out, err := gitRaw(path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if len(out) != 0 {
		return fmt.Errorf("worktree is dirty or has untracked files: %s", path)
	}
	return nil
}

func (r *Repo) Head(path string) (string, error) {
	return r.Git(path, "rev-parse", "--verify", "HEAD^{commit}")
}

func ValidBranch(name string) error {
	if name == "" || name == "HEAD" || name == "@" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid branch name: %q", name)
	}
	// --branch can expand @{-1}; accepting expansions would make validation
	// depend on checkout history rather than the supplied literal name.
	if strings.Contains(name, "@{") {
		return fmt.Errorf("invalid branch name: %q", name)
	}
	_, err := gitRaw("", "check-ref-format", "--branch", name)
	return err
}

func (r *Repo) BranchExists(branch string) bool {
	if ValidBranch(branch) != nil {
		return false
	}
	_, err := gitRaw(r.Root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// Snapshot fingerprints HEAD, the exact index, porcelain status, and the full
// contents of tracked and nonignored untracked files. Symlink targets are hashed
// as text, never followed. Unsupported entries (including populated submodules)
// fail closed. Two complete reads must agree; callers must still revalidate
// immediately before a destructive action and hold Lock while doing so.
func (r *Repo) Snapshot(path string) (string, error) {
	repo, err := Discover(path)
	if err != nil {
		return "", err
	}
	first, err := repo.snapshotOnce(repo.Root)
	if err != nil {
		return "", err
	}
	second, err := repo.snapshotOnce(repo.Root)
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("worktree changed while taking snapshot")
	}
	return first, nil
}

// Length framing prevents ambiguity between filenames, contents and fields.
func hashField(h hash.Hash, data []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	h.Write(size[:])
	h.Write(data)
}

func (r *Repo) snapshotOnce(path string) (string, error) {
	h := sha256.New()
	head, err := r.Head(path)
	if err != nil {
		// An unborn branch is a valid repository state, but other HEAD failures
		// must not be silently treated as an empty repository.
		ref, refErr := r.Git(path, "symbolic-ref", "HEAD")
		if refErr != nil {
			return "", err
		}
		_, existsErr := gitRaw(path, "show-ref", "--verify", "--quiet", ref)
		var exit *exec.ExitError
		if !errors.As(existsErr, &exit) || exit.ExitCode() != 1 {
			return "", err
		}
		head = "unborn:" + ref
	}
	hashField(h, []byte(head))
	index, err := r.gitPath(path, "index")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(index)
	if errors.Is(err, os.ErrNotExist) {
		hashField(h, []byte("missing-index"))
	} else if err != nil {
		return "", err
	} else {
		hashField(h, []byte("index"))
		hashField(h, data)
	}
	status, err := gitRaw(path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return "", err
	}
	hashField(h, status)
	files, err := gitRaw(path, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	names := strings.Split(string(files), "\x00")
	sort.Strings(names)
	previous := ""
	for _, name := range names {
		if name == "" || name == previous {
			continue
		}
		previous = name
		hashField(h, []byte(name))
		if err := hashWorkFile(h, path, name); err != nil {
			return "", fmt.Errorf("snapshot %q: %w", name, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Walk descriptor-relative paths with O_NOFOLLOW, keeping each directory open
// while opening its child. Unlike an Lstat/ReadFile pair this cannot race into a symlink
// installed in a parent directory. Missing tracked files are explicitly hashed.
func hashWorkFile(h hash.Hash, root, name string) error {
	if filepath.IsAbs(name) || filepath.Clean(name) != name || name == "." || strings.HasPrefix(name, "../") {
		return errors.New("unsafe Git path")
	}
	fd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { syscall.Close(fd) }()
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := openRelative(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC)
		if errors.Is(err, syscall.ENOENT) {
			hashField(h, []byte("missing"))
			return nil
		}
		if err != nil {
			return err
		}
		syscall.Close(fd)
		fd = next
	}
	leaf := parts[len(parts)-1]
	fileFD, err := openRelative(fd, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC)
	if errors.Is(err, syscall.ENOENT) {
		hashField(h, []byte("missing"))
		return nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return hashSymlink(h, fd, leaf)
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fileFD), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("unsupported nonregular working-tree entry")
	}
	hashField(h, []byte(fmt.Sprintf("file:%o", info.Mode().Perm())))
	// Stream full content into a separate digest to avoid loading large files.
	content := sha256.New()
	if _, err := io.Copy(content, file); err != nil {
		return err
	}
	hashField(h, content.Sum(nil))
	return nil
}

// Go 1.23's frozen syscall package lacks Darwin openat/readlinkat wrappers.
// Keep the ABI numbers here so safe traversal needs no third-party dependency.
func relativeSyscalls() (openat, readlinkat uintptr, err error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/amd64", "darwin/arm64":
		return 463, 473, nil
	case "linux/amd64":
		return 257, 267, nil
	case "linux/arm64":
		return 56, 78, nil
	default:
		return 0, 0, errors.New("safe snapshot traversal is unsupported on this platform")
	}
}

func openRelative(fd int, name string, flags int) (int, error) {
	trap, _, err := relativeSyscalls()
	if err != nil {
		return -1, err
	}
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return -1, err
	}
	n, _, errno := syscall.Syscall6(trap, uintptr(fd), uintptr(unsafe.Pointer(p)), uintptr(flags), 0, 0, 0)
	runtime.KeepAlive(p)
	if errno != 0 {
		return -1, errno
	}
	return int(n), nil
}

func hashSymlink(h hash.Hash, dirFD int, name string) error {
	_, trap, err := relativeSyscalls()
	if err != nil {
		return err
	}
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	for size := 256; size <= 1<<20; size *= 2 {
		buf := make([]byte, size)
		n, _, errno := syscall.Syscall6(trap, uintptr(dirFD), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
		runtime.KeepAlive(p)
		if errno != 0 {
			return errno
		}
		if n < uintptr(len(buf)) {
			hashField(h, []byte("symlink"))
			hashField(h, buf[:n])
			return nil
		}
	}
	return errors.New("symlink target is too long")
}

// Lock acquires a nonblocking advisory lock shared by all worktrees. The lock
// file is never unlinked: replacing its inode would allow concurrent owners.
func (r *Repo) Lock() (func(), error) {
	fd, err := syscall.Open(filepath.Join(r.Common, "wsup.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open worktree lock: %w", err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("acquire worktree lock: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			syscall.Flock(fd, syscall.LOCK_UN)
			syscall.Close(fd)
		})
	}, nil
}
