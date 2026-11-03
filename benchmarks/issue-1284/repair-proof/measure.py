#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

source_root = Path(__file__).resolve().parent
repo = Path(subprocess.check_output(['git', 'rev-parse', '--show-toplevel'], cwd=source_root, text=True).strip())
root = Path(sys.argv[sys.argv.index('--out') + 1]).resolve() if '--out' in sys.argv else repo / '.tmp/issue-1284-repair-proof'
root.mkdir(parents=True, exist_ok=True)
main = '9dc5ac8bc7fb277bef00991cd731a389a3cbca94'
branch = '0c16c26c64fb5f35256ebecaf459bbe5365aa28c'
real_pi = shutil.which(os.environ.get('NM_PROOF_REAL_PI', 'pi'))
if real_pi is None:
    raise RuntimeError('the real Pi CLI is missing; put it on PATH or set NM_PROOF_REAL_PI')

def run(args, cwd=None, **kwargs):
    return subprocess.run(args, cwd=cwd, check=True, text=True, capture_output=True, **kwargs).stdout.strip()

def low_load():
    while os.getloadavg()[0] > 10:
        time.sleep(30)

# Build products from pinned archives. Added files expose schemas and validators,
# but do not replace or change production behavior.
if '--build' in sys.argv:
    for name, commit in [('main', main), ('branch', branch)]:
        source = root / (name + '-src')
        source.mkdir()
        archive = subprocess.Popen(['git', 'archive', commit], cwd=repo, stdout=subprocess.PIPE)
        subprocess.run(['tar', '-x', '-C', str(source)], stdin=archive.stdout, check=True)
        archive.stdout.close()
        if archive.wait() != 0:
            raise RuntimeError('archive failed')
        (source / 'cmd/schema-proof').mkdir(parents=True)
        (source / 'cmd/schema-proof/main.go').write_bytes((source_root / 'main.go.tmpl').read_bytes())
        (source / 'internal/pipeline/steps/issue1284_proof_exports.go').write_bytes((source_root / 'steps.go.tmpl').read_bytes())
        (source / 'internal/agent/issue1284_proof_exports.go').write_bytes((source_root / 'agent.go.tmpl').read_bytes())
        low_load()
        print(run(['go', 'build', '-o', str(root / (name + '-proof')), './cmd/schema-proof'], cwd=source), flush=True)
    manifest = {'main': main, 'branch': branch, 'pi': real_pi, 'pi_version': run([real_pi, '--version']),
                'model': 'openai-codex/gpt-6.1-sol', 'thinking': 'high', 'repetitions': 5,
                'hashes': {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                           for p in [source_root / n for n in ('measure.py', 'main.go.tmpl', 'steps.go.tmpl', 'agent.go.tmpl', 'observe.ts', 'observe-pi.py')]}}
    (root / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    print('built both pinned production adapters', flush=True)
    sys.exit(0)

if '--run' not in sys.argv:
    raise SystemExit('use --build or --run')

results = root / 'runs.jsonl'
existing = [json.loads(line) for line in results.read_text().splitlines()] if results.exists() else []
sequence = []
for repetition in range(1, 6):
    for schema in ['commit-summary', 'ci-fix']:
        arms = ['main', 'branch'] if repetition % 2 else ['branch', 'main']
        sequence.extend((arm, schema, repetition) for arm in arms)
limit = int(sys.argv[sys.argv.index('--limit') + 1]) if '--limit' in sys.argv else 20
launched = 0
for i, (arm, schema, repetition) in enumerate(sequence):
    if i < len(existing):
        assert (existing[i]['arm'], existing[i]['schema'], existing[i]['repetition']) == (arm, schema, repetition)
        continue
    if launched >= limit:
        break
    launched += 1
    low_load()
    prefix = root / f'{i+1:02d}-{arm}-{schema}'
    fixture = root / f'{i+1:02d}-fixture'
    fixture.mkdir()
    (fixture / 'go.mod').write_text('module example.com/schema-proof\n\ngo 1.25\n')
    (fixture / 'count.go').write_text('package counter\n\n// NonNegative returns zero for negative counts and preserves all other counts.\nfunc NonNegative(n int) int {\n\tif n > 0 {\n\t\treturn 0\n\t}\n\treturn n\n}\n')
    (fixture / 'count_test.go').write_text('package counter\n\nimport "testing"\n\nfunc TestNonNegative(t *testing.T) {\n\tfor _, tc := range []struct{ input, want int }{{-3, 0}, {0, 0}, {4, 4}} {\n\t\tif got := NonNegative(tc.input); got != tc.want {\n\t\t\tt.Errorf("NonNegative(%d) = %d, want %d", tc.input, got, tc.want)\n\t\t}\n\t}\n}\n')
    test_hash = hashlib.sha256((fixture / 'count_test.go').read_bytes()).hexdigest()
    git = ['git', '-c', 'core.hooksPath=/dev/null']
    run(git + ['init', '-b', 'proof'], cwd=fixture)
    run(git + ['config', 'user.name', 'Schema Proof'], cwd=fixture)
    run(git + ['config', 'user.email', 'schema-proof@example.invalid'], cwd=fixture)
    run(git + ['add', '.'], cwd=fixture)
    run(git + ['commit', '-m', 'test: add nonnegative count contract'], cwd=fixture)
    head = run(git + ['rev-parse', 'HEAD'], cwd=fixture)
    before = subprocess.run(['go', 'test', './...'], cwd=fixture, text=True, capture_output=True)
    assert before.returncode != 0 and 'NonNegative(-3) = -3, want 0' in before.stdout
    log = prefix.with_suffix('.before.txt')
    log.write_text(before.stdout + before.stderr)
    env = os.environ.copy()
    env.update(NM_PROOF_PREFIX=str(prefix), NM_PROOF_REAL_PI=real_pi,
               NM_PROOF_IMPLEMENTATION=main if arm == 'main' else branch,
               NM_PROOF_OBSERVER=str(source_root / 'observe.ts'), NM_PROOF_PI_WRAPPER=str(source_root / 'observe-pi.py'))
    low_load()
    call = subprocess.run([str(root / (arm + '-proof')), schema, str(fixture), head, head, str(log)],
                          env=env, text=True, capture_output=True, timeout=520)
    prefix.with_suffix('.adapter.stderr.txt').write_text(call.stderr)
    prefix.with_suffix('.result.json').write_text(call.stdout)
    if call.returncode != 0:
        raise RuntimeError('measurement harness failed: ' + call.stderr)
    row = json.loads(call.stdout)
    row.update(arm=arm, repetition=repetition, sequence=i+1, load_at_completion=os.getloadavg()[0])
    events_files = sorted(root.glob(prefix.name + '.*.events.jsonl'))
    provider_file = prefix.with_suffix('.provider.jsonl')
    provider = [json.loads(line) for line in provider_file.read_text().splitlines()] if provider_file.exists() else []
    row['provider_evidence'] = provider
    events = [json.loads(line) for path in events_files for line in path.read_text().splitlines() if line.strip()]
    messages = [e['message'] for e in events if e['type'] == 'message_end' and e['message'].get('role') == 'assistant']
    row['concrete_pi_processes'] = len(events_files)
    row['settled'] = any(e['type'] == 'agent_settled' for e in events)
    row['pi_auto_retries'] = sum(e['type'] == 'auto_retry_start' for e in events)
    final = messages[-1] if messages else {}
    calls = [b for b in final.get('content', []) if b.get('type') == 'toolCall']
    row['final_tool_names'] = [b.get('name') for b in calls]
    row['final_stop_reason'] = final.get('stopReason')
    row['assistant_models'] = sorted({m.get('provider', '') + '/' + m.get('model', '') for m in messages})
    row['response_models'] = sorted({p['responseModel'] for p in provider if p['type'] == 'provider_model'})
    outputs = [e for e in events if e['type'] == 'tool_execution_end' and e.get('toolName') == 'no_mistakes_output'
               and e.get('isError') is False and e.get('result', {}).get('terminate') is True]
    row['finished_output_tool_alone'] = (final.get('stopReason') == 'toolUse' and len(calls) == 1
        and calls[0].get('name') == 'no_mistakes_output' and any(e.get('toolCallId') == calls[0].get('id') for e in outputs))
    raw_output = None
    if row['finished_output_tool_alone']:
        raw_output = json.dumps(calls[0]['arguments'])
    elif final:
        raw_output = ''.join(b.get('text', '') for b in final.get('content', []) if b.get('type') == 'text')
    row['raw_output_valid'] = False
    if raw_output:
        raw_file = prefix.with_suffix('.raw-output.txt')
        raw_file.write_text(raw_output)
        validation = json.loads(run([str(root / (arm + '-proof')), '--validate', schema, str(raw_file)]))
        row['raw_output_valid'] = validation['valid']
        row['raw_validation'] = validation
    after = subprocess.run(['go', 'test', './...'], cwd=fixture, text=True, capture_output=True)
    prefix.with_suffix('.after.txt').write_text(after.stdout + after.stderr)
    row['repair_tests_passed'] = after.returncode == 0
    row['test_unchanged'] = test_hash == hashlib.sha256((fixture / 'count_test.go').read_bytes()).hexdigest()
    row['diff'] = run(git + ['diff'], cwd=fixture)
    row['tokens'] = {name: sum(m.get('usage', {}).get(name, 0) for m in messages)
                     for name in ['input', 'output', 'cacheRead', 'cacheWrite']}
    row['cost_total_reported'] = sum(m.get('usage', {}).get('cost', {}).get('total', 0) for m in messages)
    row['model_verified'] = bool(row['response_models']) and row['assistant_models'] == ['openai-codex/gpt-6.1-sol'] and all(p.get('payloadModel') == 'gpt-6.1-sol' for p in provider if p['type'] == 'request')
    with results.open('a') as f:
        f.write(json.dumps(row, sort_keys=True) + '\n')
    print(f"{i+1}/20 {arm} {schema}: schema_valid={row['schema_valid']} raw_valid={row['raw_output_valid']} output_tool_alone={row['finished_output_tool_alone']} repair_pass={row['repair_tests_passed']} model={row['response_models']} seconds={row['seconds']:.3f}", flush=True)
    error = row.get('error', '')
    if not row['model_verified'] or any(word in error.lower() for word in ('quota', 'usage limit', 'rate limit', 'unauthorized', 'authentication', 'not supported', 'unavailable')):
        raise RuntimeError('provider/model proof blocked; inspect recorded evidence: ' + error)
print(f'{len(existing) + launched}/20 real Pi calls recorded', flush=True)
