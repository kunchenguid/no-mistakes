#!/usr/bin/env bash
set -euo pipefail

# Run from the repository root after selecting the implementation to measure.
# The archived baseline is build input, not another registered Git worktree.
main=${1:?usage: measure.sh MAIN_SHA OUTPUT_DIR [REPETITIONS]}
out=${2:?usage: measure.sh MAIN_SHA OUTPUT_DIR [REPETITIONS]}
repetitions=${3:-3}
repo=$(git rev-parse --show-toplevel)
main=$(git rev-parse "${main}^{commit}")
mkdir -p "$repo/.tmp" "$out"
out=$(cd "$out" && pwd -P)
if [[ -e "$out/baseline.jsonl" || -e "$out/strict.jsonl" ]]; then
  printf '%s\n' 'Refusing to overwrite or append to an existing measurement.' >&2
  exit 1
fi
scratch=$(mktemp -d "$repo/.tmp/pi-schema-bench.XXXXXX")
trap 'rm -rf "$scratch"' EXIT

git archive "$main" | tar -x -C "$scratch"
mkdir -p "$scratch/benchmarks/issue-1284/reviewbench"
cp "$repo/benchmarks/issue-1284/reviewbench/main.go" "$scratch/benchmarks/issue-1284/reviewbench/main.go"
(cd "$scratch" && nice -n 10 go build -o "$scratch/baseline" ./benchmarks/issue-1284/reviewbench)
(cd "$repo" && nice -n 10 go build -o "$scratch/strict" ./benchmarks/issue-1284/reviewbench)

for ((i=1; i<=repetitions; i++)); do
  nice -n 10 "$scratch/baseline" --cwd "$repo" --arm main --n 1 >> "$out/baseline.jsonl"
  nice -n 10 "$scratch/strict" --cwd "$repo" --arm strict --n 1 >> "$out/strict.jsonl"
done

python3 - "$out" <<'PY'
import json
import pathlib
import sys
root = pathlib.Path(sys.argv[1])
for name in ('baseline', 'strict'):
    rows = [json.loads(line) for line in (root / (name + '.jsonl')).read_text().splitlines()]
    first = sum(bool(row['attempts']) and row['attempts'][0]['schema_valid'] for row in rows)
    completed = sum(row['completed'] for row in rows)
    print(f'{name}: schema-valid first attempts {first}/{len(rows)}; completions {completed}/{len(rows)}')
PY
