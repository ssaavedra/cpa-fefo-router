#!/usr/bin/env bash
# Regenerates internal/pricing/models.json from https://models.dev/api.json,
# keeping only models with a cost from the anthropic and openai providers.
#
# Usage: scripts/update-prices.sh
set -euo pipefail
out="$(cd "$(dirname "$0")/.." && pwd)/internal/pricing/models.json"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 3 https://models.dev/api.json -o "$tmp/api.json"
python3 -I - "$tmp/api.json" "$tmp/models.json" <<'PY'
import json, sys
src, dst = sys.argv[1:3]
api = json.load(open(src))
keep = ("input", "output", "cache_read", "cache_write")
def trim(c):
    t = {k: c[k] for k in keep if k in c}
    if c.get("tiers"):
        t["tiers"] = [{**{k: x[k] for k in keep if k in x}, "tier": x["tier"]} for x in c["tiers"]]
    return t
out = {p: {"models": {i: {"cost": trim(m["cost"])}
                      for i, m in sorted(api[p]["models"].items()) if m.get("cost")}}
       for p in ("anthropic", "openai")}
with open(dst, "w") as f:
    json.dump(out, f, sort_keys=True, indent=1)
    f.write("\n")
PY
mv "$tmp/models.json" "$out"
echo "wrote $out"
