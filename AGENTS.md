# Repository Instructions

## GitHub Workflow

- Use the authenticated GitHub CLI (`gh`) for GitHub operations.
- Track each independently deliverable task with one Issue, one branch, and one pull request.
- Write Issues in English.
- Keep the pull request within the linked Issue's scope and use `Closes #<number>` in its body.
- Create a separate Issue for unrelated work discovered during implementation.

## Worktrees

- Implement code, configuration, and behavior changes in a dedicated Git worktree.
- Documentation-only changes may be implemented without creating a separate worktree.
- Store task worktrees under the repository's `.worktrees/` directory.
- Name task branches `issue-<number>-<short-description>`.
- Create a task worktree from the current remote `main` branch:

```bash
git fetch origin main
git worktree add .worktrees/issue-<number>-<short-description> \
  -b issue-<number>-<short-description> origin/main
```

## Commits and Pull Requests

- Write commit messages and pull request titles in English.
- Use this format with a concise imperative description:

```text
<type>[optional scope]: <description>
```

- Record the verification performed and the documentation impact in the pull request.
- Merge with squash and request remote branch deletion:

```bash
gh pr merge <number> --squash --delete-branch \
  --subject "<type>[optional scope]: <English description>"
```

## Post-merge Cleanup

After merging a pull request:

1. Confirm the pull request is merged with `gh pr view <number> --json state,mergedAt`.
2. If a task worktree was used, confirm it has no uncommitted changes, then remove it with `git worktree remove <path>`.
3. Run `git worktree prune`.
4. Update the primary worktree with `git switch main` and `git pull --ff-only`.
5. Delete the local task branch. Because squash merge does not preserve branch ancestry, use `git branch -D issue-<number>-<short-description>` only after confirming the pull request was merged.
6. Confirm the remote task branch was deleted. Delete it with `git push origin --delete issue-<number>-<short-description>` if necessary.
7. Confirm the Issue is closed with `gh issue view <number> --json state`. If `Closes #<number>` did not close it, run `gh issue close <number> --reason completed`.

Do not remove a worktree or force-delete a branch when it contains uncommitted or unmerged work.
