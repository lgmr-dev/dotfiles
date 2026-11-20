#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$REPO_ROOT"

command -v ruby >/dev/null 2>&1 || {
    echo "FAIL: ruby is required for semantic .github/settings.yml policy checks" >&2
    exit 1
}
ruby tests/static/assert_no_probot_branches.rb .github/settings.yml

python3 - <<'PY'
import json
import pathlib
import re
import sys

root = pathlib.Path(".")
failures = []


def fail(message):
    failures.append(message)


def load_json(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def external_action_ref(line):
    match = re.search(r"\buses:\s*([^@\s]+)@([^\s#]+)", line)
    if not match or match.group(1).startswith("./"):
        return None
    return match.group(1), match.group(2)


for probe, should_fail in (
    ("uses: actions/checkout@v4", True),
    ("uses: owner/action@0123456789abcdef0123456789abcdef01234567 # v1", False),
    ("uses: ./local-action", False),
):
    parsed = external_action_ref(probe)
    invalid = bool(parsed and not re.fullmatch(r"[0-9a-f]{40}", parsed[1]))
    if invalid != should_fail:
        fail(f"external Actions SHA scanner self-test failed for: {probe}")


renovate = load_json("renovate.json")
if renovate.get("automerge") is not False:
    fail("renovate.json top-level automerge must stay false")
for idx, rule in enumerate(renovate.get("packageRules", []), start=1):
    if rule.get("automerge") is not False:
        fail(f"renovate package rule {idx} must not enable automerge")

settings = pathlib.Path(".github/settings.yml").read_text(encoding="utf-8")
for snippet in (
    "allow_merge_commit: false",
    "allow_squash_merge: true",
    "allow_rebase_merge: false",
    "delete_branch_on_merge: true",
    "Branch protection is deliberately absent",
):
    if snippet not in settings:
        fail(f".github/settings.yml missing {snippet}")
for workflow in pathlib.Path(".github/workflows").glob("*.yml"):
    text = workflow.read_text(encoding="utf-8")
    if "pull_request_target:" in text:
        fail(f"{workflow} must not use pull_request_target")
    if "permissions:\n  contents: read" not in text:
        fail(f"{workflow} must declare read-only contents permission")
    for line_number, line in enumerate(text.splitlines(), start=1):
        parsed = external_action_ref(line)
        if parsed and not re.fullmatch(r"[0-9a-f]{40}", parsed[1]):
            fail(f"{workflow}:{line_number} external action {parsed[0]} must use a full lowercase 40-hex commit SHA")

makefile = pathlib.Path("Makefile").read_text(encoding="utf-8")
for target in ("test-bootstrap", "test-installer"):
    if target not in makefile.split("ci:",1)[1].splitlines()[0]:
        fail(f"canonical ci gate is missing {target}")

if failures:
    for message in failures:
        print(f"FAIL: {message}")
    sys.exit(1)

print("OK")
PY
