import copy
import json
import os
from pathlib import Path
import sqlite3
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
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = root / "repo"
            repo.mkdir()
            config = {"codex": "codex", "gh": "gh", "projects": {PROJECT: dict(ROUTE, repo_path=str(repo))}}
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

    def test_cron_preserves_unrelated_entries_and_replaces_only_own_job(self):
        old = "MAILTO=ops@example.com\n0 0 * * * backup # backup\nold # blank-agent-linear\n"
        new = "*/5 * * * * worker # blank-agent-linear"
        merged = runner.cron_text(old, new)
        self.assertIn("0 0 * * * backup # backup\n", merged)
        self.assertIn("MAILTO=ops@example.com\n", merged)
        self.assertNotIn("old #", merged)
        self.assertEqual(runner.cron_text(merged, new), merged)

    def test_fresh_session_isolation_pr_verification_and_no_duplicate_retry(self):
        with tempfile.TemporaryDirectory() as temp:
            source = Path(temp) / "source"
            source.mkdir()
            git = lambda *args: runner.command(["git", *args], source)
            git("init", "-b", "staging")
            # Identity is scoped only to this throwaway test fixture.
            git("config", "user.name", "Test")
            git("config", "user.email", "test@example.com")
            git("config", "commit.gpgsign", "false")
            (source / "app.txt").write_text("original\n")
            git("add", "app.txt")
            git("commit", "-m", "Initial")
            base = git("rev-parse", "HEAD")
            key = Path(temp) / "signing"
            other_key = Path(temp) / "other-signing"
            for signing_key in (key, other_key):
                runner.command(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(signing_key)])
            allowed = Path(temp) / "allowed-signers"
            allowed.write_text("".join("test@example.com " + Path(str(k) + ".pub").read_text()
                                       for k in (key, other_key)))
            git("config", "gpg.format", "ssh")
            git("config", "user.signingkey", str(key) + ".pub")
            git("config", "gpg.ssh.allowedSignersFile", "../allowed-signers")
            profile = runner.git_profile(source)
            (source / "app.txt").write_text("human uncommitted work\n")
            runs = Path(temp) / "runs"
            runs.mkdir()
            db = sqlite3.connect(runs / "state.sqlite3")
            db.execute("CREATE TABLE runs (issue_id TEXT PRIMARY KEY, identifier TEXT, status TEXT, folder TEXT, pr_url TEXT)")
            route = dict(ROUTE, repo_path=str(source))
            config = {"codex": "test-codex", "gh": "test-gh", "projects": {PROJECT: route}}
            issue = {"id": ISSUE, "identifier": "APP-1", "title": "Handle input", "description": "Reject empty input",
                     "project": {"id": PROJECT}, "team": {"id": ROUTE["team_id"]},
                     "state": {"id": ROUTE["ready_state_id"]}, "url": "https://linear.app/acme/issue/APP-1"}
            real_command = runner.command
            real_run = subprocess.run
            launches = []
            wrong_pr = False

            def commands(args, cwd=None):
                if args[:4] == ["git", "remote", "get-url", "origin"]:
                    return "https://github.com/acme/widget.git"
                if args[:4] == ["git", "remote", "set-url", "origin"]:
                    return ""  # Keep this fixture's fetch local; no network is used.
                if args[0] == "test-gh":
                    sha = real_command(["git", "rev-parse", "HEAD"], cwd)
                    return json.dumps({"url": result()["pr_url"], "state": "OPEN", "isDraft": False,
                                       "baseRefName": "main" if wrong_pr else "staging",
                                       "headRefName": "linear-app-1-" + ISSUE[:8], "headRefOid": sha,
                                       "isCrossRepository": False})
                return real_command(args, cwd)

            def processes(args, **kwargs):
                if args[0] != "test-codex":
                    return real_run(args, **kwargs)
                launches.append(args)
                self.assertNotIn("LINEAR_API_KEY", kwargs["env"])
                self.assertEqual(kwargs["env"]["GIT_AUTHOR_NAME"], "Test")
                self.assertEqual(kwargs["env"]["GIT_AUTHOR_EMAIL"], "test@example.com")
                self.assertNotIn("resume", args)
                self.assertIn("--sandbox", args)
                self.assertIn("workspace-write", args)
                self.assertIn(issue["description"], kwargs["input"])
                repo = Path(kwargs["cwd"])
                self.assertEqual((repo / "app.txt").read_text(), "original\n")
                (repo / "app.txt").write_text("feature\n")
                env = kwargs["env"]
                real_run(["git", "add", "app.txt"], cwd=repo, check=True)
                real_run(["git", "commit", "-S", "-m", "Handle input"],
                         cwd=repo, env=env, check=True, capture_output=True)
                sha = real_command(["git", "rev-parse", "HEAD"], repo)
                Path(args[args.index("--output-last-message") + 1]).write_text(json.dumps(result(sha)))
                return subprocess.CompletedProcess(args, 0)

            with patch.object(runner, "command", side_effect=commands), \
                    patch.object(runner.subprocess, "run", side_effect=processes), \
                    patch.dict(os.environ, {"LINEAR_API_KEY": "test-secret", "GIT_AUTHOR_NAME": "Wrong",
                                            "GIT_AUTHOR_EMAIL": "wrong@example.com"}):
                runner.run_issue(config, PROJECT, issue, runs, db)
                runner.run_issue(config, PROJECT, issue, runs, db)
                self.assertEqual(len(launches), 1)
                self.assertEqual(db.execute("SELECT status FROM runs").fetchone()[0], "ready")
                folder = runs / ISSUE
                report = json.loads((folder / "result.json").read_text())
                checkout = folder / "repo"
                self.assertEqual(real_command(["git", "config", "--get", "user.signingkey"], checkout), str(key) + ".pub")
                self.assertEqual(real_command(["git", "config", "--get", "gpg.ssh.allowedSignersFile"], checkout), str(allowed))
                for options, message in ((["--no-gpg-sign"], "verified good signatures"),
                                         (["-S", "--author", "Other <other@example.com>"], "authors"),
                                         (["-S" + str(other_key) + ".pub"], "signing key")):
                    env = dict(os.environ, GIT_AUTHOR_NAME="Test", GIT_AUTHOR_EMAIL="test@example.com",
                               GIT_COMMITTER_NAME="Test", GIT_COMMITTER_EMAIL="test@example.com")
                    real_run(["git", "commit", *options, "--allow-empty", "-m", "Rejected fixture"],
                             cwd=checkout, env=env, check=True, capture_output=True)
                    with self.assertRaisesRegex(ValueError, message):
                        runner.verify_commits(checkout, base, profile)
                    real_command(["git", "reset", "--hard", report["commit_sha"]], checkout)
                wrong_pr = True
                with self.assertRaisesRegex(ValueError, "GitHub PR does not match"):
                    runner.verify_pr(report, route, folder / "repo", "linear-app-1-" + ISSUE[:8], base, "test-gh", profile)
                next_issue = dict(issue, id="55555555-5555-4555-8555-555555555555", identifier="APP-2")
                with self.assertRaisesRegex(ValueError, "GitHub PR does not match"):
                    runner.run_issue(config, PROJECT, next_issue, runs, db)
                runner.run_issue(config, PROJECT, next_issue, runs, db)
                self.assertEqual(len(launches), 2)
                self.assertEqual(db.execute("SELECT status FROM runs WHERE issue_id=?", (next_issue["id"],)).fetchone()[0], "failed")
                self.assertTrue((runs / next_issue["id"] / "failure.txt").exists())
            self.assertEqual((source / "app.txt").read_text(), "human uncommitted work\n")
            self.assertEqual(git("rev-parse", "HEAD"), base)
            db.close()


if __name__ == "__main__":
    unittest.main()
