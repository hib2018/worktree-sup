# worktree-sup

English | [日本語](README.ja.md)

`wsup` is a Git worktree management CLI powered by fx. Written in Go, it supports worktree creation, listing, regular merges, approval-based conflict resolution, and safe removal.

## Build

Requires Go 1.23 or later and Git with support for `worktree list --porcelain -z`.

```sh
go build -o wsup ./cmd/wsup
# Or install:
go install ./cmd/wsup
```

fx integration currently requires **macOS, a native Mach-O fx binary, and `/usr/bin/sandbox-exec`**. On Linux, operations requiring an LLM stop rather than falling back to unsandboxed execution. Shell wrappers around fx are also rejected. The executable is located through `PATH`.

Your normal fx configuration, saved logins, MCP servers, and skills are not inherited. fx runs with an isolated temporary HOME, so provide a supported provider's API key through an environment variable. Only `AI_GATEWAY_API_KEY`, `OPENAI_API_KEY`, and `ANTHROPIC_API_KEY` are forwarded. Provider selection follows fx's defaults; the default Gateway requires `AI_GATEWAY_API_KEY`. Authentication failures or invalid output stop the operation.

## Usage

Run inside a Git repository. Submit interactive input with Enter. Approval requires an explicit `yes`.

```sh
wsup new --base main 'Improve login form validation'
wsup list
wsup merge
wsup merge --target release
wsup remove
```

- `new`: Provide a task description and base branch, or enter them when prompted. fx generates a short branch name and a Japanese summary. The CLI validates the name, checks for duplicates and a safe destination, then displays the base, name, summary, and path before creation. Existing changes are never moved or copied.
- Worktrees are stored in a sibling directory formed by appending `.worktrees` to the primary worktree's path. Slashes in branch names become hyphens in destination paths. A collision stops the operation.
- `list`: Displays a read-only listing without calling fx. A horizontal rule separates the headings from the values, and only headings are bold in terminals. Styling is disabled for redirected output, when `NO_COLOR` is set, or with `TERM=dumb`. `PATH` is relative to the main repository root—the primary worktree—with `.` representing the root itself. This base stays the same when running from a linked worktree. Externally created worktrees are labeled `未登録` (unregistered).
- `merge`: Select a managed worktree from a list. The default target is its original base branch. Changes made with `--target` are recorded. The target must already be checked out in another worktree; automatic checkout is not performed.
- `remove`: Select a managed worktree, review its path, branch, and integration status, then confirm removal. Removal is allowed only when the worktree is clean and its commits are ancestors of the recorded target. Remaining ignored files also block removal. A worktree containing the current directory cannot be removed.

Merging or removal stops if an affected worktree has uncommitted changes, untracked files, or an ongoing Git operation. Ignored files that collide with incoming paths are not overwritten. Before merging, the CLI warns that a regular Git merge may produce conflict markers.

## Conflict resolution and tests

When conflicts occur, wsup sends the base, both sides, and current conflicted file contents to fx. It displays the change intent, resolution plan, and exact proposed patch. No resolution edits, staging, or commits occur before approval. Approval applies only to that patch and repository state. Changes to HEAD, the index, or files prevent application.

The initial implementation supports automatic resolution of regular UTF-8 text files. Binary files, symlinks, submodules, conflicts involving deletions, mode changes, and other unsupported cases stop safely. Conflict context and patches are each limited to 2 MiB. Take care when sending files containing secrets.

You can explicitly configure a repository-wide test command. It is executed as an argument array, without implicit shell interpretation.

```sh
wsup config test -- go test ./...
wsup config test --clear
```

After applying an approved resolution, wsup checks that no unresolved conflicts remain, asks for completion confirmation, and runs the configured tests. Test failures prevent the merge commit. Tests that modify tracked files, the index, or other checked state also stop completion. Additional fixes are never applied automatically using an old approval. Without configured tests, wsup reports the merge as unverified and asks whether to continue.

Conflict-free regular merges complete without additional approval or tests. wsup does not automatically rebase, squash, push, commit or stash existing changes, force removal, or remove worktrees after merging.

## Interruption and recovery

Summaries, base and target branches, configuration, operation stages, and proposed patches are stored under `wsup/` in the shared Git directory. This metadata is shared across worktrees and is not tracked by Git. Mutating operations are serialized using an OS lock on `wsup.lock` in the shared directory. The lock is released when the process exits.

```sh
wsup status
wsup resume
wsup abort
```

- `resume` checks the current state before continuing. Unapplied resolution proposals are regenerated and require new approval. If a resolution was already applied, wsup verifies the saved state and asks again before testing and completing the merge.
- `abort` requires explicit confirmation and marks the operation as aborted. It does not automatically restore or delete already-created worktrees or partially removed resources.
- A pending merge is aborted with `git merge --abort` only when its state matches the saved snapshot. Manual changes after interruption prevent automatic recovery. Inspect the state and, if necessary, run `git merge --abort` manually in the target worktree.
- Git execution and state persistence are not a single transaction. Recovery stops when safety cannot be established, such as when only a branch was created, and requires manual inspection.

Manual Git commands and external agents do not honor wsup's lock. Immediate revalidation and compare-and-delete ref operations detect changes, but full atomicity with external operations is not guaranteed. Avoid concurrent edits to the same worktree.

## fx isolation boundary

fx does not read the Git repository directly. Only the required text is passed through standard input. Runtime restrictions deny repository reads, file writes outside the temporary directory, forking, and execution of other programs such as shells or Git. Git mutations are performed by the CLI using validated arguments and approved patches. Automatic Git hooks and signing are also disabled.

Network access is allowed for model connections. This is not complete network isolation: it does not prevent exfiltration of supplied content or API keys, or network-mediated side effects. The installed fx binary, model provider, explicitly configured test commands, and Git configuration are trusted.

## Development and verification

```sh
go test ./...
go test -race ./...
go vet ./...
# Check installed fx help/version in isolation, without calling a model:
WSUP_FX_SMOKE=1 go test ./internal/wsup -run '^TestFXInstalledSmoke$' -v
```

Tests use temporary Git repositories and fake LLM engines. Normal tests do not call live models or incur API charges. Authentication and response compatibility with a real model require separate verification in your environment.
