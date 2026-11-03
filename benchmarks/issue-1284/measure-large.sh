#!/usr/bin/env bash
set -euo pipefail

main=${1:?usage: measure-large.sh MAIN_SHA STRICT_SHA OUTPUT_DIR [REPETITIONS] [PAIRS]}
strict=${2:?usage: measure-large.sh MAIN_SHA STRICT_SHA OUTPUT_DIR [REPETITIONS] [PAIRS]}
out=${3:?usage: measure-large.sh MAIN_SHA STRICT_SHA OUTPUT_DIR [REPETITIONS] [PAIRS]}
repetitions=${4:-10}
pairs=${5:-1}
repo=$(git rev-parse --show-toplevel)
branch=$(git symbolic-ref --short HEAD)
main=$(git rev-parse "${main}^{commit}")
strict=$(git rev-parse "${strict}^{commit}")
mkdir -p "$out"
out=$(cd "$out" && pwd -P)
if [[ -n $(git status --porcelain) ]]; then
  printf '%s\n' 'A clean task worktree is required for historical replay.' >&2
  exit 1
fi

python3 - "$repo" "$out" "$main" "$strict" "$repetitions" "$pairs" <<'PY'
import hashlib
import json
import pathlib
import subprocess
import sys
repo, out, main, strict, repetitions, pairs = sys.argv[1:]
repo, out = pathlib.Path(repo), pathlib.Path(out)
repetitions, pairs = int(repetitions), int(pairs)
if repetitions < 1 or pairs < 1:
    raise SystemExit('Repetitions and pairs must be positive.')
cases = []
for name, head in (
    ('machine-local-overrides', 'f4b85ae161cef8f14e289cc90aebae044ed570b9'),
    ('review-coverage', 'ac8e342c54b97a99936dddc238198db342b7b966'),
    ('review-answer-follow', 'cf93ddc28b79ecff5500d07a9d57e0ff0276e7c8'),
):
    base = subprocess.check_output(['git', 'rev-parse', head + '^'], text=True).strip()
    stats = subprocess.check_output(['git', 'diff', '--numstat', base, head], text=True).splitlines()
    cases.append(dict(name=name, base=base, head=head, files=len(stats),
                      insertions=sum(int(line.split('\t')[0]) for line in stats),
                      deletions=sum(int(line.split('\t')[1]) for line in stats)))
manifest = dict(main=main, strict=strict, repetitions=repetitions, cases=cases,
                provider='openai-codex', model='gpt-6.1-sol', thinking='high',
                pi_version=subprocess.check_output(['pi', '--version'], text=True).strip(),
                harness_sha256=hashlib.sha256((repo / 'benchmarks/issue-1284/reviewbench/main.go').read_bytes()).hexdigest(),
                script_sha256=hashlib.sha256((repo / 'benchmarks/issue-1284/measure-large.sh').read_bytes()).hexdigest())
manifest_path = out / 'manifest.json'
results = out / 'runs.jsonl'
if manifest_path.exists():
    if json.loads(manifest_path.read_text()) != manifest:
        raise SystemExit('Refusing to mix different implementations, harnesses, or workloads.')
else:
    if results.exists():
        raise SystemExit('Results without a manifest cannot be resumed.')
    manifest_path.write_text(json.dumps(manifest, indent=2) + '\n')
plan = []
for trial in range(1, repetitions + 1):
    for case_index, case in enumerate(cases):
        pair_index = (trial - 1) * len(cases) + case_index
        arms = ('main', 'strict') if pair_index % 2 == 0 else ('strict', 'main')
        for arm in arms:
            plan.append(dict(sequence=len(plan) + 1, trial=trial, case=case['name'], arm=arm,
                             base=case['base'], head=case['head'], implementation=manifest[arm]))
rows = [json.loads(line) for line in results.read_text().splitlines()] if results.exists() else []
if len(rows) > len(plan):
    raise SystemExit('More results than planned launches.')
for row, expected in zip(rows, plan):
    if any(row.get(key) != value for key, value in expected.items()):
        raise SystemExit('Results are not an intact prefix of the interleaved plan.')
    if not row['completed'] and not row['schema_exhausted']:
        raise SystemExit('A provider/process failure ended this cohort; do not silently replace it.')
remaining = plan[len(rows):len(rows) + 2 * pairs]
(out / 'next.tsv').write_text(''.join('\t'.join(str(row[key]) for key in (
    'sequence', 'trial', 'case', 'arm', 'base', 'head', 'implementation')) + '\n' for row in remaining))
print(f'Completed {len(rows)}/{len(plan)} launches; this batch has {len(remaining)} launches.')
PY

if [[ ! -s "$out/next.tsv" ]]; then
  exit 0
fi
if [[ ! -f "$out/build/ready" ]]; then
  mkdir -p "$out/build"
  for arm in main strict; do
    sha=$main
    [[ $arm == strict ]] && sha=$strict
    mkdir -p "$out/build/$arm/source"
    git archive "$sha" | tar -x -C "$out/build/$arm/source"
    mkdir -p "$out/build/$arm/source/benchmarks/issue-1284/reviewbench"
    cp "$repo/benchmarks/issue-1284/reviewbench/main.go" "$out/build/$arm/source/benchmarks/issue-1284/reviewbench/main.go"
    (cd "$out/build/$arm/source" && go build -o "$out/build/$arm/reviewbench" ./benchmarks/issue-1284/reviewbench)
  done
  touch "$out/build/ready"
fi

# Only this existing task worktree changes HEAD; archives above are build input.
restore_branch() {
  if git diff --quiet && git diff --cached --quiet; then
    git checkout --quiet "$branch"
  else
    printf '%s\n' 'Historical review modified tracked files; leaving them intact for inspection.' >&2
  fi
}
trap restore_branch EXIT
while IFS=$'\t' read -r sequence trial workload arm base head implementation; do
  git checkout --quiet --detach "$head"
  until awk '{exit !($1 <= 10)}' /proc/loadavg; do sleep 30; done
  printf 'Launch %s: %s trial %s on %s\n' "$sequence" "$workload" "$trial" "$arm" >&2
  status=0
  "$out/build/$arm/reviewbench" --cwd "$repo" --base "$base" --head "$head" \
    --arm "$arm" --implementation "$implementation" --case "$workload" \
    --trial "$trial" --sequence "$sequence" > "$out/run.json.tmp" || status=$?
  python3 - "$out" "$sequence" <<'PY'
import json
import pathlib
import sys
out = pathlib.Path(sys.argv[1])
data = (out / 'run.json.tmp').read_text()
lines = data.splitlines()
if len(lines) != 1:
    raise SystemExit('Review did not produce exactly one outcome record.')
row = json.loads(lines[0])
if row['sequence'] != int(sys.argv[2]):
    raise SystemExit('Unexpected launch ordinal.')
with (out / 'runs.jsonl').open('a') as results:
    results.write(data)
fields = [attempt['schema_field'] for attempt in row['adapter_attempts'] if attempt['schema_rejected']]
print(f"{row['arm']} {row['case']} {row['trial']}: first_valid={row['schema_valid_first_attempt']} "
      f"completed={row['completed']} exhausted={row['schema_exhausted']} fields={fields} "
      f"schema_retries={row['schema_retries']} adapter_retries={row['adapter_retries']} wall_ms={row['wall_ms']}")
PY
  rm "$out/run.json.tmp"
  if [[ $status != 0 ]]; then
    exit "$status"
  fi
  if [[ -n $(git status --porcelain) ]]; then
    printf '%s\n' 'Historical review changed the task worktree; stopping.' >&2
    exit 1
  fi
done < "$out/next.tsv"
