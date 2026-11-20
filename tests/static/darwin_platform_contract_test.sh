#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
python3 - <<'PY'
import json,pathlib
pins={tool: targets for path in sorted(pathlib.Path("installer/tools").glob("*/pins.json")) for tool, targets in json.loads(path.read_text())["resources"].items()}
assert all(key != "darwin/amd64" for targets in pins.values() for key in targets)
bootstrap=pathlib.Path("scripts/installer-bootstrap.sh").read_text()
assert "darwin-arm64" in bootstrap
assert "darwin-amd64" not in bootstrap
for workflow in pathlib.Path(".github/workflows").glob("*.yml"):
    text=workflow.read_text()
    assert "macos-26-intel" not in text, workflow
print("OK: active macOS provisioning supports Apple Silicon only")
PY
