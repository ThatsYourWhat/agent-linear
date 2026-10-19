# Single agent for Linear issues

One fresh Codex or OpenCode CLI session handles one issue through all six source roles.
Feature PRs always target **`staging`**. Linear integration and local cron
activation are deferred until the owner configures project routes. The Linear
API key is supplied through the local `LINEAR_API_KEY` environment variable.
No live issue, PR, deployment, or cron job has been created by this setup.

- [AGENT.md](AGENT.md): reusable, sequential agent directives.
- [config.example.json](config.example.json): trusted Linear-project → repository
  mapping and deployment authority. Multiple projects may use the same agent.
- [runner.py](runner.py): paginated Linear polling, fresh isolated checkouts,
  serial launches, persistent issue claims, and GitHub PR verification.
- [result.schema.json](result.schema.json): acceptance evidence and final result.

## Use the directives now

In a fresh session in the target repository, supply `AGENT.md`, the complete
Linear issue body, and this trusted context separately from the issue:

```text
Handle only this issue using the attached AGENT.md sequential workflow.
Target: OWNER/REPO; base: staging; checkout: ABSOLUTE_PATH
Feature branch: UNIQUE_BRANCH; baseline SHA: STAGING_SHA
Scratch directory outside product checkout: ABSOLUTE_PATH
Signed commits required: true
Deployment: pr-only (owner handles releases; do not merge or deploy)
Requirements and acceptance criteria: [complete issue body]
Return the final criterion evidence, checks, security/QA verdict, branch/SHA,
PR URL, deployment state, and any blockers. Do not merge.
```

The runner supplies this context automatically. Do not paste API keys into it.
A single session's self-review does not replace independent review on the PR.

## Configure later

Needs Linux, Python 3, git, an authenticated Codex or OpenCode CLI, authenticated GitHub CLI,
access to each GitHub repository, and its remote `staging` branch. Every feature
commit must use the local Git profile's author and signing key. The runner reads
effective `user.name`, `user.email`, `user.signingkey`, and signing settings from
the mapped local repository, honoring repository-local overrides and conditional
includes. It requires all three profile values and resolves the key fingerprint.
It applies the captured settings only to the isolated checkout, enables commit
signing there, and supplies the same author/committer identity to the session.
It does not change the source checkout's or global Git configuration or copy
private keys. All routes require `require_signed_commits: true`; disabling it
is rejected. Unavailable signing keys or failed verification block the run.

```bash
cd /home/noah/Documents/blank-agent-linear
cp config.example.json config.json
chmod 600 config.json
```

Edit `config.json`: use real project/team/ready-state UUIDs, the existing local
repository path and matching GitHub `OWNER/REPO`. Replace every placeholder.
Find UUIDs using Linear's command menu → “Copy model UUID”. A ready-state plus
the optional `agent-ready` label opts issues into processing; remove `label`
if readiness should depend only on status. Polling includes all currently
eligible issues, including existing ones, rather than a creation-time cutoff.

Supply the Linear API key through the local `LINEAR_API_KEY` environment variable.
The runner does not load credential files. Credentials are not stored in config,
issue snapshots, prompts, or the state database, and the Linear key is removed
from the agent's environment. The runner posts claim and finish comments through
Linear's API and changes issue status; the key must allow comment creation,
workflow-state/user reads, and issue updates. At start, the runner assigns the issue
to the authenticated user returned by Linear's `viewer` query using that runner's
`LINEAR_API_KEY`, replacing any existing assignee. Each teammate should use their
own personal key; the MCP connection and runner key can represent different users.
No user ID is hardcoded or added to config. Each routed
team must have exactly one status named `In Progress` and one named `In Review`.
The runner resolves these by name within that team; no new config is needed.
Smoke tests and dry runs remain read-only against Linear.

All projects use `pr-only` with environment `none`; the runner rejects preview
and direct deployment policies. The agent creates a PR into `staging`, then
DevOps documents release requirements for the owner. The agent does not merge
or deploy. Deploy-dependent acceptance criteria remain blocked for owner
release/verification. Existing repository CI may run automatically on a PR;
that does not authorize the agent to trigger a separate deployment.

