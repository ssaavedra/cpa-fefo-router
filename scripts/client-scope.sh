#!/usr/bin/env bash
# Prints the CPA caller scope of client API keys, for `clients.<name>.keys`.
# The caller scope is sha256("cli-proxy-api:caller-scope:v1\0" + key); CPA
# hands it to scheduler plugins instead of the key itself.
#
# Usage:
#   scripts/client-scope.sh FILE...        # one key per file, name = file name
#   printf %s "$KEY" | scripts/client-scope.sh -
set -euo pipefail
scope() { { printf 'cli-proxy-api:caller-scope:v1\0'; printf %s "$1"; } | sha256sum | cut -d' ' -f1; }
command -v sha256sum >/dev/null || sha256sum() { shasum -a 256 "$@"; }
[ $# -gt 0 ] || { echo "usage: $0 FILE... | -" >&2; exit 2; }
for f in "$@"; do
  if [ "$f" = - ]; then key=$(cat); name=stdin; else key=$(cat "$f"); name=$(basename "$f"); fi
  key=${key%$'\n'}
  printf '%s: %s\n' "$name" "$(scope "$key")"
done
