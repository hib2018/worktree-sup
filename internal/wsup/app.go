package wsup

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"
)

const help = `worktree-sup (wsup) — Git worktree management CLI powered by fx

  wsup new [--base BRANCH] [TASK]
  wsup issue [--base BRANCH]      Create worktrees for all open GitHub issues (requires gh)
  wsup list
  wsup merge [--target BRANCH]    Select from the list
  wsup remove                    Select from the list
  wsup config test -- COMMAND [ARGS...]
  wsup config test --clear
  wsup status
  wsup resume
  wsup abort

Conflict resolution changes are applied only after approval. No push, rebase, squash, or forced deletion.
fx integration requires native fx + sandbox-exec on macOS (stops safely on other platforms).
`

type App struct {
	Repo          *Repo
	Engine        Engine
	In            *bufio.Reader
	Out           io.Writer
	ctx           context.Context
	inputTerminal *os.File
}

func Run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, help)
		return err
	}
	r, err := Discover(".")
	if err != nil {
		return err
	}
	a := &App{Repo: r, In: bufio.NewReader(in), Out: out}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		a.inputTerminal = f
	}
	trees, err := r.Worktrees()
	if err != nil {
		return err
	}
	denied := []string{r.Common}
	for _, t := range trees {
		denied = append(denied, t.Path)
	}
	a.Engine = FX{DenyPaths: denied}
	return a.Execute(ctx, args)
}
func (a *App) Execute(ctx context.Context, args []string) error {
	a.ctx = ctx
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("a command is required")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("list takes no arguments")
		}
		return a.list()
	case "status":
		if len(args) != 1 {
			return errors.New("status takes no arguments")
		}
		s, err := a.loadState()
		if err != nil {
			return err
		}
		if s == nil {
			fmt.Fprintln(a.Out, "No saved operation")
			return nil
		}
		fmt.Fprintf(a.Out, "%s / %s\nsource: %s\ntarget: %s\npath: %s\n", s.Kind, s.Stage, s.Source, s.Target, s.TargetPath)
		return nil
	}
	switch args[0] {
	case "new", "issue", "merge", "remove", "config", "resume", "abort":
	default:
		return fmt.Errorf("unknown command: %s", args[0])
	}
	unlock, err := a.Repo.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if args[0] != "resume" && args[0] != "abort" && args[0] != "config" {
		if err = a.idle(); err != nil {
			return err
		}
	}
	switch args[0] {
	case "new":
		return a.create(ctx, args[1:])
	case "issue":
		return a.createIssues(ctx, args[1:])
	case "merge":
		return a.merge(ctx, args[1:])
	case "remove":
		if len(args) != 1 {
			return errors.New("remove takes no arguments; select from the list")
		}
		return a.remove()
	case "config":
		return a.config(args[1:])
	case "resume", "abort":
		if len(args) != 1 {
			return errors.New("too many arguments")
		}
		return a.recover(ctx, args[0] == "abort")
	}
	return nil
}
func (a *App) ask(prompt string) (string, error) {
	ctx := a.operationContext()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := fmt.Fprint(a.Out, prompt+": "); err != nil {
		return "", err
	}
	line, err := readPrompt(ctx, a.In, a.Out, a.inputTerminal)
	if err != nil {
		return "", fmt.Errorf("could not complete input: %w", err)
	}
	return line, nil
}

