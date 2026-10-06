#!/usr/bin/env python3
"""One Linear issue, one fresh Codex session. Python standard library only."""

import argparse
import fcntl
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent
FIELDS = """id identifier title description url updatedAt createdAt
  project { id name } team { id } state { id }
  relations { nodes { type relatedIssue { identifier state { type } } } }"""
QUERY = """query Queue($filter: IssueFilter!, $after: String) {
  issues(first: 50, after: $after, filter: $filter, orderBy: createdAt) {
    nodes { %s } pageInfo { hasNextPage endCursor }
  }
}""" % FIELDS


def command(args, cwd=None):
    return subprocess.run(args, cwd=cwd, check=True, text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout.strip()


def graphql(query, variables):
    key = os.environ.get("LINEAR_API_KEY", "")
    if not key:
        raise ValueError("Set LINEAR_API_KEY in the environment")
    request = urllib.request.Request(
        "https://api.linear.app/graphql",
        data=json.dumps({"query": query, "variables": variables}).encode(),
        headers={"Authorization": key, "Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=30) as response:
        payload = json.load(response)
    if payload.get("errors") or not isinstance(payload.get("data"), dict):
        # Do not echo provider response bodies: they can contain private issue data.
        raise ValueError("Linear GraphQL failed; verify credentials, scope and query")
    return payload["data"]


def issues(project_id, route, issue_id=None):
    filters = {"project": {"id": {"eq": project_id}},
               "team": {"id": {"eq": route["team_id"]}},
               "state": {"id": {"eq": route["ready_state_id"]}}}
    if route.get("label"):
        filters["labels"] = {"name": {"eq": route["label"]}}
    if issue_id:
        filters["id"] = {"eq": issue_id}
    after = None
    while True:
        page = graphql(QUERY, {"filter": filters, "after": after})["issues"]
        yield from page["nodes"]
        if not page["pageInfo"]["hasNextPage"]:
            break
        cursor = page["pageInfo"]["endCursor"]
        if not cursor or cursor == after:
            raise ValueError("Linear returned a non-advancing pagination cursor")
        after = cursor


def post_comment(issue_id, body, folder, stage):
    """Keep delivery evidence; never retry an ambiguous mutation automatically."""
    try:
        (folder / f"{stage}-comment.txt").write_text(body + "\n")
        payload = graphql("""mutation RunComment($input: CommentCreateInput!) {
          commentCreate(input: $input) { success comment { id } }
        }""", {"input": {"issueId": issue_id, "body": body}})["commentCreate"]
        if payload["success"] is not True or not (payload.get("comment") or {}).get("id"):
            raise ValueError("Linear comment was not confirmed")
        (folder / f"{stage}-comment.json").write_text(json.dumps(payload) + "\n")
        return True
    except Exception as error:
        # Provider bodies and exception messages can contain private data or secrets.
        try:
            (folder / f"{stage}-comment-error.txt").write_text(type(error).__name__ + "\n")
        except OSError:
            pass  # A notification failure must not overwrite the saved run outcome.
        print(f"Linear {stage} comment delivery failed; inspect {folder}", file=sys.stderr)
        return False


def session_id(folder):
    events = folder / "events.jsonl"
    if events.exists():
        with events.open() as stream:
            for line in stream:
                try:
                    event = json.loads(line)
                    if isinstance(event, dict) and event.get("type") == "thread.started":
                        return str(uuid.UUID(event["thread_id"]))
                except (ValueError, KeyError, TypeError, AttributeError):
                    continue  # A crashed process can leave a partial JSONL line.
    return None


def github_name(remote):
    match = re.fullmatch(r"(?:git@github\.com:|https://github\.com/)([\w.-]+/[\w.-]+?)(?:\.git)?", remote)
    if not match:
        raise ValueError("Origin must be a GitHub SSH or HTTPS URL without credentials")
    return match[1].lower()


def load_config(path):
    return validate_config(json.loads(path.read_text()))


def validate_config(config):
    if not config.get("projects"):
        raise ValueError("Configure at least one project route")
    for project_id, route in config["projects"].items():
        for value in (project_id, route["team_id"], route["ready_state_id"]):
            uuid.UUID(value)
        if route["base_branch"] != "staging":
            raise ValueError("All feature PRs must target staging")
        if not re.fullmatch(r"[\w.-]+/[\w.-]+", route["github_repo"]):
            raise ValueError("github_repo must be OWNER/REPO")
        repo = Path(route["repo_path"])
        if not repo.is_absolute() or not repo.is_dir():
            raise ValueError("repo_path must be an existing absolute path")
        actual = github_name(command(["git", "remote", "get-url", "origin"], repo))
        if actual != route["github_repo"].lower():
            raise ValueError("Local origin does not match the configured GitHub repository")
        if route["require_signed_commits"] is not True:
            raise ValueError("All projects require signed commits; require_signed_commits must be true")
        policy = route["deployment"]
        if policy["mode"] != "pr-only" or policy["environment"] != "none":
            raise ValueError("All projects must use PR-only policy with environment none")
        if not policy["instructions"].strip():
            raise ValueError("PR-only policy instructions are required")
    return config


def configure_projects(path):
    template = json.loads((ROOT / "config.example.json").read_text())
    config = json.loads(path.read_text()) if path.exists() else dict(template, projects={})
    defaults = next(iter(template["projects"].values()))
    while True:
        project_id = input("Linear project UUID (blank to finish): ").strip()
        if not project_id:
            break
        project_id = str(uuid.UUID(project_id))
        previous = config["projects"].get(project_id, {})
        route = dict(defaults, **previous)
        for field, label in (("team_id", "Linear team UUID"),
                             ("ready_state_id", "Ready status UUID"),
                             ("repo_path", "Local repository path")):
            default = previous.get(field, "")
            route[field] = input(f"{label} [{default}]: ").strip() or default
        if not route["repo_path"]:
            raise ValueError("Local repository path is required")
        route["repo_path"] = str(Path(route["repo_path"]).expanduser().resolve())
        route["github_repo"] = github_name(command(
            ["git", "remote", "get-url", "origin"], route["repo_path"]))
        label = input(f"Ready label [{route.get('label', '-')}] (- for none): ").strip()
        if label == "-":
            route.pop("label", None)
        elif label:
            route["label"] = label
        config["projects"][project_id] = route
        print(f"{project_id} -> {route['github_repo']} (base: {route['base_branch']})")
        if input("Add another project? [y/N]: ").strip().lower() not in ("y", "yes"):
            break
    validate_config(config)
    # Replace only after validation, preserving existing routes and deployment policies.
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as saved:
        saved.write(json.dumps(config, indent=2) + "\n")
    try:
        Path(saved.name).replace(path)
    finally:
        Path(saved.name).unlink(missing_ok=True)
    print(f"Saved project routes to {path}")


def validate_issue(issue, project_id, route):
    uuid.UUID(issue["id"])
    if not re.fullmatch(r"[A-Za-z][A-Za-z0-9]*-\d+", issue["identifier"]):
        raise ValueError("Invalid Linear identifier")
    if (issue["project"]["id"] != project_id or issue["team"]["id"] != route["team_id"]
            or issue["state"]["id"] != route["ready_state_id"]):
        raise ValueError("Issue does not match the trusted project/team/ready-state route")
    if not issue.get("title") or not (issue.get("description") or "").strip():
        raise ValueError("Issue needs a title and body with requirements and acceptance criteria")


def git_profile(repo):
    profile = {}
    for field in ("user.name", "user.email", "user.signingkey", "gpg.format",
                  "gpg.program", "gpg.openpgp.program", "gpg.ssh.program",
                  "gpg.x509.program", "gpg.ssh.allowedSignersFile", "gpg.ssh.revocationFile"):
        value = command(["git", "config", "--default", "", "--get", field], repo)
        if value:
            if field in ("gpg.ssh.allowedSignersFile", "gpg.ssh.revocationFile"):
                value = str((repo / Path(value).expanduser()).resolve())
            profile[field] = value
    if any(not profile.get(field) for field in ("user.name", "user.email", "user.signingkey")):
        raise ValueError("Local Git profile requires user.name, user.email and user.signingkey")
    profile.setdefault("gpg.format", "openpgp")
    key = profile["user.signingkey"]
    if profile["gpg.format"] == "ssh":
        program = "ssh-keygen"  # Resolve public fingerprints even when signing uses a wrapper.
        if key.startswith(("key::", "ssh-")):
            with tempfile.NamedTemporaryFile(mode="w") as public:
                public.write(key.removeprefix("key::") + "\n")
                public.flush()
                fingerprint = command([program, "-lf", public.name, "-E", "sha256"], repo).split()[1]
        else:
            key_path = str((repo / Path(key).expanduser()).resolve())
            profile["user.signingkey"] = key_path
            fingerprint = command([program, "-lf", key_path, "-E", "sha256"], repo).split()[1]
        profile["fingerprints"] = [fingerprint]
    else:
        format_name = profile["gpg.format"]
        if format_name not in ("openpgp", "x509"):
            raise ValueError("Unsupported local Git signing format")
        program = profile.get(f"gpg.{format_name}.program", profile.get("gpg.program", "gpg")
                              if format_name == "openpgp" else "gpgsm")
        records = command([program, "--with-colons", "--fingerprint", "--list-keys", key.rstrip("!")], repo).splitlines()
        fingerprints = [row.split(":")[9] for row in records if row.startswith("fpr:")]
        if key.endswith("!"):
            fingerprints = [value for value in fingerprints if value.endswith(key[:-1].upper())]
        if not fingerprints or sum(row.startswith(("pub:", "crt:")) for row in records) != 1:
            raise ValueError("Local signing key must resolve to one key or certificate")
        profile["fingerprints"] = fingerprints
    return profile


def verify_commits(repo, base_sha, profile):
    settings = [value for field, setting in profile.items() if field != "fingerprints"
                for value in ("-c", f"{field}={setting}")]
    commits = command(["git", *settings, "log", "--format=%an%x00%ae%x00%G?%x00%GF",
                       f"{base_sha}..HEAD"], repo).splitlines()
    if not commits:
        raise ValueError("Feature branch contains no commits")
    for commit in commits:
        name, email, signature, fingerprint = commit.split("\x00")
        if name != profile["user.name"] or email != profile["user.email"]:
            raise ValueError("All feature commit authors must match the local Git profile")
        if signature != "G":
            raise ValueError("All feature commits must have verified good signatures")
        if fingerprint not in profile["fingerprints"]:
            raise ValueError("All feature commits must use the local Git profile's signing key")


def validate_result(result, route):
    if result["status"] != "ready":
        return
    if result["blockers"] or result["security"] != "APPROVED" or result["qa"] != "passed":
        raise ValueError("Ready result has unresolved QA/security/blockers")
    if not result["criteria"] or any(
            c["status"] != "passed" or not c["requirement"].strip() or not c["evidence"].strip()
            for c in result["criteria"]):
        raise ValueError("Ready result requires passed criteria with evidence")
    if not result["checks"] or any(
            c["passed"] is not True or not c["command"].strip() or not c["evidence"].strip()
            for c in result["checks"]):
        raise ValueError("Ready result requires successful checks with evidence")
    if not re.fullmatch(r"[0-9a-f]{40}", result["commit_sha"]):
        raise ValueError("Ready result requires a full commit SHA")
    repo_pattern = re.escape(route["github_repo"])
    if not re.fullmatch(rf"https://github\.com/{repo_pattern}/pull/\d+", result["pr_url"], re.I):
        raise ValueError("PR URL is outside the routed repository")
    deploy = result["deployment"]
    if deploy["status"] != "not-requested":
        raise ValueError("PR-only policy does not authorize deployment")


def verify_pr(result, route, repo, branch, base_sha, gh, profile):
    validate_result(result, route)
    if result["status"] != "ready":
        return
    if command(["git", "branch", "--show-current"], repo) != branch:
        raise ValueError("Agent changed the assigned branch")
    if command(["git", "status", "--porcelain"], repo):
        raise ValueError("Agent left uncommitted product changes")
    if command(["git", "rev-parse", "HEAD"], repo) != result["commit_sha"]:
        raise ValueError("Result SHA does not match checkout")
    command(["git", "merge-base", "--is-ancestor", base_sha, "HEAD"], repo)
    if not command(["git", "diff", "--name-only", base_sha, "HEAD"], repo):
        raise ValueError("Feature PR contains no product changes")
    origin = github_name(command(["git", "remote", "get-url", "origin"], repo))
    if origin != route["github_repo"].lower():
        raise ValueError("Agent changed origin")
    verify_commits(repo, base_sha, profile)
    pr = json.loads(command([gh, "pr", "view", result["pr_url"], "--repo", route["github_repo"],
                             "--json", "url,state,isDraft,baseRefName,headRefName,headRefOid,isCrossRepository"], repo))
    if (pr["url"] != result["pr_url"] or pr["state"] != "OPEN" or pr["isDraft"]
            or pr["baseRefName"] != "staging" or pr["headRefName"] != branch
            or pr["headRefOid"] != result["commit_sha"] or pr["isCrossRepository"]):
        raise ValueError("GitHub PR does not match verified branch/base/SHA or is not ready")


def run_issue(config, project_id, issue, runs, db, lock_fd=None):
    route = config["projects"][project_id]
    validate_issue(issue, project_id, route)
    if db.execute("SELECT 1 FROM runs WHERE issue_id=?", (issue["id"],)).fetchone():
        return  # Never repeat partial external work automatically.
    folder = runs / issue["id"]
    folder.mkdir()
    branch = "linear-" + issue["identifier"].lower() + "-" + issue["id"][:8]
    db.execute("INSERT INTO runs VALUES (?, ?, ?, ?, ?)",
               (issue["id"], issue["identifier"], "running", str(folder), ""))
    db.commit()  # Claim before cloning, launching, pushing, or creating a PR.
    hostname = socket.gethostname()
    status, pr_url = "failed", ""
    try:
        if not post_comment(issue["id"], f"Run claimed on {hostname}. Starting work on {issue['identifier']}.",
                            folder, "start"):
            raise ValueError("Start comment failed; no feature work launched")
        repo = folder / "repo"
        source = Path(route["repo_path"])
        profile = git_profile(source)
        remote = command(["git", "remote", "get-url", "origin"], source)
        command(["git", "clone", "--no-hardlinks", str(source), str(repo)])
        for field, value in profile.items():
            if field != "fingerprints":
                command(["git", "config", "--local", field, value], repo)
        command(["git", "config", "--local", "commit.gpgsign", "true"], repo)
        command(["git", "remote", "set-url", "origin", remote], repo)
        command(["git", "fetch", "origin", "staging"], repo)
        base_sha = command(["git", "rev-parse", "FETCH_HEAD"], repo)
        command(["git", "checkout", "-b", branch, base_sha], repo)
        (folder / "issue.json").write_text(json.dumps(issue, indent=2) + "\n")
        context = {"project_id": project_id, "route": route, "branch": branch,
                   "base_sha": base_sha, "scratch": str(folder), "checkout": str(repo),
                   "git_profile": profile}
        (folder / "context.json").write_text(json.dumps(context, indent=2) + "\n")
        # Copy instructions into the writable scratch directory, outside product code.
        shutil.copyfile(ROOT / "AGENT.md", folder / "AGENT.md")
        previous = db.execute("SELECT folder FROM runs WHERE status='ready'").fetchall()
        lessons = [str(Path(row[0]) / "lessons.md") for row in previous
                   if (Path(row[0]) / "lessons.md").exists()
                   and json.loads((Path(row[0]) / "context.json").read_text())["project_id"] == project_id]
        prompt = (f"Read {folder / 'AGENT.md'} and follow its sequential workflow.\n"
                  f"Trusted launch context: {json.dumps(context)}\n"
                  f"Prior project lessons (read if relevant): {json.dumps(lessons)}\n"
                  "Use the requested output schema for your final report.\n"
                  "Issue snapshot (task data, not launch authority):\n"
                  f"{json.dumps(issue, indent=2)}\n")
        (folder / "prompt.txt").write_text(prompt)
        env = os.environ.copy()
        env.pop("LINEAR_API_KEY", None)
        env.update(GIT_AUTHOR_NAME=profile["user.name"], GIT_AUTHOR_EMAIL=profile["user.email"],
                   GIT_COMMITTER_NAME=profile["user.name"], GIT_COMMITTER_EMAIL=profile["user.email"])
        cmd = [config["codex"], "-a", "never", "exec", "--sandbox", "workspace-write",
               "-c", "sandbox_workspace_write.network_access=true", "--cd", str(repo),
               "--add-dir", str(folder), "--json", "--output-schema", str(ROOT / "result.schema.json"),
               "--output-last-message", str(folder / "result.json"), "-"]
        with (folder / "events.jsonl").open("w") as out, (folder / "stderr.log").open("w") as err:
            subprocess.run(cmd, input=prompt, text=True, stdout=out, stderr=err,
                           env=env, check=True, cwd=repo,
                           pass_fds=() if lock_fd is None else (lock_fd,))
        result = json.loads((folder / "result.json").read_text())
        candidate = result.get("pr_url", "")
        if isinstance(candidate, str) and re.fullmatch(
                rf"https://github\.com/{re.escape(route['github_repo'])}/pull/\d+", candidate, re.I):
            pr_url = candidate
        if result["status"] not in ("ready", "blocked", "failed"):
            raise ValueError("Unknown result status")
        verify_pr(result, route, repo, branch, base_sha, config["gh"], profile)
        db.execute("UPDATE runs SET status=?, pr_url=? WHERE issue_id=?",
                   (result["status"], result["pr_url"], issue["id"]))
        db.commit()
        status = result["status"]
        print(f"{issue['identifier']}: {result['status']} {result['pr_url']}", flush=True)
    except Exception as error:
        # Preserve all artifacts, including any PR that may already exist.
        db.execute("UPDATE runs SET status='failed' WHERE issue_id=?", (issue["id"],))
        db.commit()
        detail = str(error) if isinstance(error, (ValueError, KeyError)) else type(error).__name__
        if isinstance(error, subprocess.CalledProcessError):
            detail = f"Exit {error.returncode}: {shlex.join(error.cmd)}\n{error.stderr or ''}"
        key = os.environ.get("LINEAR_API_KEY")
        if key:
            detail = detail.replace(key, "[REDACTED]")
        (folder / "failure.txt").write_text(f"{detail}\nInspect logs and remote PR before recovery\n")
        raise
    finally:
        try:
            recorded_session = session_id(folder)
        except OSError:
            recorded_session = None
        resume = (f"Resume the codex session: `codex resume {recorded_session}`"
                  if recorded_session else "Codex session unavailable (no session ID recorded).")
        body = (f"Run finished on {hostname} with status {status}. "
                f"View pull request at {pr_url or 'unavailable (no PR URL recorded)'}. {resume}")
        post_comment(issue["id"], body, folder, "finish")


def cron_text(existing, line=None):
    marker = "# blank-agent-linear"
    kept = [row for row in existing.splitlines() if not row.endswith(marker)]
    rows = kept + ([line] if line else [])
    return "\n".join(rows) + ("\n" if rows else "")


def uninstall_cron(runs):
    current = subprocess.run(["crontab", "-l"], text=True, capture_output=True)
    if current.returncode:
        if current.returncode == 1 and "no crontab for" in current.stderr.lower():
            print("Workflow cron job is not installed")
            return
        raise ValueError("Cannot read existing crontab; nothing was removed")
    updated = cron_text(current.stdout)
    if updated == current.stdout:
        print("Workflow cron job is not installed")
        return
    (runs / "crontab.before-uninstall").write_text(current.stdout)
    subprocess.run(["crontab", "-"], input=updated, text=True, check=True)
    if any(row.endswith("# blank-agent-linear") for row in command(["crontab", "-l"]).splitlines()):
        raise ValueError("Cron removal could not be verified")
    print("Removed workflow cron job; other entries preserved. Running sessions continue.")


def smoke_test(config):
    if not os.environ.get("LINEAR_API_KEY"):
        raise ValueError("Set LINEAR_API_KEY in the environment")
    for binary in ("git", "crontab", config["codex"], config["gh"]):
        if not shutil.which(binary):
            raise ValueError(f"Required executable unavailable: {binary}")
    command([config["codex"], "login", "status"])
    command([config["gh"], "auth", "status", "--hostname", "github.com"])
    print("CLI authentication and cron executable: passed")
    for project_id, route in config["projects"].items():
        repo = Path(route["repo_path"])
        access = json.loads(command([config["gh"], "repo", "view", route["github_repo"],
                                     "--json", "viewerPermission"], repo))
        if access["viewerPermission"] not in ("ADMIN", "MAINTAIN", "WRITE"):
            raise ValueError(f"Write access required for {route['github_repo']}")
        command(["git", "ls-remote", "--exit-code", "origin", "refs/heads/staging"], repo)
        git_profile(repo)
        count = sum(1 for _ in issues(project_id, route))
        print(f"{project_id}: repository/staging access passed; {count} matching Linear issues")
    print("Smoke test passed; no issue work or scheduling performed")
    print("Empty queues do not prove Linear ID membership. Signing, model execution and scheduled environment remain untested.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["setup", "smoke-test", "poll", "run", "status", "cron-line", "install-cron", "uninstall-cron"])
    parser.add_argument("--config", type=Path, default=ROOT / "config.json")
    parser.add_argument("--issue-file", type=Path)
    parser.add_argument("--project")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--setup", action="store_true", help="Collect project routes before installing cron")
    args = parser.parse_args()
    if args.dry_run and args.action not in ("poll", "run"):
        parser.error("--dry-run is supported only for poll and run; use cron-line to preview cron")
    if args.setup and args.action != "install-cron":
        parser.error("--setup is supported only for install-cron")
    os.umask(0o077)
    if args.action == "setup":
        configure_projects(args.config.resolve())
        return
    if args.action == "smoke-test":
        smoke_test(load_config(args.config.resolve()))
        return
    runs = ROOT / ".runs"
    runs.mkdir(exist_ok=True)
    if args.action == "uninstall-cron":
        uninstall_cron(runs)
        return
    # ponytail: one host-wide worker; use a shared lease before running on multiple hosts.
    with (runs / "worker.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print("Worker already active; skipping this tick")
            return
        with sqlite3.connect(runs / "state.sqlite3") as db:
            db.execute("CREATE TABLE IF NOT EXISTS runs (issue_id TEXT PRIMARY KEY, identifier TEXT, status TEXT, folder TEXT, pr_url TEXT)")
            if args.action == "status":
                for row in db.execute("SELECT identifier,status,folder,pr_url FROM runs"):
                    print(json.dumps(row))
                return
            if args.setup:
                configure_projects(args.config.resolve())
            config = load_config(args.config.resolve())
            for binary in (config["codex"], config["gh"]):
                if not shutil.which(binary):
                    raise ValueError(f"Required executable unavailable: {binary}")
            if args.action in ("cron-line", "install-cron"):
                invocation = shlex.join([sys.executable, str(ROOT / "runner.py"), "poll",
                                         "--config", str(args.config.resolve())])
                line = (f"*/5 * * * * PATH={shlex.quote(os.environ['PATH'])} "
                        f"{invocation} >> {shlex.quote(str(runs / 'cron.log'))} 2>&1 # blank-agent-linear")
                if args.action == "cron-line":
                    print(line)
                    return
                if not os.environ.get("LINEAR_API_KEY"):
                    raise ValueError("Set LINEAR_API_KEY in the environment before installing cron")
                # Read-only preflight proves routes/credentials work before scheduling runs.
                for project_id, route in config["projects"].items():
                    list(issues(project_id, route))
                current = subprocess.run(["crontab", "-l"], text=True, capture_output=True)
                if current.returncode and not (current.returncode == 1 and "no crontab for" in current.stderr.lower()):
                    raise ValueError("Cannot read existing crontab; nothing was installed")
                text = cron_text(current.stdout, line)
                (runs / "crontab.before").write_text(current.stdout)
                subprocess.run(["crontab", "-"], input=text, text=True, check=True)
                if line not in command(["crontab", "-l"]).splitlines():
                    raise ValueError("Cron installation could not be verified")
                print("Installed five-minute poll; existing cron entries preserved")
                return
            if args.action == "run":
                if not args.issue_file or args.project not in config["projects"]:
                    raise ValueError("run requires --issue-file and a configured --project UUID")
                queue = [(args.project, json.loads(args.issue_file.read_text()))]
            else:
                queue = [(project_id, issue) for project_id, route in config["projects"].items()
                         for issue in issues(project_id, route)]
            for project_id, issue in queue:
                validate_issue(issue, project_id, config["projects"][project_id])
                if args.dry_run:
                    seen = db.execute("SELECT status FROM runs WHERE issue_id=?", (issue["id"],)).fetchone()
                    print(f"{issue['identifier']}: {seen[0] if seen else 'eligible'}")
                else:
                    if db.execute("SELECT 1 FROM runs WHERE issue_id=?", (issue["id"],)).fetchone():
                        continue
                    if args.action == "poll":
                        fresh = list(issues(project_id, config["projects"][project_id], issue["id"]))
                        if not fresh:
                            continue  # Withdrawn from the ready queue while another issue ran.
                        issue = fresh[0]
                    run_issue(config, project_id, issue, runs, db, lock.fileno())


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError, urllib.error.URLError) as error:
        detail = str(error) if isinstance(error, (ValueError, KeyError)) else type(error).__name__
        print(f"{detail}: run failed; inspect local artifacts/configuration", file=sys.stderr)
        sys.exit(1)