## Choose the CLI

Codex is the default, including for existing configs without `engine`. To use
OpenCode, set these top-level fields in `config.json`, alongside the existing
`gh` and `projects` fields:

```json
{
  "engine": "opencode",
  "opencode": "/home/noah/.opencode/bin/opencode",
  "model": "provider/model"
}
```

Replace `provider/model` with an entry from `opencode models`. `model` is optional;
when omitted, the selected CLI uses its own default. Set `engine` back to `codex`
to use the existing `codex` executable setting. Remove or change `model` when
switching engines because their model names may differ. Each poll uses the
configured engine for all project routes; changing it never retries claimed issues.
The cron entry itself does not change. Prefer absolute executable paths for cron.

Configure provider access once as the OS user that runs cron (`opencode auth login`
for OpenCode). The cron process must have access to that user's credentials,
signing agent/key, and any required provider environment variables. Do not put
API keys in this runner's config. `smoke-test` checks Codex login status, or reads
OpenCode's credential/model inventory and checks an explicitly selected model.
An OpenCode inventory check does not prove a provider accepts requests; that
requires a live model run. Neither smoke test launches issue work.

OpenCode support uses `opencode run --format json` with the prompt on stdin,
verified against version 1.18.14. The runner extracts the final completed text
from its events into `result.json`; both engines' reports must match
`result.schema.json` before PR verification. Missing, malformed, interrupted,
or error results fail the run. No engine fallback or automatic retry occurs.

The runner gives OpenCode a per-run `linear-issue` agent: shell work is allowed,
subagents and questions are denied, and edits are restricted to the checkout
and scratch directory. Prior lesson files are readable. Permission prompts are
automatically rejected in noninteractive mode; the runner does not use `--auto`.
Sharing and automatic updates are disabled for the run. The settings are saved
as `opencode.json` in scratch for recovery. Native provider configuration is
still loaded; the runner supplies its own `OPENCODE_CONFIG_CONTENT` overrides.
These are OpenCode tool permissions, **not an OS filesystem sandbox**. Shell
commands run with the cron user's access. Use an isolated OS account/container
if OS-level confinement is required. Codex retains its `workspace-write` sandbox.

## Verify and run

The first live test is a setup smoke test, not feature implementation. With
`LINEAR_API_KEY` supplied in the environment, collect mappings without scheduling
anything, then check access:

```bash
python3 -m unittest -v test_runner.py
python3 runner.py setup
python3 runner.py smoke-test
```

`smoke-test` validates the configuration/local origins and PR-only policy,
checks git/crontab/selected-agent/GitHub executables, CLI preflight, GitHub write
permission, read access to each remote `staging` branch, Git profile/key resolution,
and the filtered Linear queues. It prints counts instead of issue bodies.
It does not launch an agent, claim issues, create SQLite state, change product
files, create commits/branches/PRs, or install cron. An empty queue does not prove
the project/team/status IDs belong together; verify those in Linear. The smoke
test does not exercise signing, a model request, Linear comment-write access,
or the cron job's environment.

Feature processing is enabled later, only when explicitly requested:

```bash
python3 runner.py poll --dry-run
python3 runner.py poll
python3 runner.py status
```

`--dry-run` queries Linear and reports eligible/previously claimed issues. It
does not start an agent, clone, push, create a PR, or deploy. It creates only the
local lock/state files. Real polling refreshes each issue immediately before
launching, so a withdrawn ready label/state does not start stale work.

For a saved issue snapshot, the same launcher can run with Linear access for
claim/finish comments and status updates:

```bash
python3 runner.py run --project REAL_PROJECT_UUID --issue-file /absolute/path/issue.json --dry-run
python3 runner.py run --project REAL_PROJECT_UUID --issue-file /absolute/path/issue.json
```

