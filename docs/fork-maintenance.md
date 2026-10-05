# Fork maintenance

Read when: preparing a fork PR or reviewing an upstream update without publishing to upstream.

`origin` is [gabrielMalonso/wacli](https://github.com/gabrielMalonso/wacli), the only push/PR destination in this workflow. `upstream` is [openclaw/wacli](https://github.com/openclaw/wacli), for read/fetch only; keep its push URL disabled. Verify `git remote -v` before pushing. Existing upstream release/site links do not authorize upstream writes or deployment.

Use short, focused branches from current fork `main`, a Conventional Commit, and an origin PR targeting `main`. Keep changes scoped, obtain maintainer review and pass the exact gate below. There is no automatic merge, release, deploy or tag creation. Dependencies, tooling and CI changes require explicit maintainer authorization.

For an upstream update, fetch into remote-tracking refs, then inspect `origin/main..upstream/main`. Select an existing local toolchain; use the [offline acceptance recipe](offline-acceptance.md) to isolate fixtures. In a clean, separate checkout/worktree, create a short branch from current `origin/main` and merge the reviewed upstream commit normally. Inspect every conflict against the fork's [agent](agent.md), [draft](drafts.md), [outbound](outbound.md), [history](history.md), [chat](chats.md) and [media](media.md) contracts; unresolved scope/behavior changes go to the maintainer before implementation. Preserve active work, avoid force-push/rebase of published history, and never run upstream deployment or tag workflows automatically.

```bash
# Inspect remotes first. These fetches do not write upstream branches.
git remote -v
git fetch --no-tags origin
git fetch --no-tags upstream
git log --oneline origin/main..upstream/main
git diff --stat origin/main...upstream/main

# In the isolated clean checkout, after selecting/reviewing a specific commit:
git switch -c maintenance/upstream-update origin/main
upstream_commit="$(git rev-parse upstream/main)"
git merge --no-ff --no-commit "$upstream_commit"
# Review staged changes/conflicts; finish only the approved merge locally.
```

Run the full gate before each PR, including an upstream merge PR, and rerun affected offline binary fixtures when their contracts change:

```bash
pnpm format:check && pnpm lint && pnpm lint:deadcode && pnpm test && pnpm build && pnpm docs:site && git diff --check
```

Record the upstream SHA, substantive conflict decisions, test/log evidence and live checks still pending in the fork PR. Push only its branch to `origin`, link the PR to the working T3 thread, and leave merge to the maintainer. When authorized to merge a fork PR, return to `main` and `git pull --ff-only origin main`. Release/tag/deploy actions remain a separate explicit task under [release instructions](release.md).
