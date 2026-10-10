#!/usr/bin/env python3
import json
import os
from pathlib import Path
import subprocess
import sys
import threading

real_pi = os.environ['NM_PROOF_REAL_PI']
if sys.argv[1:] == ['--version']:
    os.execv(real_pi, [real_pi, '--version'])
prefix = os.environ['NM_PROOF_PREFIX'] + '.' + str(os.getpid())
Path(prefix + '.argv.json').write_text(json.dumps([real_pi] + sys.argv[1:]) + '\n')
with open(prefix + '.events.jsonl', 'xb') as capture, open(prefix + '.stderr.txt', 'xb') as errors:
    child = subprocess.Popen([real_pi] + sys.argv[1:], stdin=sys.stdin.buffer,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    def forward_errors():
        while block := child.stderr.read1(65536):
            errors.write(block)
            errors.flush()
            sys.stderr.buffer.write(block)
            sys.stderr.buffer.flush()
    thread = threading.Thread(target=forward_errors)
    thread.start()
    while block := child.stdout.read1(65536):
        capture.write(block)
        capture.flush()
        sys.stdout.buffer.write(block)
        sys.stdout.buffer.flush()
    result = child.wait()
    thread.join()
    sys.exit(result)
