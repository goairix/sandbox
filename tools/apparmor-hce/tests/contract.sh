#!/usr/bin/env bash
set -euo pipefail
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
export PYTHONDONTWRITEBYTECODE=1
"$ROOT/tests/test-node-prerequisites.sh"
exec python3 -m unittest discover -s "$ROOT/tests" -p 'test_*.py' -v
