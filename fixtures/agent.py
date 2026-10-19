#!/usr/bin/env python3
"""CLI stand-in for end-to-end runner tests; all git operations are real."""
import json
import os
from pathlib import Path
import subprocess
import sys


def main():
    fixture = json.loads(Path(__file__).with_name("fixture.json").read_text())
    engine, outcome = fixture["engine"], fixture["outcome"]
    prompt = sys.stdin.read()
    assert "Reject empty input" in prompt
    assert "LINEAR_API_KEY" not in os.environ
    assert os.environ["GIT_AUTHOR_NAME"] == "Test"
    assert os.environ["GIT_AUTHOR_EMAIL"] == "test@example.com"
    assert "--model" in sys.argv
    assert "--session" not in sys.argv and "resume" not in sys.argv
    session = "ses_test123" if engine == "opencode" else "11111111-1111-4111-8111-111111111111"

    def emit(kind, **data):
        print(json.dumps({"type": kind, "sessionID": session, **data}), flush=True)

    if engine == "opencode":
        assert sys.argv[1:4] == ["run", "--format", "json"]
        permission = json.loads(os.environ["OPENCODE_CONFIG_CONTENT"])["agent"]["linear-issue"]["permission"]
        assert permission["task"] == permission["question"] == "deny"
        emit("step_start")
    else:
        assert "workspace-write" in sys.argv
        emit("thread.started", thread_id=session)
    if outcome == "process-error":
        sys.exit(1)

    assert Path("app.txt").read_text() == "original\n"
    Path("app.txt").write_text("feature\n")
    subprocess.run(["git", "add", "app.txt"], check=True)
    subprocess.run(["git", "commit", "-S", "-m", "Handle input"], check=True, capture_output=True)
    report = fixture["report"]
    report["commit_sha"] = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    if outcome == "invalid-report":
        report["checks"][0]["passed"] = "true"
    text = "not JSON" if outcome == "malformed" else json.dumps(report)
    if engine == "codex":
        Path(sys.argv[sys.argv.index("--output-last-message") + 1]).write_text(text)
    else:
        emit("text", part={"id": "part_test", "text": text})
        emit("step_finish", part={"reason": "length" if outcome == "incomplete" else "stop"})
        if outcome == "error-event":
            emit("error", error={"name": "ProviderError"})


if __name__ == "__main__":
    main()
