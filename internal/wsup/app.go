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
)

const help = `worktree-sup (wsup) — fxを使うGit worktree管理CLI

  wsup new [--base BRANCH] [作業内容]
  wsup list
  wsup merge [--target BRANCH]    一覧から対象を選択
  wsup remove                    一覧から対象を選択
  wsup config test -- COMMAND [ARGS...]
  wsup config test --clear
  wsup status
  wsup resume
  wsup abort

競合の解消差分は承認後だけ適用します。push/rebase/squash/強制削除はしません。
fx連携はmacOSのnative fx + sandbox-execに対応します（他環境では安全側に停止）。
`

type App struct {
	Repo   *Repo
	Engine Engine
	In     *bufio.Reader
	Out    io.Writer
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
	if len(args) == 0 {
		return errors.New("コマンドが必要です")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("listは引数を取りません")
		}
		return a.list()
	case "status":
		if len(args) != 1 {
			return errors.New("statusは引数を取りません")
		}
		s, err := a.loadState()
		if err != nil {
			return err
		}
		if s == nil {
			fmt.Fprintln(a.Out, "保存された操作はありません")
			return nil
		}
		fmt.Fprintf(a.Out, "%s / %s\nsource: %s\ntarget: %s\npath: %s\n", s.Kind, s.Stage, s.Source, s.Target, s.TargetPath)
		return nil
	}
	switch args[0] {
	case "new", "merge", "remove", "config", "resume", "abort":
	default:
		return fmt.Errorf("不明なコマンド: %s", args[0])
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
	case "merge":
		return a.merge(ctx, args[1:])
	case "remove":
		if len(args) != 1 {
			return errors.New("removeは引数を取りません。一覧から選択してください")
		}
		return a.remove()
	case "config":
		return a.config(args[1:])
	case "resume", "abort":
		if len(args) != 1 {
			return errors.New("引数が多すぎます")
		}
		return a.recover(ctx, args[0] == "abort")
	}
	return nil
}
func (a *App) ask(prompt string) (string, error) {
	fmt.Fprint(a.Out, prompt+": ")
	line, err := a.In.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("入力を完了できません: %w", err)
	}
	return strings.TrimSpace(line), nil
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
		return errors.New("wsup config test -- COMMAND [ARGS...] または --clear")
	}
	c := Config{}
	if len(args) == 2 && args[1] == "--clear" {
	} else if args[1] == "--" && len(args) > 2 {
		c.Test = args[2:]
	} else {
		return errors.New("テストコマンドは -- の後に指定してください")
	}
	if err := writeJSON(a.storage("config.json"), c); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "テスト設定を保存しました")
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
		summary := "未登録"
		if e, ok := m.Entries[t.Branch]; ok && e.Path == t.Path {
			summary = e.Summary
		}
		state := "clean"
		if t.Bare {
			state = "bare"
		} else if err := a.Repo.Clean(t.Path); err != nil {
			state = "dirty / 操作中 / 読取不可"
		}
		branch := t.Branch
		if t.Detached {
			branch = "(detached)"
		}
		path, err := filepath.Rel(root, t.Path)
		if err != nil {
			return fmt.Errorf("worktreeの相対パス: %w", err)
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
		return Entry{}, errors.New("管理対象のworktreeがありません。wsup newで作成してください")
	}
	choice, err := a.ask("対象番号")
	if err != nil {
		return Entry{}, err
	}
	n, err := strconv.Atoi(choice)
	if err != nil || n < 1 || n > len(candidates) {
		return Entry{}, errors.New("無効な対象番号")
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
	return errors.New("worktreeのパスまたはブランチが記録と一致しません")
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
	return "", fmt.Errorf("統合先 %s を別のworktreeにcheckoutしてください（自動checkoutはしません）", branch)
}
func (a *App) create(ctx context.Context, args []string) error {
	f := a.flags("new")
	base := f.String("base", "", "起点ブランチ")
	if err := f.Parse(args); err != nil {
		return err
	}
	intent := strings.Join(f.Args(), " ")
	var err error
	if intent == "" {
		intent, err = a.ask("作業内容")
		if err != nil {
			return err
		}
	}
	if intent == "" {
		return errors.New("作業内容は必須です")
	}
	if *base == "" {
		*base, err = a.ask("起点ブランチ")
		if err != nil {
			return err
		}
	}
	before, err := a.branchHEAD(*base)
	if err != nil {
		return err
	}
	if a.Engine == nil {
		return errors.New("fxエンジンがありません")
	}
	n, err := a.Engine.Name(ctx, intent, "起点: "+*base+"。summaryは日本語で簡潔に。branchは短く。")
	if err != nil {
		return err
	}
	if err = ValidBranch(n.Branch); err != nil {
		return err
	}
	if strings.TrimSpace(n.Summary) == "" || strings.ContainsAny(n.Summary, "\x00\r\n\t") {
		return errors.New("不正な作業要約")
	}
	if a.Repo.BranchExists(n.Branch) {
		return errors.New("生成ブランチ名は既に存在します")
	}
	trees, err := a.Repo.Worktrees()
	if err != nil {
		return err
	}
	if len(trees) == 0 {
		return errors.New("worktreeがありません")
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
		return errors.New("保存先にsymlinkを含めることはできません")
	}
	path := filepath.Join(parent, strings.ReplaceAll(n.Branch, "/", "-"))
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("保存先が既に存在するか検証できません")
	}
	now, err := a.branchHEAD(*base)
	if err != nil {
		return err
	}
	if now != before {
		return errors.New("起点ブランチが変更されました。再実行してください")
	}
	e := Entry{Branch: n.Branch, Summary: n.Summary, Base: *base, Target: *base, Path: path}
	s := &State{Kind: "new", Stage: "planned", Source: n.Branch, Target: *base, SourcePath: path, TargetHEAD: before, Entry: &e}
	fmt.Fprintf(a.Out, "起点: %s\n生成名: %s\n要約: %s\n作成パス: %s\n", *base, n.Branch, n.Summary, path)
	if err = a.saveState(s); err != nil {
		return err
	}
	return a.continueCreate(s)
}
func (a *App) continueCreate(s *State) error {
	if s.Entry == nil {
		return errors.New("作成情報がありません")
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
		return errors.New("保存先が変更されたか安全ではありません")
	}
	// Only the original planned path is used; never reuse an existing directory.
	if s.Stage == "planned" {
		if _, err := os.Lstat(e.Path); errors.Is(err, os.ErrNotExist) {
			head, err := a.branchHEAD(e.Base)
			if err != nil {
				return err
			}
			if head != s.TargetHEAD {
				return errors.New("起点が変更されています")
			}
			if a.Repo.BranchExists(e.Branch) {
				return errors.New("ブランチが既に存在します。自動復旧しません")
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
			return errors.New("作成先HEADが変更されています")
		}
		s.Stage = "worktree-created"
		if err = a.saveState(s); err != nil {
			return err
		}
	}
	if s.Stage != "worktree-created" {
		return errors.New("不正な作成段階")
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
	fmt.Fprintln(a.Out, "作成しました:", e.Path)
	return nil
}
func (a *App) merge(ctx context.Context, args []string) error {
	f := a.flags("merge")
	target := f.String("target", "", "統合先ブランチ")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("対象は一覧から選択してください")
	}
	e, err := a.selectEntry()
	if err != nil {
		return err
	}
	if *target == "" {
		*target = e.Target
	}
	if *target == e.Branch {
		return errors.New("同じブランチへはマージできません")
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
	fmt.Fprintf(a.Out, "対象: %s\n統合先: %s (%s)\n通常のGit mergeは競合マーカー等を作成する場合があります。\n", s.Source, s.Target, path)
	if err = a.saveState(s); err != nil {
		return err
	}
	return a.startMerge(ctx, s)
}
func (a *App) startMerge(ctx context.Context, s *State) error {
	if err := a.checkMergeStart(s); err != nil {
		return err
	}
	s.Stage = "merging"
	if err := a.saveState(s); err != nil {
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
		fmt.Fprintln(a.Out, "マージが完了しました（自動削除しません）")
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
	fmt.Fprintf(a.Out, "競合状態を保存しました。中止: git -C %q merge --abort または wsup abort\n", s.TargetPath)
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
		return errors.New("ブランチHEADが変更されました")
	}
	path, err := a.targetPath(s.Target)
	if err != nil || path != s.TargetPath {
		return errors.New("統合先worktreeが変更されました")
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
		return errors.New("統合先の状態が変更されました")
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
	fmt.Fprintf(a.Out, "削除パス: %s\nブランチ: %s\n統合先: %s（取り込み済み）\n", e.Path, e.Branch, e.Target)
	yes, err := a.confirm("worktreeとブランチを削除しますか？")
	if err != nil {
		return err
	}
	if !yes {
		return errors.New("削除を中止しました")
	}
	if err = a.saveState(s); err != nil {
		return err
	}
	return a.continueRemove(s)
}
func (a *App) checkRemove(s *State) error {
	if s.Source == s.Target {
		return errors.New("統合先自身は削除できません")
	}
	head, err := a.branchHEAD(s.Source)
	if err != nil {
		return err
	}
	if head != s.SourceHEAD {
		return errors.New("削除対象HEADが変更されました")
	}
	if _, err = a.branchHEAD(s.Target); err != nil {
		return err
	}
	if _, err = a.Repo.Git(a.Repo.Root, "merge-base", "--is-ancestor", s.SourceHEAD, "refs/heads/"+s.Target); err != nil {
		return errors.New("記録された統合先に取り込み済みと確認できません")
	}
	return nil
}
func (a *App) continueRemove(s *State) error {
	if s.Entry == nil {
		return errors.New("削除情報がありません")
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
					return errors.New("削除対象の登録が残るか別worktreeで使用されています")
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
			return errors.New("現在のディレクトリを含むworktreeは削除できません")
		}
		// Ignored files are also user data. Git's default remove can delete them.
		ignored, err := gitRaw(s.SourcePath, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
		if err != nil {
			return err
		}
		if len(ignored) > 0 {
			return errors.New("ignoredファイルが残っています。削除せず停止します")
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
		return errors.New("不正な削除段階")
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
				return errors.New("削除対象ブランチは別worktreeで使用されています")
			}
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
	fmt.Fprintln(a.Out, "削除しました")
	return nil
}
func (a *App) recover(ctx context.Context, abort bool) error {
	s, err := a.loadState()
	if err != nil {
		return err
	}
	if s == nil || s.Stage == "completed" || s.Stage == "aborted" {
		return errors.New("中断操作はありません")
	}
	fmt.Fprintf(a.Out, "中断操作: %s / %s\n対象: %s -> %s\n", s.Kind, s.Stage, s.Source, s.Target)
	if abort {
		yes, err := a.confirm("保存した操作を中止しますか？（作成済みworktreeは保持します）")
		if err != nil {
			return err
		}
		if !yes {
			return errors.New("中止しませんでした")
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
	yes, err := a.confirm("現在の状態を再検証して再開しますか？")
	if err != nil {
		return err
	}
	if !yes {
		return errors.New("再開しませんでした")
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
			return errors.New("進行中のマージがありません。状態を確認しwsup abortで記録を中止してください")
		} else if err != nil {
			return err
		}
		return a.resolve(ctx, s)
	}
	return errors.New("不明な中断操作")
}
