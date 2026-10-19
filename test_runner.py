import copy
import json
import os
from pathlib import Path
import sqlite3
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import runner

PROJECT = "11111111-1111-4111-8111-111111111111"
ISSUE = "22222222-2222-4222-8222-222222222222"
ROUTE = {
    "team_id": "33333333-3333-4333-8333-333333333333",
    "ready_state_id": "44444444-4444-4444-8444-444444444444",
    "github_repo": "acme/widget", "base_branch": "staging",
    "require_signed_commits": True,
    "deployment": {"mode": "pr-only", "environment": "none", "instructions": "PR only"},
}
STATES = {"In Progress": "66666666-6666-4666-8666-666666666666",
          "In Review": "77777777-7777-4777-8777-777777777777"}
USER = "88888888-8888-4888-8888-888888888888"


def linear_response(query, variables):
    if "viewer" in query:
        return {"viewer": {"id": USER}}
    if "workflowStates(" in query:
        return {"workflowStates": {"nodes": [{"id": value, "name": name} for name, value in STATES.items()],
                                   "pageInfo": {"hasNextPage": False}}}
    if "issueUpdate(" in query:
        return {"issueUpdate": {"success": True, "issue": {"id": variables["id"],
                                "state": {"id": variables["input"]["stateId"]},
                                "assignee": {"id": variables["input"].get("assigneeId", USER)}}}}
    return {"commentCreate": {"success": True, "comment": {"id": ISSUE}}}


def result(sha="a" * 40):
    return {
        "status": "ready", "summary": "Handle empty input",
        "pr_url": "https://github.com/acme/widget/pull/12", "commit_sha": sha,
        "criteria": [{"requirement": "Reject empty input", "status": "passed", "evidence": "test passed"}],
        "checks": [{"command": "python3 check.py", "passed": True, "evidence": "exit 0"}],
        "security": "APPROVED", "qa": "passed", "blockers": [],
        "deployment": {"status": "not-requested", "environment": "none", "sha": "", "url": "", "evidence": "PR only"},
    }


