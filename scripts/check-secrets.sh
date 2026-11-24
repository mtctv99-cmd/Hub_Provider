#!/usr/bin/env bash
# Fail if a shipped secret placeholder, or anything shaped like one, reappears
# in tracked code/config files. Patterns match by shape/fragment so this script
# never has to contain a complete secret itself.
set -u
cd "$(dirname "$0")/.." || exit 1
PATTERN='[0-9a-f]{64}|sk-sp-[A-Za-z0-9_-]{8,}|omni-secret-key|stress-test-secret'
if git grep -nE "$PATTERN" -- '*.go' '*.py' '*.yaml' '*.yml' '*.json' '*.sh' '*.toml' \
  ':!*_test.go' ':!scripts/check-secrets.sh'; then
  echo "hardcoded secret placeholder found; read it from the environment instead" >&2
  exit 1
fi
exit 0
