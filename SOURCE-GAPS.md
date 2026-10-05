# Source review and adaptations

Source: `../blank-agent-project/Blank-Project`, inspected on 2026-10-05.
Source repository revision: `be0d33d76067108923cd4a3858dd78f8dd48605a`.
This project adapts its six roles without modifying the source template.

| Source role | Retained in AGENT.md |
|---|---|
| CEO (`IDENTITY-CEO.md`, `CEO-PROCESS.md`) | Intake, scope/dependencies, work order, acceptance map, final audit, blockers, write-back. |
| Architect (`IDENTITY-ARCHITECT.md`, `ARCHITECT-PROCESS.md`) | Contracts, invariants, existing patterns, locked decisions, systemic root-cause fixes. |
| Security (`IDENTITY-SECURITY.md`, `SECURITY-PROCESS.md`) | Design review, diff review, secrets/auth/data boundaries, explicit pre-release sign-off. |
| Builder (`IDENTITY-BUILDER.md`, `BUILDER-PROCESS.md`) | Narrow implementation, regression tests, versioned migrations, existing git identity, signed commits, push and PR. |
| QA (`IDENTITY-QA.md`, `QA-PROCESS.md`) | Failing-first regression tests, baseline failures, behavioral acceptance evidence, browser/device requirements. |
| DevOps (`IDENTITY-DEVOPS.md`, `DEVOPS-PROCESS.md`) | PR-only release handoff, migration/env requirements, verification and rollback guidance; owner handles deployment. |

## Conflicts resolved for the requested single agent

- **Independent QA:** impossible with one agent. Sequential self-review retains
  evidence and severity reporting, but PR review must supply true independence.
- **Parallel test work:** source CEO cycle writes QA tests in parallel with
  Builder. Now tests precede implementation for bugs and QA follows the build.
- **Builder testing contradiction:** `BUILDER-PROCESS.md` forbids tests, while
  `IDENTITY-BUILDER.md` requires writing/running them. The new workflow explicitly
  keeps testing within Builder and QA responsibilities. The source has no
  separate Testing role.
- **Deploy order contradiction:** CEO's cycle deploys staging before QA, but its
  closing rule says no deploy until QA has tested. The owner selected PR-only
  for all projects; local checks and review precede the PR, and DevOps prepares
  a release handoff. Deployment is outside this workflow.
- **Git flow:** generic source process pulls `main` on deploy; source identity
  instead discovers the integration branch. Owner requires `staging`, now
  enforced for every PR. Never pull/deploy `main` by assumption or auto-merge.
- **Branch names:** source prefers no ticket namespace; automatic runs use the
  issue identifier and UUID suffix for collision-resistant routing/recovery.
- **Agent files in git:** source forbids pushing agent state and creating new
  Markdown in product repositories. This repository intentionally contains the
  requested agent toolkit/docs. Target product checkouts retain the hygiene rule;
  generated worklogs, findings, evidence and lessons live outside them.

## Source gaps handled in the adaptation

- **Local cron setup (resolved by host prerequisite):** the host supplies its
  OS-installed `crontab` command and running cron service. The installer manages
  the current user's crontab, preserves unrelated entries, backs up the previous
  contents, installs one five-minute Python invocation, and verifies the entry.
  Project routes are collected with `install-cron --setup`. The scheduled user's
  environment must supply `LINEAR_API_KEY`, CLI authentication, and git signing;
  activation remains deferred until those setup inputs are supplied.
- **Deployment configuration (resolved by owner decision):** all projects use
  `pr-only`, environment `none`, and PRs into `staging`. Configuration validation
  rejects preview/direct deployment policies. DevOps documents release needs
  for the owner; the agent never merges or deploys. No host, deployment pipeline,
  or deployment credentials are required for this workflow.
- **Test strategy (resolved by owner decision):** the source references a missing
  `TESTING.md`. QA will determine repository-specific test commands, coverage,
  and required browser/device evidence during each issue session. No separate
  test-strategy document is required for setup.
- **Linear project routing:** `python3 runner.py install-cron --setup` collects
  project/team/ready-status UUIDs, local repository paths, and optional labels.
  It derives GitHub repositories from local origins, validates UUIDs and matching
  origins, preserves existing routes and deployment policies, and saves the
  mappings privately before cron installation. Multiple projects use the same
  worker, with one team and ready status per project. Actual project values are
  supplied during setup; they are no longer a missing workflow capability.
- **Workspace guards:** source assertions/setup require filled project paths and
  runtime provisioning. The runner validates routing and uses a separate clone
  based on the remote staging SHA; it never invokes placeholder source setup
  scripts or edits global git identity.
- **Credential notes:** TOOLS suggests writing API keys/connection details into
  notes. The agent records credential source names only, and the runner reads
  `LINEAR_API_KEY` from its environment and removes it from the agent environment.
  A workspace-write sandbox is not a credential vault: trusted local sessions
  can read user-accessible files and saved auth.

## Remaining setup and verification

1. **Setup smoke test:** supply `LINEAR_API_KEY` in the local environment and
   collect routes with `python3 runner.py setup`, then run
   `python3 runner.py smoke-test`. This checks configuration, CLI authentication,
   GitHub write permission, remote `staging` access, git identity, cron executable
   availability, and read-only Linear queues. It starts no feature sessions and
   creates no commits, PRs, issue claims, or cron jobs. Confirm project/team/status
   membership in Linear when the queue is empty. Actual signing, model execution,
   issue-to-PR delivery, and cron's scheduled environment remain unproven by this
   smoke test and will be verified later when processing is explicitly enabled.

## Limits to verify before activation

Acceptance extraction, criterion completeness and security review are agent
judgments. The Python gates reject empty/failed evidence and
check the actual GitHub PR, but cannot establish that the agent omitted no
requirement or proved every behavioral claim. When feature processing is later
authorized, review its first real issue against the original body and final
artifacts. Initial setup verification remains a smoke test only.

Local locking/claims coordinate one installation on one host. Interrupted work
is held rather than auto-retried. Linear status updates, comments, distributed
claiming, auto-merge, and automatic recovery are not implemented. They are not
needed for the agreed local issue-to-PR workflow; add them only with a defined
policy. Deploy-dependent acceptance criteria remain blocked for owner
release/verification under the PR-only policy.