class RunnerTest(unittest.TestCase):
    def test_git_profile_resolves_keys_and_rejects_missing_profile(self):
        fingerprint = "A" * 40
        for format_name in ("openpgp", "x509"):
            values = {"user.name": "Test", "user.email": "test@example.com",
                      "user.signingkey": fingerprint, "gpg.format": format_name}
            record = "pub" if format_name == "openpgp" else "crt"
            def commands(args, cwd=None):
                if args[0] == "git":
                    return values.get(args[-1], "")
                return record + ":\nfpr:::::::::" + fingerprint + ":"
            with self.subTest(format=format_name), patch.object(runner, "command", side_effect=commands):
                self.assertEqual(runner.git_profile(Path("/tmp"))["fingerprints"], [fingerprint])
        with patch.object(runner, "command", return_value=""):
            with self.assertRaisesRegex(ValueError, "Local Git profile requires"):
                runner.git_profile(Path("/tmp"))

    def test_commits_require_profile_author_and_signing_key(self):
        profile = {"user.name": "Test", "user.email": "test@example.com", "fingerprints": ["SHA256:local"]}
        valid = "Test\x00test@example.com\x00G\x00SHA256:local"
        with patch.object(runner, "command", return_value=valid):
            runner.verify_commits(Path("/tmp"), "a" * 40, profile)
        for invalid in ("Other\x00test@example.com\x00G\x00SHA256:local",
                        "Test\x00other@example.com\x00G\x00SHA256:local",
                        "Test\x00test@example.com\x00N\x00",
                        "Test\x00test@example.com\x00G\x00SHA256:other",
                        valid + "\nOther\x00other@example.com\x00G\x00SHA256:local"):
            with self.subTest(commit=invalid), patch.object(runner, "command", return_value=invalid):
                with self.assertRaises(ValueError):
                    runner.verify_commits(Path("/tmp"), "a" * 40, profile)

    def test_smoke_test_checks_access_without_issue_work_or_scheduling(self):
        for engine in ("codex", "opencode"):
            with tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                repo = root / "repo"
                repo.mkdir()
                config = {"codex": "codex", "gh": "gh", "projects": {PROJECT: dict(ROUTE, repo_path=str(repo))}}
                if engine == "opencode":
                    config.update(engine=engine, opencode="opencode", model="test/model")
                path = root / "config.json"
                original = json.dumps(config)
                path.write_text(original)
                reads = {
                    ("git", "remote", "get-url", "origin"): "https://github.com/acme/widget.git",
                    ("codex", "login", "status"): "Logged in",
                    ("gh", "auth", "status", "--hostname", "github.com"): "Logged in",
                    ("gh", "repo", "view", "acme/widget", "--json", "viewerPermission"): '{"viewerPermission":"WRITE"}',
                    ("git", "ls-remote", "--exit-code", "origin", "refs/heads/staging"): "a" * 40 + "\trefs/heads/staging",
                }
                if engine == "opencode":
                    del reads[("codex", "login", "status")]
                    reads[("opencode", "auth", "list")] = "test provider"
                    reads[("opencode", "models")] = "test/model"
                with patch.object(runner, "ROOT", root), \
                        patch.object(runner.sys, "argv", ["runner.py", "smoke-test"]), \
                        patch.dict(os.environ, {"LINEAR_API_KEY": "test-secret"}), \
                        patch.object(runner.shutil, "which", return_value="/test/bin"), \
                        patch.object(runner, "command", side_effect=lambda args, cwd=None: reads[tuple(args)]) as commands, \
                        patch.object(runner, "git_profile", return_value={}) as profile, \
                        patch.object(runner, "issues", return_value=[{"id": ISSUE}]) as api, \
                        patch.object(runner, "run_issue") as launch, \
                        patch.object(runner.sqlite3, "connect") as database, \
                        patch.object(runner.subprocess, "run") as processes:
                    runner.main()
                    launch.assert_not_called()
                    database.assert_not_called()
                    processes.assert_not_called()
                    api.assert_called_once_with(PROJECT, config["projects"][PROJECT])
                    profile.assert_called_once_with(repo)
                    self.assertEqual(commands.call_count, len(reads))
                self.assertFalse((root / ".runs").exists())
                self.assertEqual(path.read_text(), original)
                self.assertEqual(list(repo.iterdir()), [])

    def test_smoke_test_rejects_missing_key_tools_and_read_only_access(self):
        config = {"codex": "codex", "gh": "gh", "projects": {PROJECT: dict(ROUTE, repo_path="/tmp")}}
        with patch.dict(os.environ, {}, clear=True), patch.object(runner, "command") as commands:
            with self.assertRaisesRegex(ValueError, "LINEAR_API_KEY"):
                runner.smoke_test(config)
            commands.assert_not_called()
        with patch.dict(os.environ, {"LINEAR_API_KEY": "test-secret"}), \
                patch.object(runner.shutil, "which", return_value=None):
            with self.assertRaisesRegex(ValueError, "executable unavailable"):
                runner.smoke_test(config)
        with patch.dict(os.environ, {"LINEAR_API_KEY": "test-secret"}), \
                patch.object(runner.shutil, "which", return_value="/test/bin"), \
                patch.object(runner, "command", side_effect=["", "", '{"viewerPermission":"READ"}']), \
                patch.object(runner, "issues") as api:
            with self.assertRaisesRegex(ValueError, "Write access required"):
                runner.smoke_test(config)
            api.assert_not_called()

    def test_setup_finishes_after_project_unless_another_is_requested(self):
        second = "66666666-6666-4666-8666-666666666666"
        for another in ("", "n", "y"):
            with self.subTest(another=another), tempfile.TemporaryDirectory() as temp:
                path = Path(temp) / "config.json"
                answers = [PROJECT, ROUTE["team_id"], ROUTE["ready_state_id"], temp, "-", another]
                if another == "y":
                    answers += [second, ROUTE["team_id"], ROUTE["ready_state_id"], temp, "-", "n"]
                with patch("builtins.input", side_effect=answers) as prompts, \
                        patch.object(runner, "command", return_value="https://github.com/acme/widget.git"):
                    runner.configure_projects(path)
                saved = json.loads(path.read_text())
                self.assertEqual(set(saved["projects"]), {PROJECT, second} if another == "y" else {PROJECT})
                self.assertEqual(prompts.call_args.args[0], "Add another project? [y/N]: ")

    def test_setup_action_only_collects_routes(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(runner, "ROOT", Path(temp)), \
                patch.object(runner.sys, "argv", ["runner.py", "setup"]), \
                patch.object(runner, "configure_projects") as configure, \
                patch.object(runner.subprocess, "run") as processes:
            runner.main()
            configure.assert_called_once_with(Path(temp) / "config.json")
            processes.assert_not_called()
            self.assertFalse((Path(temp) / ".runs").exists())

    def test_config_allows_only_pr_only_policy(self):
        with tempfile.TemporaryDirectory() as temp, \
                patch.object(runner, "command", return_value="https://github.com/acme/widget.git"):
            config = {"projects": {PROJECT: dict(ROUTE, repo_path=temp)}}
            runner.validate_config(config)
            for mode, environment in (("pr-preview", "staging"), ("direct", "production"),
                                      ("pr-only", "staging")):
                with self.subTest(mode=mode, environment=environment):
                    invalid = copy.deepcopy(config)
                    invalid["projects"][PROJECT]["deployment"].update(mode=mode, environment=environment)
                    with self.assertRaisesRegex(ValueError, "PR-only"):
                        runner.validate_config(invalid)
            invalid = copy.deepcopy(config)
            invalid["projects"][PROJECT]["require_signed_commits"] = False
            with self.assertRaisesRegex(ValueError, "require signed commits"):
                runner.validate_config(invalid)

    def test_setup_installs_one_cron_job_without_launching_issue_sessions(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "config.example.json").write_text((runner.ROOT / "config.example.json").read_text())
            repo = root / "repo"
            repo.mkdir()
            installed = []
            answers = [PROJECT, ROUTE["team_id"], ROUTE["ready_state_id"], str(repo), "", ""]

            def processes(args, **kwargs):
                if args == ["crontab", "-l"]:
                    return subprocess.CompletedProcess(args, 0, stdout="0 0 * * * backup\n", stderr="")
                self.assertEqual(args, ["crontab", "-"])
                installed.append(kwargs["input"])
                return subprocess.CompletedProcess(args, 0)

            def commands(args, cwd=None):
                if args == ["crontab", "-l"]:
                    return installed[0].strip()
                self.assertEqual(args, ["git", "remote", "get-url", "origin"])
                return "https://github.com/acme/widget.git"

            with patch.object(runner, "ROOT", root), patch("builtins.input", side_effect=answers), \
                    patch.object(runner.sys, "argv", ["runner.py", "install-cron", "--setup"]), \
                    patch.dict(os.environ, {"LINEAR_API_KEY": "test-secret"}), \
                    patch.object(runner.shutil, "which", return_value="/test/bin"), \
                    patch.object(runner, "command", side_effect=commands), \
                    patch.object(runner.subprocess, "run", side_effect=processes), \
                    patch.object(runner, "issues", return_value=[]), \
                    patch.object(runner, "run_issue") as launch:
                runner.main()
                launch.assert_not_called()
            self.assertEqual(len(installed), 1)
            self.assertIn("0 0 * * * backup\n", installed[0])
            self.assertEqual(installed[0].count("# blank-agent-linear"), 1)
            self.assertIn("*/5 * * * *", installed[0])
            self.assertNotIn("test-secret", installed[0])
            self.assertNotIn("test-secret", (root / "config.json").read_text())

    def test_setup_adds_projects_and_preserves_existing_policies(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            old_repo, new_repo = root / "old", root / "new"
            old_repo.mkdir()
            new_repo.mkdir()
            policy = {"mode": "pr-only", "environment": "none", "instructions": "Create PR; owner handles release"}
            old_route = dict(ROUTE, repo_path=str(old_repo), deployment=policy)
            config = {"codex": "codex", "gh": "gh", "projects": {PROJECT: old_route}}
            path = root / "config.json"
            path.write_text(json.dumps(config))
            new_project = "66666666-6666-4666-8666-666666666666"
            answers = [new_project, ROUTE["team_id"], ROUTE["ready_state_id"], str(new_repo), "-", ""]
            with patch("builtins.input", side_effect=answers), \
                    patch.object(runner, "command", return_value="https://github.com/acme/widget.git"):
                runner.configure_projects(path)
            saved = json.loads(path.read_text())
            self.assertEqual(saved["projects"][PROJECT], old_route)
            added = saved["projects"][new_project]
            self.assertEqual(added["repo_path"], str(new_repo))
            self.assertEqual(added["github_repo"], "acme/widget")
            self.assertEqual(added["base_branch"], "staging")
            self.assertEqual(added["deployment"]["mode"], "pr-only")
            self.assertTrue(added["require_signed_commits"])
            self.assertNotIn("label", added)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_invalid_setup_does_not_replace_existing_config(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "config.json"
            original = json.dumps({"codex": "codex", "gh": "gh", "projects": {PROJECT: dict(ROUTE, repo_path=temp)}})
            path.write_text(original)
            answers = [PROJECT, "invalid-team-id", "", "", "", ""]
            with patch("builtins.input", side_effect=answers), \
                    patch.object(runner, "command", return_value="https://github.com/acme/widget.git"):
                with self.assertRaises(ValueError):
                    runner.configure_projects(path)
            self.assertEqual(path.read_text(), original)

    def test_poll_requires_environment_key_and_does_not_load_dotenv(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / ".env").write_text("LINEAR_API_KEY=file-key\n")
            config = {"codex": "codex", "gh": "gh", "projects": {PROJECT: ROUTE}}
            with patch.object(runner, "ROOT", root), \
                    patch.dict(os.environ, {}, clear=True), \
                    patch.object(runner, "load_config", return_value=config), \
                    patch.object(runner.shutil, "which", return_value="/test/bin"), \
                    patch.object(runner.sys, "argv", ["runner.py", "poll", "--dry-run"]), \
                    patch.object(runner.urllib.request, "urlopen") as http:
                http.return_value.__enter__.return_value.read.return_value = json.dumps(
                    {"data": {"issues": {"nodes": [], "pageInfo": {"hasNextPage": False}}}}).encode()
                with self.assertRaisesRegex(ValueError, "Set LINEAR_API_KEY in the environment"):
                    runner.main()
                http.assert_not_called()
                self.assertNotIn("LINEAR_API_KEY", os.environ)

    def test_dry_run_cannot_install_cron(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(runner, "ROOT", Path(temp)), \
                patch.object(runner.sys, "argv", ["runner.py", "install-cron", "--dry-run"]), \
                patch.object(runner.sys, "stderr"), \
                patch.object(runner, "load_config", side_effect=ValueError("must not reach configuration")):
            with self.assertRaises(SystemExit) as error:
                runner.main()
            self.assertEqual(error.exception.code, 2)

    def test_startup_failure_records_command_and_error_without_linear_key(self):
        with tempfile.TemporaryDirectory() as temp:
            runs = Path(temp)
            with sqlite3.connect(runs / "state.sqlite3") as db:
                db.execute("CREATE TABLE runs (issue_id TEXT PRIMARY KEY, identifier TEXT, status TEXT, folder TEXT, pr_url TEXT)")
                issue = {"id": ISSUE, "identifier": "APP-1", "title": "Handle input", "description": "Reject empty input",
                         "project": {"id": PROJECT}, "team": {"id": ROUTE["team_id"]},
                         "state": {"id": ROUTE["ready_state_id"]}}
                config = {"projects": {PROJECT: dict(ROUTE, repo_path=temp)}}
                error = subprocess.CalledProcessError(128, ["git", "fetch", "origin", "staging"],
                                                     stderr="missing staging; test-secret")
                with patch.object(runner, "command", side_effect=error), \
                        patch.object(runner, "graphql", side_effect=linear_response), \
                        patch.dict(os.environ, {"LINEAR_API_KEY": "test-secret"}):
                    with self.assertRaises(subprocess.CalledProcessError):
                        runner.run_issue(config, PROJECT, issue, runs, db)
                evidence = (runs / ISSUE / "failure.txt").read_text()
                self.assertIn("git fetch origin staging", evidence)
                self.assertIn("missing staging", evidence)
                self.assertNotIn("test-secret", evidence)

    def test_poll_does_not_refresh_or_launch_previously_claimed_issues(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            runs = root / ".runs"
            runs.mkdir()
            with sqlite3.connect(runs / "state.sqlite3") as db:
                db.execute("CREATE TABLE runs (issue_id TEXT PRIMARY KEY, identifier TEXT, status TEXT, folder TEXT, pr_url TEXT)")
                db.execute("INSERT INTO runs VALUES (?, 'APP-1', 'failed', '', '')", (ISSUE,))
            issue = {"id": ISSUE, "identifier": "APP-1", "title": "Handle input", "description": "Reject empty input",
                     "project": {"id": PROJECT}, "team": {"id": ROUTE["team_id"]},
                     "state": {"id": ROUTE["ready_state_id"]}}
            config = {"codex": "codex", "gh": "gh", "projects": {PROJECT: ROUTE}}
            with patch.object(runner, "ROOT", root), patch.object(runner, "load_config", return_value=config), \
                    patch.object(runner, "issues", return_value=[issue]) as api, \
                    patch.object(runner, "run_issue") as launch, \
                    patch.object(runner.sys, "argv", ["runner.py", "poll"]):
                runner.main()
                self.assertEqual(api.call_count, 1)
                launch.assert_not_called()

    def test_pagination_and_server_side_route(self):
        pages = [
            {"issues": {"nodes": [{"id": "first"}], "pageInfo": {"hasNextPage": True, "endCursor": "next"}}},
            {"issues": {"nodes": [{"id": "second"}], "pageInfo": {"hasNextPage": False, "endCursor": "last"}}},
        ]
        with patch.object(runner, "graphql", side_effect=pages) as api:
            self.assertEqual([i["id"] for i in runner.issues(PROJECT, ROUTE)], ["first", "second"])
            self.assertEqual(api.call_args.args[1]["after"], "next")
            filters = api.call_args.args[1]["filter"]
            self.assertEqual(filters["project"]["id"]["eq"], PROJECT)
            self.assertEqual(filters["state"]["id"]["eq"], ROUTE["ready_state_id"])

    def test_graphql_errors_are_not_partial_success_or_secret_output(self):
        with patch.dict(os.environ, {"LINEAR_API_KEY": "private-test-key"}):
            with patch.object(runner.urllib.request, "urlopen") as http:
                http.return_value.__enter__.return_value.read.return_value = json.dumps(
                    {"data": {"issues": {}}, "errors": [{"message": "private payload"}]}).encode()
                with self.assertRaisesRegex(ValueError, "Linear GraphQL failed") as error:
                    runner.graphql("query {}", {})
                self.assertNotIn("private", str(error.exception))
                self.assertEqual(http.call_args.args[0].get_header("Authorization"), "private-test-key")

    def test_release_gates(self):
        runner.validate_result(result(), ROUTE)
        mutations = [
            ("criteria", []), ("checks", []), ("security", "CONDITIONAL"),
            ("qa", "unverified"), ("blockers", ["need access"]),
            ("pr_url", "https://github.com/other/repo/pull/1"),
        ]
        for field, value in mutations:
            with self.subTest(field=field):
                report = result()
                report[field] = value
                with self.assertRaises(ValueError):
                    runner.validate_result(report, ROUTE)
        for field, value in [("status", "unverified"), ("evidence", "")]:
            report = result()
            report["criteria"][0][field] = value
            with self.assertRaises(ValueError):
                runner.validate_result(report, ROUTE)
        report = result()
        report["deployment"] = {"status": "verified", "environment": "staging", "sha": "a" * 40,
                                "url": "https://preview.example", "evidence": "health + SHA + behavior"}
        with self.assertRaisesRegex(ValueError, "PR-only"):
            runner.validate_result(report, ROUTE)

    def test_run_comments_report_outcome_session_and_delivery_failures(self):
        session = "77777777-7777-4777-8777-777777777777"
        for outcome in ("ready", "blocked", "failed", "process-error", "start-error", "finish-error",
                        "start-state-error", "review-state-error", "verification-error", "missing-state",
                        "viewer-error", "assignment-error"):
            with self.subTest(outcome=outcome), tempfile.TemporaryDirectory() as temp:
                runs = Path(temp)
                db = sqlite3.connect(":memory:")
                self.addCleanup(db.close)
                db.execute("CREATE TABLE runs (issue_id TEXT PRIMARY KEY, identifier TEXT, status TEXT, folder TEXT, pr_url TEXT)")
                issue = {"id": ISSUE, "identifier": "APP-1", "title": "Handle input", "description": "Reject empty input",
                         "project": {"id": PROJECT}, "team": {"id": ROUTE["team_id"]},
                         "state": {"id": ROUTE["ready_state_id"]}}
                config = {"codex": "test-codex", "gh": "test-gh", "projects": {PROJECT: dict(ROUTE, repo_path=temp)}}
                launched, comments, transitions = [], [], []

                def api(query, variables):
                    if "viewer" in query:
                        if outcome == "viewer-error":
                            return {"viewer": {"id": ""}}
                        return linear_response(query, variables)
                    if "workflowStates(" in query:
                        self.assertEqual(variables["team"], ROUTE["team_id"])
                        response = linear_response(query, variables)
                        if outcome == "missing-state":
                            response["workflowStates"]["nodes"].pop()
                        return response
                    if "issueUpdate(" in query:
                        self.assertEqual(variables["id"], ISSUE)
                        state = variables["input"]["stateId"]
                        transitions.append(state)
                        if state == STATES["In Progress"]:
                            self.assertFalse(launched)
                            self.assertEqual(variables["input"]["assigneeId"], USER)
                        else:
                            self.assertTrue(launched)
                            self.assertNotIn("assigneeId", variables["input"])
                            verification.assert_called_once()
                        if outcome == "start-state-error" or outcome == "review-state-error" and state == STATES["In Review"]:
                            raise runner.urllib.error.URLError("test-secret")
                        response = linear_response(query, variables)
                        if outcome == "assignment-error":
                            response["issueUpdate"]["issue"]["assignee"] = None
                        return response
                    self.assertIn("commentCreate(input: $input)", query)
                    self.assertEqual(variables["input"]["issueId"], ISSUE)
                    self.assertNotIn("test-secret", variables["input"]["body"])
                    comments.append(variables["input"]["body"])
                    if len(comments) == 1 and outcome not in ("missing-state", "viewer-error"):
                        self.assertEqual(db.execute("SELECT status FROM runs").fetchone()[0], "running")
                        self.assertFalse(launched)
                    if outcome == "start-error" and len(comments) == 1:
                        return {"commentCreate": {"success": False}}
                    if outcome == "finish-error" and len(comments) == 2:
                        raise runner.urllib.error.URLError("test-secret")
                    return {"commentCreate": {"success": True, "comment": {"id": ISSUE}}}

                def process(args, **kwargs):
                    self.assertEqual(transitions, [STATES["In Progress"]])
                    launched.append(args)
                    kwargs["stdout"].write('{partial\n[]\n{"type":"thread.started","thread_id":"invalid"}\n' +
                                           json.dumps({"type": "thread.started", "thread_id": session}) + "\n{partial")
                    if outcome == "process-error":
                        raise subprocess.CalledProcessError(1, args)
                    report = result()
                    report["status"] = "blocked" if outcome == "finish-error" else "ready" if outcome in ("review-state-error", "verification-error") else outcome
                    Path(args[args.index("--output-last-message") + 1]).write_text(json.dumps(report))
                    return subprocess.CompletedProcess(args, 0)

                with patch.object(runner, "graphql", side_effect=api), \
                        patch.object(runner.socket, "gethostname", return_value="worker-host"), \
                        patch.object(runner, "git_profile", return_value={"user.name": "Test", "user.email": "test@example.com"}), \
                        patch.object(runner, "command", return_value=""), \
                        patch.object(runner, "verify_pr", side_effect=ValueError("Invalid PR") if outcome == "verification-error" else None) as verification, \
                        patch.object(runner.subprocess, "run", side_effect=process), \
                        patch.dict(os.environ, {"LINEAR_API_KEY": "test-secret"}):
                    if outcome in ("start-error", "process-error", "start-state-error", "review-state-error", "verification-error", "missing-state", "viewer-error", "assignment-error"):
                        with self.assertRaises((ValueError, subprocess.CalledProcessError, runner.urllib.error.URLError)):
                            runner.run_issue(config, PROJECT, issue, runs, db)
                    else:
                        runner.run_issue(config, PROJECT, issue, runs, db)
                    runner.run_issue(config, PROJECT, issue, runs, db)
                expected = "blocked" if outcome == "finish-error" else "failed" if outcome.endswith("error") or outcome == "missing-state" else outcome
                self.assertEqual(db.execute("SELECT status FROM runs").fetchone()[0], expected)
                expected_transitions = [] if outcome in ("start-error", "missing-state", "viewer-error") else [STATES["In Progress"]]
                if outcome in ("ready", "review-state-error"):
                    expected_transitions.append(STATES["In Review"])
                self.assertEqual(transitions, expected_transitions)
                if outcome in ("missing-state", "viewer-error"):
                    self.assertFalse(launched)
                    self.assertEqual(len(comments), 1)
                    self.assertIn("with status failed.", comments[0])
                    self.assertTrue((runs / ISSUE / "finish-comment.json").exists())
                    continue
                self.assertEqual(len(comments), 2)
                self.assertEqual(comments[0], "Run claimed on worker-host. Starting work on APP-1.")
                self.assertIn(f"Run finished on worker-host with status {expected}.", comments[1])
                if outcome in ("start-error", "start-state-error", "assignment-error"):
                    self.assertFalse(launched)
                    self.assertNotIn("codex resume", comments[1])
                else:
                    self.assertIn(f"Resume the codex session: `test-codex resume {session}`", comments[1])
                if outcome not in ("start-error", "start-state-error", "assignment-error", "process-error"):
                    self.assertEqual(comments[1], f"Run finished on worker-host with status {expected}. "
                                     f"View pull request at {result()['pr_url']}. Resume the codex session: `test-codex resume {session}`")
                folder = runs / ISSUE
                if outcome == "finish-error":
                    self.assertEqual((folder / "finish-comment-error.txt").read_text(), "URLError\n")
                    self.assertFalse((folder / "failure.txt").exists())
                else:
                    self.assertTrue((folder / "finish-comment.json").exists())

    def test_workflow_states_require_unique_exact_team_statuses(self):
        valid = linear_response("workflowStates(", {})
        for variant in ("valid", "missing", "duplicate", "wrong-case", "more-pages"):
            response = copy.deepcopy(valid)
            page = response["workflowStates"]
            if variant == "missing":
                page["nodes"].pop()
            elif variant == "duplicate":
                page["nodes"].append(dict(page["nodes"][0], id=ISSUE))
            elif variant == "wrong-case":
                page["nodes"][0]["name"] = "in progress"
            elif variant == "more-pages":
                page["pageInfo"]["hasNextPage"] = True
            with self.subTest(variant=variant), patch.object(runner, "graphql", return_value=response) as api:
                if variant == "valid":
                    self.assertEqual(runner.workflow_states(ROUTE["team_id"]), STATES)
                else:
                    with self.assertRaisesRegex(ValueError, "requires exactly one"):
                        runner.workflow_states(ROUTE["team_id"])
                self.assertEqual(api.call_args.args[1], {"team": ROUTE["team_id"], "names": list(STATES)})
                self.assertIn("team: {id: {eq: $team}}", api.call_args.args[0])

    def test_state_update_requires_confirmation_and_retains_evidence_without_retry(self):
        variables = {"id": ISSUE, "input": {"stateId": STATES["In Progress"], "assigneeId": USER}}
        valid = linear_response("issueUpdate(", variables)["issueUpdate"]
        invalid = [dict(valid, success=False), dict(valid, issue=None),
                   dict(valid, issue={"id": PROJECT, "state": valid["issue"]["state"]}),
                   dict(valid, issue={"id": ISSUE, "state": {"id": STATES["In Review"]}}),
                   dict(valid, issue=dict(valid["issue"], assignee=None)),
                   dict(valid, issue=dict(valid["issue"], assignee={"id": PROJECT}))]
        for response in [valid, *invalid]:
            with self.subTest(response=response), tempfile.TemporaryDirectory() as temp, \
                    patch.object(runner, "graphql", return_value={"issueUpdate": response}) as api:
                folder = Path(temp)
                if response == valid:
                    runner.update_issue_state(ISSUE, STATES["In Progress"], folder, "start", USER)
                    self.assertEqual(json.loads((folder / "start-state.json").read_text()), valid)
                else:
                    with self.assertRaisesRegex(ValueError, "status update was not confirmed"):
                        runner.update_issue_state(ISSUE, STATES["In Progress"], folder, "start", USER)
                    self.assertFalse((folder / "start-state.json").exists())
                api.assert_called_once()
                self.assertEqual(api.call_args.args[1], variables)
                self.assertEqual(json.loads((folder / "start-state-request.json").read_text()), variables)

    def test_cron_preserves_unrelated_entries_and_replaces_only_own_job(self):
        old = "MAILTO=ops@example.com\n0 0 * * * backup # backup\nold # blank-agent-linear\n"
        new = "*/5 * * * * worker # blank-agent-linear"
        merged = runner.cron_text(old, new)
        self.assertIn("0 0 * * * backup # backup\n", merged)
        self.assertIn("MAILTO=ops@example.com\n", merged)
        self.assertNotIn("old #", merged)
        self.assertEqual(runner.cron_text(merged, new), merged)
        self.assertEqual(runner.cron_text(old), "MAILTO=ops@example.com\n0 0 * * * backup # backup\n")
        self.assertEqual(runner.cron_text(new + "\n"), "")

    def test_uninstall_cron_preserves_other_jobs_without_config_credentials_or_worker_lock(self):
        existing = "MAILTO=ops@example.com\n0 0 * * * backup\n*/5 * * * * poll # blank-agent-linear\n"
        updated = "MAILTO=ops@example.com\n0 0 * * * backup\n"
        with tempfile.TemporaryDirectory() as temp, patch.object(runner, "ROOT", Path(temp)), \
                patch.dict(os.environ, {}, clear=True), \
                patch.object(runner.sys, "argv", ["runner.py", "uninstall-cron"]), \
                patch.object(runner, "load_config") as config, \
                patch.object(runner.fcntl, "flock") as lock, \
                patch.object(runner.sqlite3, "connect") as database, \
                patch.object(runner.subprocess, "run", side_effect=[
                    subprocess.CompletedProcess(["crontab", "-l"], 0, stdout=existing, stderr=""),
                    subprocess.CompletedProcess(["crontab", "-"], 0)]) as processes, \
                patch.object(runner, "command", return_value=updated) as verify:
            runner.main()
            config.assert_not_called()
            lock.assert_not_called()
            database.assert_not_called()
            self.assertEqual(processes.call_args.args[0], ["crontab", "-"])
            self.assertEqual(processes.call_args.kwargs["input"], updated)
            verify.assert_called_once_with(["crontab", "-l"])
            self.assertEqual((Path(temp) / ".runs/crontab.before-uninstall").read_text(), existing)

    def test_uninstall_cron_handles_absent_job_and_rejects_read_or_verification_failure(self):
        responses = [(0, "0 0 * * * backup\n", "", False),
                     (1, "", "no crontab for test", False),
                     (1, "", "permission denied", True)]
        for code, stdout, stderr, fails in responses:
            with self.subTest(stderr=stderr), tempfile.TemporaryDirectory() as temp, \
                    patch.object(runner.subprocess, "run", return_value=
                                 subprocess.CompletedProcess(["crontab", "-l"], code, stdout, stderr)) as processes:
                if fails:
                    with self.assertRaisesRegex(ValueError, "nothing was removed"):
                        runner.uninstall_cron(Path(temp))
                else:
                    runner.uninstall_cron(Path(temp))
                self.assertEqual(processes.call_count, 1)
        with tempfile.TemporaryDirectory() as temp, \
                patch.object(runner.subprocess, "run", side_effect=[
                    subprocess.CompletedProcess(["crontab", "-l"], 0, "poll # blank-agent-linear\n", ""),
                    subprocess.CompletedProcess(["crontab", "-"], 0)]), \
                patch.object(runner, "command", return_value="poll # blank-agent-linear"):
            with self.assertRaisesRegex(ValueError, "removal could not be verified"):
                runner.uninstall_cron(Path(temp))

    def test_both_engines_complete_a_signed_pr_and_do_not_repeat_work(self):
        for engine in ("codex", "opencode"):
            with self.subTest(engine=engine):
                self.run_agent_fixture(engine, "ready")

    def test_failed_runs_never_move_to_review_or_retry(self):
        for engine, outcome in (("codex", "invalid-report"), ("opencode", "invalid-report"),
                                ("opencode", "malformed"), ("opencode", "incomplete"),
                                ("opencode", "error-event"), ("opencode", "process-error"),
                                ("opencode", "wrong-pr")):
            with self.subTest(engine=engine, outcome=outcome):
                self.run_agent_fixture(engine, outcome)

    def run_agent_fixture(self, engine, outcome):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "source"
            source.mkdir()
            git = lambda *args: runner.command(["git", *args], source)
            git("init", "-b", "staging")
            git("config", "user.name", "Test")
            git("config", "user.email", "test@example.com")
            git("config", "commit.gpgsign", "false")
            (source / "app.txt").write_text("original\n")
            git("add", "app.txt")
            git("commit", "-m", "Initial")
            key = root / "signing"
            runner.command(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)])
            allowed = root / "allowed-signers"
            allowed.write_text("test@example.com " + key.with_suffix(".pub").read_text())
            git("config", "gpg.format", "ssh")
            git("config", "user.signingkey", str(key) + ".pub")
            git("config", "gpg.ssh.allowedSignersFile", str(allowed))
            (source / "app.txt").write_text("human uncommitted work\n")
            binary = root / engine
            shutil.copyfile(runner.ROOT / "fixtures/agent.py", binary)
            binary.chmod(0o700)
            (root / "fixture.json").write_text(json.dumps({"engine": engine, "outcome": outcome, "report": result()}))
            runs = root / "runs"
            runs.mkdir()
            config = {"engine": engine, engine: str(binary), "model": "test/model", "gh": "test-gh",
                      "projects": {PROJECT: dict(ROUTE, repo_path=str(source))}}
            issue = {"id": ISSUE, "identifier": "APP-1", "title": "Handle input", "description": "Reject empty input",
                     "project": {"id": PROJECT}, "team": {"id": ROUTE["team_id"]},
                     "state": {"id": ROUTE["ready_state_id"]}}
            real_command = runner.command

            def commands(args, cwd=None):
                if args[:4] == ["git", "remote", "get-url", "origin"]:
                    return "https://github.com/acme/widget.git"
                if args[:4] == ["git", "remote", "set-url", "origin"]:
                    return ""  # Fetch from the local fixture, never GitHub.
                if args[0] == "test-gh":
                    return json.dumps({"url": result()["pr_url"], "state": "OPEN", "isDraft": False,
                                       "baseRefName": "main" if outcome == "wrong-pr" else "staging",
                                       "headRefName": "linear-app-1-" + ISSUE[:8],
                                       "headRefOid": real_command(["git", "rev-parse", "HEAD"], cwd),
                                       "isCrossRepository": False})
                return real_command(args, cwd)

            db = sqlite3.connect(":memory:")
            try:
                db.execute("CREATE TABLE runs (issue_id TEXT PRIMARY KEY, identifier TEXT, status TEXT, folder TEXT, pr_url TEXT)")
                with patch.object(runner, "command", side_effect=commands), \
                        patch.object(runner, "graphql", side_effect=linear_response) as api, \
                        patch.dict(os.environ, {"LINEAR_API_KEY": "test-secret"}):
                    if outcome == "ready":
                        runner.run_issue(config, PROJECT, issue, runs, db)
                    else:
                        with self.assertRaises((ValueError, subprocess.CalledProcessError)):
                            runner.run_issue(config, PROJECT, issue, runs, db)
                    calls = api.call_count
                    runner.run_issue(config, PROJECT, issue, runs, db)
                    self.assertEqual(api.call_count, calls)
                expected = "ready" if outcome == "ready" else "failed"
                self.assertEqual(db.execute("SELECT status FROM runs").fetchone()[0], expected)
                transitions = [call.args[1]["input"]["stateId"] for call in api.call_args_list if "issueUpdate(" in call.args[0]]
                self.assertEqual(transitions, list(STATES.values()) if outcome == "ready" else [STATES["In Progress"]])
                updates = [call.args[1]["input"] for call in api.call_args_list if "issueUpdate(" in call.args[0]]
                self.assertEqual(updates[0]["assigneeId"], USER)
                if outcome == "ready":
                    self.assertNotIn("assigneeId", updates[1])
                comment = (runs / ISSUE / "finish-comment.txt").read_text()
                self.assertIn(f"status {expected}.", comment)
                self.assertIn("--session ses_test123" if engine == "opencode" else f"resume {PROJECT}", comment)
                self.assertEqual(json.loads((runs / ISSUE / "context.json").read_text())["engine"], engine)
                self.assertEqual((source / "app.txt").read_text(), "human uncommitted work\n")
                if outcome != "ready":
                    self.assertTrue((runs / ISSUE / "failure.txt").exists())
            finally:
                db.close()


if __name__ == "__main__":
    unittest.main()