The snapshot uses Linear's `id`, `identifier`, `title`, `description`, `url`,
and nested `project.id`, `team.id`, `state.id`. Supply the full body and linked
dependency context. Saved snapshots are deliberate manual inputs, not refreshed
from Linear. A live target session still needs access to the selected CLI, its provider, and GitHub.

After the local claim is committed, the runner resolves the authenticated user and
both team statuses, then posts
`Run claimed on HOSTNAME. Starting work on IDENTIFIER.` before cloning or launching
the agent. If that comment cannot be confirmed, the run fails without starting feature
work. It then changes the issue to **In Progress**, confirming the returned issue
and status IDs before cloning or launching the agent. Missing/ambiguous statuses or
an unconfirmed start update fail the run before feature work starts.
The same In Progress mutation assigns the authenticated user, and the response must
confirm both the status and assignee before work launches. A failed user lookup or
unconfirmed assignment fails the run without launching feature work. The In Review
update does not change the assignee.
The comment announces ownership; SQLite remains the local duplicate guard,
and the comment is not a lock across machines.

After verifying a `ready` result (including its PR and signed commits), the runner
changes the issue to **In Review** before recording `ready`. Blocked, failed, and
invalid completion results do not trigger this transition. An unconfirmed review
update fails the run and preserves the result/PR for manual recovery. Failed or
blocked work otherwise stays In Progress; the runner never moves issues to Done.
Status mutation requests and confirmed responses are retained as `start-state*`
and `finish-state*` in scratch. No mutations are automatically retried; inspect
Linear before recovering from a timeout, which may have applied the update.

After saving the final outcome, the runner posts:

```text
Run finished on <hostname> with status <run status>. View pull request at <GitHub PR URL>. Resume the codex session: `codex resume <codex session id>`
```

Statuses are `ready`, `blocked`, or `failed`. Session IDs come from CLI events:
Codex's `thread.started.thread_id` or OpenCode's `sessionID`, not the agent report.
For OpenCode, the comment supplies `opencode --session ses_...` with the saved
`OPENCODE_CONFIG_CONTENT` permissions. The executable path follows the run's config. Resume on the named machine
under the original OS user, from `.runs/ISSUE_UUID/repo`. When no PR URL is recorded,
the comment says it is unavailable; when no session ID is recorded, it omits the
resume command and explains that the session is unavailable. A finish-comment
failure preserves the run outcome and logs delivery evidence in the issue's
scratch directory. Comment bodies, confirmed IDs, and delivery errors are retained
as `start-comment.*` and `finish-comment.*`. There are no automatic mutation retries:
after a timeout, check Linear before manually reposting. A killed runner may leave
only a start comment. A successful run adds six Linear API requests: one user
lookup, one status lookup, two status updates, and two comments.

Every issue is claimed in `.runs/state.sqlite3` before external work starts.
The existing repository is cloned with separate git metadata; only committed
source is copied, then the actual remote `staging` SHA is fetched as the base.
The user's uncommitted checkout stays intact. Branches use
`linear-IDENTIFIER-UUID8` to avoid collisions. No session resume or role fan-out
occurs. The agent creates the PR; the runner independently queries GitHub to
check its repository, base/head, SHA, open/non-draft state, and every feature
commit's raw author, signature validity, and signing-key fingerprint against the
profile captured before launch. A valid signature from another key is rejected.
The agent checks the same requirements before pushing.

The process holds a nonblocking `flock` for the entire tick; the agent child
inherits the lock descriptor, so an orphaned session retains the worker lock.
This is a **single-host** queue. Do not run another installation against the
same issues: local claims do not coordinate multiple machines.

## Install cron after the smoke test and explicit activation

The agreed interval is five minutes. The host is assumed to have its OS-provided
`crontab` command on PATH and its cron service running. The installer uses that
command to manage the current user's crontab; no OS-specific package installation
or service management is needed in this workflow.
The cron job must receive `LINEAR_API_KEY` through its own environment; an export
in an interactive shell does not configure cron. The installer does not copy
the key into the generated cron entry.

