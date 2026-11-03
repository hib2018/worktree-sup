package wsup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type githubIssue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	URL         string          `json:"html_url"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func (a *App) openIssues(ctx context.Context) ([]githubIssue, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "--method", "GET", "--paginate", "--slurp", "repos/{owner}/{repo}/issues?state=open&per_page=100")
	cmd.Dir = a.Repo.Root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	// Resolve placeholders from this checkout, not an inherited repository override.
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GH_REPO=") && !strings.HasPrefix(env, "GH_PROMPT_DISABLED=") && !strings.HasPrefix(env, "GIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("fetch open issues with gh: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var pages [][]githubIssue
	if err = json.Unmarshal(data, &pages); err != nil {
		return nil, fmt.Errorf("invalid gh issue response: %w", err)
	}
	if pages == nil {
		return nil, errors.New("invalid gh issue response: expected pages")
	}
	var issues []githubIssue
	seen := map[int]bool{}
	for _, page := range pages {
		if page == nil {
			return nil, errors.New("invalid gh issue page")
		}
		for _, issue := range page {
			if len(issue.PullRequest) > 0 && string(issue.PullRequest) != "null" {
				continue
			}
			u, err := url.Parse(issue.URL)
			if issue.Number <= 0 || strings.TrimSpace(issue.Title) == "" || strings.ContainsAny(issue.Title, "\x00\r\n\t\x1b") || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || !strings.HasSuffix(u.Path, "/issues/"+strconv.Itoa(issue.Number)) || u.RawQuery != "" || u.Fragment != "" || seen[issue.Number] {
				return nil, errors.New("invalid or duplicate GitHub issue")
			}
			seen[issue.Number] = true
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

func (a *App) createIssues(ctx context.Context, args []string) error {
	f := a.flags("issue")
	base := f.String("base", "", "Base branch for all issue worktrees")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("issue takes only --base BRANCH")
	}
	issues, err := a.openIssues(ctx)
	if err != nil {
		return err
	}
	if len(issues) == 0 {
		fmt.Fprintln(a.Out, "No open issues")
		return nil
	}
	statuses := make([]string, len(issues))
	for i := range statuses {
		statuses[i] = "Unprocessed"
	}
	defer func() {
		fmt.Fprintln(a.Out, "Issue results:")
		for i, issue := range issues {
			fmt.Fprintf(a.Out, "%s: #%d — %s\n", statuses[i], issue.Number, issue.Title)
		}
	}()
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
	m, err := a.loadMetadata()
	if err != nil {
		return err
	}
	for i, issue := range issues {
		if err := ctx.Err(); err != nil {
			return err
		}
		branch := fmt.Sprintf("issue-%d", issue.Number)
		for _, e := range m.Entries {
			if e.IssueNumber == issue.Number && e.IssueURL == issue.URL {
				if err := a.verifyEntry(e); err != nil {
					return fmt.Errorf("cannot verify existing worktree for issue #%d: %w", issue.Number, err)
				}
				statuses[i] = "Skipped"
				break
			}
		}
		if statuses[i] == "Skipped" {
			continue
		}
		if _, exists := m.Entries[branch]; exists {
			return fmt.Errorf("metadata collision for %s", branch)
		}
		e := Entry{Branch: branch, Summary: issue.Title, Base: *base, Target: *base, IssueNumber: issue.Number, IssueURL: issue.URL}
		if err := a.createEntry(e, before); err != nil {
			// A failed Git command or state write may already have created resources.
			statuses[i] = "Incomplete / unprocessed (inspect saved operation)"
			return fmt.Errorf("issue #%d: %w", issue.Number, err)
		}
		statuses[i] = "Created"
	}
	return nil
}