func (a *App) operationContext() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}
func (a *App) confirm(prompt string) (bool, error) {
	s, err := a.ask(prompt + " [yes/no]")
	return s == "yes", err
}
func (a *App) flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(a.Out)
	return f
}
func (a *App) config(args []string) error {
	if len(args) < 2 || args[0] != "test" {
		return errors.New("wsup config test -- COMMAND [ARGS...] or --clear")
	}
	c := Config{}
	if len(args) == 2 && args[1] == "--clear" {
	} else if args[1] == "--" && len(args) > 2 {
		c.Test = args[2:]
	} else {
		return errors.New("specify the test command after --")
	}
	if err := writeJSON(a.storage("config.json"), c); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "Test configuration saved")
	return nil
}
func (a *App) list() error {
	trees, err := a.Repo.Worktrees()
	if err != nil {
		return err
	}
	m, err := a.loadMetadata()
	if err != nil {
		return err
	}
	// Git lists the primary worktree first, even from a linked worktree.
	root := a.Repo.Root
	if len(trees) > 0 {
		root = trees[0].Path
	}
	rows := [][]string{{"BRANCH", "SUMMARY", "PATH", "STATE"}}
	for _, t := range trees {
		summary := "unregistered"
		if e, ok := m.Entries[t.Branch]; ok && e.Path == t.Path {
			summary = e.Summary
		}
		state := "clean"
		if t.Bare {
			state = "bare"
		} else if err := a.Repo.Clean(t.Path); err != nil {
			state = "dirty / operation in progress / unreadable"
		}
		branch := t.Branch
		if t.Detached {
			branch = "(detached)"
		}
		path, err := filepath.Rel(root, t.Path)
		if err != nil {
			return fmt.Errorf("relative worktree path: %w", err)
		}
		rows = append(rows, []string{branch, summary, filepath.ToSlash(path), state})
	}
	return writeTable(a.Out, rows)
}
func (a *App) selectEntry() (Entry, error) {
	trees, err := a.Repo.Worktrees()
	if err != nil {
		return Entry{}, err
	}
	m, err := a.loadMetadata()
	if err != nil {
		return Entry{}, err
	}
	var candidates []Entry
	for _, t := range trees {
		if e, ok := m.Entries[t.Branch]; ok && e.Path == t.Path && !t.Bare && !t.Detached {
			candidates = append(candidates, e)
			fmt.Fprintf(a.Out, "%d) %s — %s (%s)\n", len(candidates), e.Branch, e.Summary, e.Path)
		}
	}
	if len(candidates) == 0 {
		return Entry{}, errors.New("no managed worktrees; create one with wsup new")
	}
	choice, err := a.ask("Selection number")
	if err != nil {
		return Entry{}, err
	}
	n, err := strconv.Atoi(choice)
	if err != nil || n < 1 || n > len(candidates) {
		return Entry{}, errors.New("invalid selection number")
	}
	e := candidates[n-1]
	return e, a.verifyEntry(e)
}
func (a *App) verifyEntry(e Entry) error {
	if err := ValidBranch(e.Branch); err != nil {
		return err
	}
	if err := ValidBranch(e.Target); err != nil {
		return err
	}
	trees, err := a.Repo.Worktrees()
	if err != nil {
		return err
	}
	for _, t := range trees {
		if t.Path == e.Path && t.Branch == e.Branch && !t.Detached && !t.Bare {
			r, err := Discover(t.Path)
			if err != nil {
				return err
			}
			if r.Common != a.Repo.Common {
				return errors.New("repository mismatch")
			}
			return nil
		}
	}
	return errors.New("worktree path or branch does not match the record")
}
func (a *App) branchHEAD(branch string) (string, error) {
	if err := ValidBranch(branch); err != nil {
		return "", err
	}
	return a.Repo.Git(a.Repo.Root, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
}
func (a *App) targetPath(branch string) (string, error) {
	if err := ValidBranch(branch); err != nil {
		return "", err
	}
	trees, err := a.Repo.Worktrees()
	if err != nil {
		return "", err
	}
	for _, t := range trees {
		if t.Branch == branch && !t.Bare && !t.Detached {
			return t.Path, nil
		}
	}
	return "", fmt.Errorf("check out target %s in another worktree (no automatic checkout)", branch)
}
func (a *App) create(ctx context.Context, args []string) error {
	f := a.flags("new")
	base := f.String("base", "", "Base branch")
	if err := f.Parse(args); err != nil {
		return err
	}
	intent := strings.Join(f.Args(), " ")
	var err error
	if intent == "" {
		intent, err = a.ask("Task description")
		if err != nil {
			return err
		}
	}
	if intent == "" {
		return errors.New("a task description is required")
	}
	if *base == "" {
		*base, err = a.ask("Base branch")
		if err != nil {
			return err
		}
	}
	before, err := a.branchHEAD(*base)
	if err != nil {
		return err
	}
	if a.Engine == nil {
		return errors.New("fx engine is unavailable")
	}
	n, err := a.Engine.Name(ctx, intent, "Base: "+*base+". Keep summary concise in Japanese; branch short.")
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = ValidBranch(n.Branch); err != nil {
		return err
	}
	if strings.TrimSpace(n.Summary) == "" || strings.ContainsAny(n.Summary, "\x00\r\n\t") {
		return errors.New("invalid task summary")
	}
	return a.createEntry(Entry{Branch: n.Branch, Summary: n.Summary, Base: *base, Target: *base}, before)
}

func (a *App) createEntry(e Entry, before string) error {
	if err := a.operationContext().Err(); err != nil {
		return err
	}
	if err := ValidBranch(e.Branch); err != nil {
		return err
	}
	if a.Repo.BranchExists(e.Branch) {
		return errors.New("generated branch name already exists")
	}
	trees, err := a.Repo.Worktrees()
	if err != nil {
		return err
	}
	if len(trees) == 0 {
		return errors.New("no worktrees")
	}
	parent := trees[0].Path + ".worktrees"
	if err = os.Mkdir(parent, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	if real != parent {
		return errors.New("destination must not contain symlinks")
	}
	path := filepath.Join(parent, strings.ReplaceAll(e.Branch, "/", "-"))
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("destination already exists or cannot be verified")
	}
	now, err := a.branchHEAD(e.Base)
	if err != nil {
		return err
	}
	if now != before {
		return errors.New("base branch changed; run the command again")
	}
	e.Path = path
	s := &State{Kind: "new", Stage: "planned", Source: e.Branch, Target: e.Base, SourcePath: path, TargetHEAD: before, Entry: &e}
	fmt.Fprintf(a.Out, "Base: %s\nGenerated name: %s\nSummary: %s\nCreation path: %s\n", e.Base, e.Branch, e.Summary, path)
	if err = a.saveState(s); err != nil {
		return err
	}
	return a.continueCreate(s)
}
func (a *App) continueCreate(s *State) error {
	if err := a.operationContext().Err(); err != nil {
		return err
	}
	if s.Entry == nil {
		return errors.New("creation details are missing")
	}
	e := *s.Entry
	if err := ValidBranch(e.Branch); err != nil {
		return err
	}
	// Revalidate on resume as well: an empty parent may have been replaced.
	parent := filepath.Dir(e.Path)
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(e.Path) || real != parent || filepath.Base(e.Path) != strings.ReplaceAll(e.Branch, "/", "-") {
		return errors.New("destination changed or is unsafe")
	}
	// Only the original planned path is used; never reuse an existing directory.
	if s.Stage == "planned" {
		if _, err := os.Lstat(e.Path); errors.Is(err, os.ErrNotExist) {
			head, err := a.branchHEAD(e.Base)
			if err != nil {
				return err
			}
			if head != s.TargetHEAD {
				return errors.New("base changed")
			}
			if a.Repo.BranchExists(e.Branch) {
				return errors.New("branch already exists; no automatic recovery")
			}
			if err := a.operationContext().Err(); err != nil {
				return err
			}
			if _, err = a.Repo.Git(a.Repo.Root, "worktree", "add", "-b", e.Branch, "--", e.Path, s.TargetHEAD); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := a.verifyEntry(e); err != nil {
			return err
		}
		head, err := a.Repo.Head(e.Path)
		if err != nil {
			return err
		}
		if head != s.TargetHEAD {
			return errors.New("destination HEAD changed")
		}
		s.Stage = "worktree-created"
		if err = a.saveState(s); err != nil {
			return err
		}
	}
	if s.Stage != "worktree-created" {
		return errors.New("invalid creation stage")
	}
	if err := a.operationContext().Err(); err != nil {
		return err
	}
	if err := a.verifyEntry(e); err != nil {
		return err
	}
	m, err := a.loadMetadata()
	if err != nil {
		return err
	}
	m.Entries[e.Branch] = e
	if err = writeJSON(a.storage("metadata.json"), m); err != nil {
		return err
	}
	s.Stage = "completed"
	if err = a.saveState(s); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "Created:", e.Path)
	return nil
}
func (a *App) merge(ctx context.Context, args []string) error {
	f := a.flags("merge")
	target := f.String("target", "", "Target branch")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("select from the list")
	}
	e, err := a.selectEntry()
	if err != nil {
		return err
	}
	if *target == "" {
		*target = e.Target
	}
	if *target == e.Branch {
		return errors.New("cannot merge a branch into itself")
	}
	path, err := a.targetPath(*target)
	if err != nil {
		return err
	}
	for _, p := range []string{e.Path, path} {
		if err = a.Repo.Clean(p); err != nil {
			return err
		}
	}
	sourceHEAD, err := a.branchHEAD(e.Branch)
	if err != nil {
		return err
	}
	targetHEAD, err := a.branchHEAD(*target)
	if err != nil {
		return err
	}
	snap, err := a.Repo.Snapshot(path)
	if err != nil {
		return err
	}
	e.Target = *target
	m, err := a.loadMetadata()
	if err != nil {
		return err
	}
	m.Entries[e.Branch] = e
	if err = writeJSON(a.storage("metadata.json"), m); err != nil {
		return err
	}
	s := &State{Kind: "merge", Stage: "planned", Source: e.Branch, Target: *target, SourcePath: e.Path, TargetPath: path, SourceHEAD: sourceHEAD, TargetHEAD: targetHEAD, Snapshot: snap}
	fmt.Fprintf(a.Out, "Source: %s\nTarget: %s (%s)\nA normal Git merge may create conflict markers and other changes.\n", s.Source, s.Target, path)
	if err = a.saveState(s); err != nil {
		return err
	}
	return a.startMerge(ctx, s)
}
func (a *App) startMerge(ctx context.Context, s *State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.checkMergeStart(s); err != nil {
		return err
	}
	s.Stage = "merging"
	if err := a.saveState(s); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, mergeErr := a.Repo.Git(s.TargetPath, "merge", "--no-edit", "--no-verify", "--no-gpg-sign", "--no-overwrite-ignore", s.SourceHEAD)
	names, err := conflictNames(s.TargetPath)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if mergeErr != nil {
			return mergeErr
		}
		s.Stage = "completed"
		if err = a.saveState(s); err != nil {
			return err
		}
		fmt.Fprintln(a.Out, "Merge completed (no automatic deletion)")
		return nil
	}
	s.Stage = "conflicted"
	s.Snapshot, err = a.Repo.Snapshot(s.TargetPath)
	if err != nil {
		return err
	}
	if err = a.saveState(s); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Conflict state saved. To abort: git -C %q merge --abort or wsup abort\n", s.TargetPath)
	return a.resolve(ctx, s)
}
func (a *App) checkMergeStart(s *State) error {
	source, err := a.branchHEAD(s.Source)
	if err != nil {
		return err
	}
	target, err := a.branchHEAD(s.Target)
	if err != nil {
		return err
	}
	if source != s.SourceHEAD || target != s.TargetHEAD {
		return errors.New("branch HEAD changed")
	}
	path, err := a.targetPath(s.Target)
	if err != nil || path != s.TargetPath {
		return errors.New("target worktree changed")
	}
	if err = a.Repo.Clean(s.SourcePath); err != nil {
		return err
	}
	if err = a.Repo.Clean(s.TargetPath); err != nil {
		return err
	}
	if err := a.protectIgnored(s); err != nil {
		return err
	}
	snap, err := a.Repo.Snapshot(s.TargetPath)
	if err != nil {
		return err
	}
	if snap != s.Snapshot {
		return errors.New("target state changed")
	}
	return nil
}
func (a *App) remove() error {
	e, err := a.selectEntry()
	if err != nil {
		return err
	}
	head, err := a.branchHEAD(e.Branch)
	if err != nil {
		return err
	}
	s := &State{Kind: "remove", Stage: "planned", Source: e.Branch, Target: e.Target, SourcePath: e.Path, SourceHEAD: head, Entry: &e}
	if err = a.checkRemove(s); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Removal path: %s\nBranch: %s\nTarget: %s (already merged)\n", e.Path, e.Branch, e.Target)
	yes, err := a.confirm("Remove the worktree and branch?")
	if err != nil {
		return err
	}
	if !yes {
		return errors.New("removal canceled")
	}
	if err = a.saveState(s); err != nil {
		return err
	}
	return a.continueRemove(s)
}
func (a *App) checkRemove(s *State) error {
	if s.Source == s.Target {
		return errors.New("cannot remove the target branch itself")
	}
	head, err := a.branchHEAD(s.Source)
	if err != nil {
		return err
	}
	if head != s.SourceHEAD {
		return errors.New("removal source HEAD changed")
	}
	if _, err = a.branchHEAD(s.Target); err != nil {
		return err
	}
	if _, err = a.Repo.Git(a.Repo.Root, "merge-base", "--is-ancestor", s.SourceHEAD, "refs/heads/"+s.Target); err != nil {
		return errors.New("cannot confirm the branch is merged into the recorded target")
	}
	return nil
}
func (a *App) continueRemove(s *State) error {
	if err := a.operationContext().Err(); err != nil {
		return err
	}
	if s.Entry == nil {
		return errors.New("removal details are missing")
	}
	if s.Stage == "planned" {
		// Reconcile a crash after worktree removal but before saving its stage.
		if _, err := os.Lstat(s.SourcePath); errors.Is(err, os.ErrNotExist) {
			trees, err := a.Repo.Worktrees()
			if err != nil {
				return err
			}
			for _, tree := range trees {
				if tree.Path == s.SourcePath || tree.Branch == s.Source {
					return errors.New("removal source is still registered or in use by another worktree")
				}
			}
			if err := a.checkRemove(s); err != nil {
				return err
			}
			s.Stage = "worktree-removed"
			if err := a.saveState(s); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if s.Stage == "planned" {
		if err := a.verifyEntry(*s.Entry); err != nil {
			return err
		}
		if err := a.Repo.Clean(s.SourcePath); err != nil {
			return err
		}
		if err := a.checkRemove(s); err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(s.SourcePath, cwd)
		if err != nil {
			return err
		}
		if rel == "." || !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".." {
			return errors.New("cannot remove the worktree containing the current directory")
		}
		// Ignored files are also user data. Git's default remove can delete them.
		ignored, err := gitRaw(s.SourcePath, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
		if err != nil {
			return err
		}
		if len(ignored) > 0 {
			return errors.New("ignored files remain; stopping without deletion")
		}
		if err := a.operationContext().Err(); err != nil {
			return err
		}
		if _, err = a.Repo.Git(a.Repo.Root, "worktree", "remove", "--", s.SourcePath); err != nil {
			return err
		}
		s.Stage = "worktree-removed"
		if err = a.saveState(s); err != nil {
			return err
		}
	}
	if s.Stage != "worktree-removed" {
		return errors.New("invalid removal stage")
	}
	if err := a.operationContext().Err(); err != nil {
		return err
	}
	if a.Repo.BranchExists(s.Source) {
		if err := a.checkRemove(s); err != nil {
			return err
		}
		trees, err := a.Repo.Worktrees()
		if err != nil {
			return err
		}
		for _, tree := range trees {
			if tree.Branch == s.Source {
				return errors.New("branch to remove is in use by another worktree")
			}
		}
		if err := a.operationContext().Err(); err != nil {
			return err
		}
		// Delete only if the ref still points at the checked commit, never force-delete.
		if _, err := a.Repo.Git(a.Repo.Root, "update-ref", "-d", "refs/heads/"+s.Source, s.SourceHEAD); err != nil {
			return err
		}
	}
	m, err := a.loadMetadata()
	if err != nil {
		return err
	}
	delete(m.Entries, s.Source)
	if err = writeJSON(a.storage("metadata.json"), m); err != nil {
		return err
	}
	s.Stage = "completed"
	if err = a.saveState(s); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "Removed")
	return nil
}
func (a *App) recover(ctx context.Context, abort bool) error {
	s, err := a.loadState()
	if err != nil {
		return err
	}
	if s == nil || s.Stage == "completed" || s.Stage == "aborted" {
		return errors.New("no interrupted operation")
	}
	fmt.Fprintf(a.Out, "Interrupted operation: %s / %s\nBranches: %s -> %s\n", s.Kind, s.Stage, s.Source, s.Target)
	if abort {
		yes, err := a.confirm("Abort the saved operation? (Existing worktrees are kept)")
		if err != nil {
			return err
		}
		if !yes {
			return errors.New("operation not aborted")
		}
		if s.Kind == "merge" {
			p, err := a.Repo.gitPath(s.TargetPath, "MERGE_HEAD")
			if err != nil {
				return err
			}
			if _, err = os.Stat(p); err == nil {
				if err = a.conflictUnchanged(s, s.Snapshot); err != nil {
					return err
				}
				if _, err = a.Repo.Git(s.TargetPath, "merge", "--abort"); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		s.Stage = "aborted"
		return a.saveState(s)
	}
	yes, err := a.confirm("Revalidate the current state and resume?")
	if err != nil {
		return err
	}
	if !yes {
		return errors.New("operation not resumed")
	}
	switch s.Kind {
	case "new":
		return a.continueCreate(s)
	case "remove":
		return a.continueRemove(s)
	case "merge":
		if s.Stage == "planned" {
			return a.startMerge(ctx, s)
		}
		mergeHead, err := a.Repo.gitPath(s.TargetPath, "MERGE_HEAD")
		if err != nil {
			return err
		}
		if _, err = os.Stat(mergeHead); errors.Is(err, os.ErrNotExist) {
			if err = a.Repo.Clean(s.TargetPath); err != nil {
				return err
			}
			head, err := a.Repo.Head(s.TargetPath)
			if err != nil {
				return err
			}
			if head != s.TargetHEAD {
				if _, err = a.Repo.Git(s.TargetPath, "merge-base", "--is-ancestor", s.SourceHEAD, head); err == nil {
					s.Stage = "completed"
					return a.saveState(s)
				}
			}
			return errors.New("no merge in progress; check the state and clear the saved operation with wsup abort")
		} else if err != nil {
			return err
		}
		return a.resolve(ctx, s)
	}
	return errors.New("unknown interrupted operation")
}