Use `python3 runner.py setup` to collect mappings for the smoke test without
scheduling. Later, to supply or update mappings while explicitly enabling cron:

```bash
python3 runner.py install-cron --setup
```

The prompts collect each Linear project UUID, team UUID, ready-status UUID,
local repository path, and optional label. The GitHub repository is read from
the local repository's origin. After each route, `Add another project? [y/N]`
defaults to finishing: press Enter or enter `n` to save and exit, or enter `y`
to configure another project. Existing routes and deployment policies are
preserved; new routes default to signed commits, PRs into `staging`, and
`pr-only` deployment. Validated mappings are saved privately to `config.json`,
then the installer checks Linear access and installs the single five-minute job.
Existing routes with preview/direct policies must be changed to `pr-only`,
environment `none`, and owner-release instructions before setup can save them.
The installer starts no issue sessions itself, but the installed polling job
can start feature work on its next tick. If installation fails, saved mappings
remain available for the next attempt.

For mappings already configured in `config.json`:

```bash
python3 runner.py smoke-test
python3 runner.py cron-line
python3 runner.py install-cron
crontab -l
```

The installer verifies config and read-only Linear access, reads the existing
crontab, backs it up at `.runs/crontab.before`, and replaces only its marked
`# blank-agent-linear` entry. It preserves other entries and uses absolute
Python/script/config paths plus the current PATH. Logs go to `.runs/cron.log`.
Cron directly invokes the Python runner; no Bash wrapper is required. Install
under the same OS user whose agent/provider and GitHub authentication and git signing setup
will be used by scheduled sessions.
To stop future polling, run under the same OS user:

```bash
python3 runner.py uninstall-cron
```

It removes only this workflow's marked entry, preserves other jobs, saves the
previous crontab to `.runs/crontab.before-uninstall`, and verifies removal.
It works without project config or credentials and while a worker is active.
Already-running sessions continue; re-enable polling with `install-cron`.
Keep the host awake; cron does not replay missed ticks. Inspect logs and local
status for failures; Linear comments provide per-run notifications.

## Evidence and recovery

`.runs/ISSUE_UUID/` retains issue/context/prompt snapshots, product checkout,
agent JSONL events, selected engine/model, stderr, final structured result, worklog/lessons, and failure
information. It is ignored by git and created private to the current user.
Do not commit it. Retain it until review/deployment is finished, then archive or
remove it deliberately; there is no automatic cleanup.

Blocked, failed, and interrupted claims are **never retried by cron**, even if
the issue body changes. A `running` database row is a claim, not proof of a live
process: inspect the event stream/session ID and current agent process/session
before deciding it died. For recovery, inspect the checkout and actual remote
branch/PR first. Continue that issue's recorded session with the original
checkout/context if appropriate; never `resume --last` into a different issue.
Reconcile the result/PR before any operator-approved reset. This version leaves
recovery manual; deleting state to force retries can duplicate external work.

The tests exercise a real throwaway git repository with executable Codex/OpenCode fixtures and simulated GitHub
responses, plus pagination, API errors, completion gates, signing rejection,
duplicate suppression, failed-run retention, crontab preservation, and the smoke
test's lack of issue-work/scheduling effects. They also cover claim/finish comments,
recorded session IDs, status transitions and failures, and notification failures
without changing run outcomes. They do not prove live Linear auth, status updates, or comment delivery,
model behavior, real GitHub delivery, or cron execution. The first live check
is only the smoke test; model/signing/issue-to-PR and scheduled execution evidence
can be collected later when feature processing is authorized. Deployment is
outside this workflow.

Reference: [official Codex non-interactive documentation](https://developers.openai.com/codex/noninteractive),
[OpenCode CLI](https://opencode.ai/docs/cli/),
[OpenCode permissions](https://opencode.ai/docs/permissions/),
[Linear GraphQL authentication/errors](https://linear.app/developers/graphql),
and [Linear filtering](https://linear.app/developers/filtering).
