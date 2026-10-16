#!/usr/bin/env python3
"""Run the documented load matrix and retain reports plus environment metadata."""

import argparse
import datetime
import json
import os
from pathlib import Path
import platform
import subprocess
import sys


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apis', default='http://localhost:8081,http://localhost:8082,http://localhost:8083')
    parser.add_argument('--issuer', default='http://localhost:8080/realms/jungle')
    parser.add_argument('--histories', default='0,100,1000')
    parser.add_argument('--wallet-counts', default='1,8')
    parser.add_argument('--concurrency', type=int, default=16)
    parser.add_argument('--duration', type=float, default=15)
    parser.add_argument('--warmup', type=float, default=1)
    parser.add_argument('--drain-timeout', type=float, default=600)
    parser.add_argument('--output-dir', type=Path, default=Path('.local/load') / datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ'))
    args = parser.parse_args()
    try:
        histories = [int(value) for value in args.histories.split(',')]
        wallet_counts = [int(value) for value in args.wallet_counts.split(',')]
    except ValueError:
        parser.error('histories and wallet-counts must be comma-separated integers')
    if any(value < 0 or value > 100000 for value in histories):
        parser.error('histories must be between 0 and 100000')
    if any(value < 1 or value > 256 for value in wallet_counts):
        parser.error('wallet-counts must be between 1 and 256')
    args.output_dir.mkdir(parents=True, exist_ok=True)
    root = Path(__file__).resolve().parent.parent
    cpu_model = None
    total_memory = None
    if Path('/proc/cpuinfo').exists():
        cpu_model = next((line.split(':', 1)[1].strip() for line in Path('/proc/cpuinfo').read_text().splitlines() if line.startswith('model name')), None)
    if Path('/proc/meminfo').exists():
        total_memory = next((line.split(':', 1)[1].strip() for line in Path('/proc/meminfo').read_text().splitlines() if line.startswith('MemTotal:')), None)
    revision = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
    dirty = bool(subprocess.check_output(['git', 'status', '--porcelain', '--untracked-files=no'], cwd=root))
    manifest = {'started_at': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'revision': revision,
                'working_tree_modified': dirty, 'python': platform.python_version(), 'platform': platform.platform(),
                'logical_cpus': os.cpu_count(), 'cpu_model': cpu_model, 'memory_total': total_memory,
                'configuration': {**vars(args), 'output_dir': str(args.output_dir)}, 'runs': []}
    cases = [('write', wallets, history) for history in histories for wallets in wallet_counts]
    cases += [('replay', 1, 0), ('conflict', 1, 0)]
    failed = False
    for scenario, wallets, history in cases:
        name = scenario + '-w' + str(wallets) + '-h' + str(history)
        destination = args.output_dir / (name + '.json')
        command = [sys.executable, str(root / 'scripts/load.py'), '--apis', args.apis, '--issuer', args.issuer,
                   '--scenario', scenario, '--wallets', str(wallets), '--history', str(history),
                   '--concurrency', str(args.concurrency), '--duration', str(args.duration), '--warmup', str(args.warmup),
                   '--drain-timeout', str(args.drain_timeout), '--output', str(destination)]
        print('Running ' + name, flush=True)
        with (args.output_dir / (name + '.log')).open('w') as log:
            result = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT)
        manifest['runs'].append({'case': name, 'report': destination.name, 'exit_code': result.returncode})
        failed = failed or result.returncode != 0
        (args.output_dir / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        if result.returncode != 0:
            print('FAIL: ' + name + '; inspect ' + str(destination.with_suffix('.log')), flush=True)
        else:
            report = json.loads(destination.read_text())
            print(name + ': ' + str(round(report['requests_per_second'], 2)) + ' req/s; p95=' + str(round(report['latency_ms']['p95'], 2)) + 'ms', flush=True)
    manifest['ended_at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    manifest['passed'] = not failed
    (args.output_dir / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    print('Evidence: ' + str(args.output_dir), flush=True)
    raise SystemExit(1 if failed else 0)


if __name__ == '__main__':
    main()
