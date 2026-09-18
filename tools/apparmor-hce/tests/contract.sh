#!/usr/bin/env bash
set -euo pipefail
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
export PYTHONDONTWRITEBYTECODE=1
exec python3 -m unittest discover -s "$ROOT/tests" -p 'test_*.py' -v
