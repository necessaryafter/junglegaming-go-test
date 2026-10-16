#!/usr/bin/env python3
"""Compare two binaries on fresh copies of one local PostgreSQL snapshot."""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--engine', choices=['docker', 'podman'], default='podman')
    parser.add_argument('--postgres', default='jungle_postgres_1')
    parser.add_argument('--app', default='jungle_app_1')
    parser.add_argument('--baseline-binary', type=Path, required=True)
    parser.add_argument('--optimized-binary', type=Path, required=True)
    parser.add_argument('--migrate-binary', type=Path, required=True)
    parser.add_argument('--wallet-counts', default='1,8')
    parser.add_argument('--variants', default='baseline,optimized')
    parser.add_argument('--monitor-locks', action='store_true', help='diagnostic sampling; adds measurement overhead')
    parser.add_argument('--repeats', type=int, default=3)
    parser.add_argument('--duration', type=float, default=30)
    parser.add_argument('--port', type=int, default=18081)
    parser.add_argument('--postgres-port', type=int, default=5432)
    parser.add_argument('--output-dir', type=Path, required=True)
    args = parser.parse_args()
    wallets = [int(value) for value in args.wallet_counts.split(',')]
    requested_variants = args.variants.split(',')
    if not requested_variants or any(value not in ['baseline', 'optimized'] for value in requested_variants):
        parser.error('variants must contain baseline and/or optimized')
    if not wallets or any(value < 1 or value > 256 for value in wallets):
        parser.error('wallet counts must be between 1 and 256')
    if args.repeats < 1 or args.duration <= 0:
        parser.error('repeats and duration must be positive')
    for binary in [args.baseline_binary, args.optimized_binary, args.migrate_binary]:
        if not binary.is_file():
            parser.error('missing binary: ' + str(binary))
    args.output_dir.mkdir(parents=True, exist_ok=False)
    root = Path(__file__).resolve().parent.parent

    def engine(*command, **kwargs):
        return subprocess.run([args.engine, *command], check=True, **kwargs)

    def inspect(container):
        return json.loads(engine('inspect', container, capture_output=True, text=True).stdout)[0]

    app = inspect(args.app)
    pg = inspect(args.postgres)
    environment = dict(item.split('=', 1) for item in app['Config']['Env'])
    pg_environment = dict(item.split('=', 1) for item in pg['Config']['Env'])
    admin = pg_environment['POSTGRES_USER']
    database = pg_environment['POSTGRES_DB']
    networks = list(app['NetworkSettings']['Networks'])
    if len(networks) != 1:
        raise RuntimeError('source app must have exactly one network')

    def sql(target, statement):
        return engine('exec', args.postgres, 'psql', '-X', '-v', 'ON_ERROR_STOP=1',
                      '-U', admin, '-d', target, '-Atc', statement, capture_output=True, text=True).stdout.strip()

    if sql(database, 'SELECT COUNT(*) FROM public.outbox WHERE published_at IS NULL') != '0':
        raise RuntimeError('source outbox must be empty before snapshot')
    if sql(database, "SELECT COUNT(*) FROM public.wager_transactions WHERE status IN ('PENDING', 'PENDING_REFERENCE')") != '0':
        raise RuntimeError('source must not contain unfinished financial operations')
    snapshot = args.output_dir / 'snapshot.dump'
    with snapshot.open('wb') as output:
        engine('exec', args.postgres, 'pg_dump', '-U', admin, '-d', database,
               '--schema=public', '--format=custom', stdout=output)
    memory = {}
    if Path('/proc/meminfo').exists():
        memory = {line.split(':')[0]: line.split(':')[1].strip() for line in Path('/proc/meminfo').read_text().splitlines()
                  if line.split(':')[0] in ['MemTotal', 'MemAvailable', 'SwapTotal', 'SwapFree']}
    manifest = {
        'started_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
        'configuration': {key: str(value) if isinstance(value, Path) else value for key, value in vars(args).items()},
        'snapshot_sha256': hashlib.sha256(snapshot.read_bytes()).hexdigest(),
        'binary_sha256': {name: hashlib.sha256(path.read_bytes()).hexdigest() for name, path in
                          [('baseline', args.baseline_binary), ('optimized', args.optimized_binary)]},
        'image': app['Image'], 'network': networks[0],
        'environment': {'python': platform.python_version(), 'platform': platform.platform(), 'logical_cpus': os.cpu_count(),
                        'memory': memory},
        'initial_counts': json.loads(sql(database, "SELECT json_build_object('wallets', (SELECT COUNT(*) FROM wallets), 'transactions', (SELECT COUNT(*) FROM wager_transactions), 'ledger', (SELECT COUNT(*) FROM wallet_ledger))")),
        'runs': [],
    }
    manifest_path = args.output_dir / 'manifest.json'
    names = ['jungle-compare-' + uuid.uuid4().hex[:8] + '-' + str(index) for index in range(3)]
    failed = False
    for wallet_count in wallets:
        for repeat in range(1, args.repeats + 1):
            # Alternate order to reduce systematic cache/host drift bias.
            variants = requested_variants if repeat % 2 else list(reversed(requested_variants))
            for variant in variants:
                name = variant + '-w' + str(wallet_count) + '-r' + str(repeat)
                target = 'bench_' + uuid.uuid4().hex
                print('Running ' + name, flush=True)
                sql('postgres', 'CREATE DATABASE ' + target)
                try:
                    with snapshot.open('rb') as source:
                        engine('exec', '-i', args.postgres, 'pg_restore', '-U', admin,
                               '-d', target, '--clean', '--if-exists', '--exit-on-error', stdin=source,
                               stdout=subprocess.DEVNULL)
                    if variant == 'optimized':
                        migration_url = urllib.parse.urlunsplit(('postgres',
                            urllib.parse.quote(admin, safe='') + ':' + urllib.parse.quote(pg_environment['POSTGRES_PASSWORD'], safe='')
                            + '@localhost:' + str(args.postgres_port), '/' + target, 'sslmode=disable', ''))
                        subprocess.run([str(args.migrate_binary.resolve()), 'up'], check=True,
                                       env={**os.environ, 'MIGRATION_DATABASE_URL': migration_url})
                    sql(target, 'ANALYZE')
                    parsed = urllib.parse.urlsplit(environment['DATABASE_URL'])
                    runtime_url = parsed._replace(path='/' + target).geturl()
                    binary = args.baseline_binary if variant == 'baseline' else args.optimized_binary
                    for index, container in enumerate(names):
                        command = ['run', '-d', '--name', container, '--network', networks[0],
                                   '--volumes-from', args.app, '-v', str(binary.resolve()) + ':/usr/local/bin/jungle:ro',
                                   '-p', '127.0.0.1:' + str(args.port + index) + ':8081']
                        for key, value in {**environment, 'DATABASE_URL': runtime_url}.items():
                            command.extend(['-e', key + '=' + value])
                        engine(*command, app['Image'], stdout=subprocess.DEVNULL)
                    urls = ['http://localhost:' + str(args.port + index) for index in range(3)]
                    deadline = time.monotonic() + 90
                    for url in urls:
                        while True:
                            try:
                                with urllib.request.urlopen(url + '/health/ready', timeout=2) as response:
                                    if response.status == 200:
                                        break
                            except OSError:
                                pass
                            if time.monotonic() >= deadline:
                                raise RuntimeError('comparison app did not become ready')
                            time.sleep(.25)
                    report = args.output_dir / (name + '.json')
                    command = [sys.executable, str(root / 'scripts/load.py'), '--apis', ','.join(urls),
                               '--issuer', environment['OIDC_ISSUER'], '--wallets', str(wallet_count), '--history', '0',
                               '--concurrency', '16', '--duration', str(args.duration), '--warmup', '3',
                               '--drain-timeout', '600', '--output', str(report)]
                    observations = []
                    stop_monitor = threading.Event()

                    def monitor():
                        query = """SELECT COALESCE(json_agg(row_to_json(s)), '[]'::json) FROM (
                            SELECT clock_timestamp() AS observed_at, pid, state, wait_event_type, wait_event,
                                clock_timestamp() - xact_start AS transaction_age,
                                clock_timestamp() - query_start AS query_age,
                                pg_blocking_pids(pid) AS blocked_by, left(query, 300) AS query
                            FROM pg_stat_activity
                            WHERE datname = '""" + target + "' AND state <> 'idle') s"
                        while not stop_monitor.is_set():
                            try:
                                observations.append(json.loads(sql('postgres', query)))
                            except (subprocess.SubprocessError, ValueError) as error:
                                observations.append({'error': type(error).__name__})
                            stop_monitor.wait(.5)

                    observer = threading.Thread(target=monitor, daemon=True) if args.monitor_locks else None
                    if observer:
                        observer.start()
                    try:
                        with report.with_suffix('.log').open('w') as log:
                            result = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT)
                    finally:
                        stop_monitor.set()
                        if observer:
                            observer.join()
                            (args.output_dir / (name + '-locks.json')).write_text(json.dumps(observations, indent=2) + '\n')
                    data = json.loads(report.read_text()) if report.exists() else {}
                    manifest['runs'].append({'variant': variant, 'wallets': wallet_count, 'repeat': repeat,
                                             'report': report.name, 'exit_code': result.returncode,
                                             'requests_per_second': data.get('requests_per_second'),
                                             'latency_ms': data.get('latency_ms'), 'passed': data.get('passed', False)})
                    failed = failed or result.returncode != 0
                    print(name + ': ' + str(data.get('requests_per_second')) + ' req/s; passed=' + str(data.get('passed')), flush=True)
                    manifest['passed'] = not failed
                    manifest_path.write_text(json.dumps(manifest, indent=2) + '\n')
                    if data.get('baseline'):
                        probes = (root / 'scripts/load-validations.sql').read_text()
                        wallet_id = data['baseline'][0]['wallet_id']
                        raw_plans = engine('exec', '-i', args.postgres, 'psql', '-X', '-q', '-At',
                                       '-v', 'ON_ERROR_STOP=1', '-v', 'wallet_id=' + wallet_id,
                                       '-U', admin, '-d', target, '-f', '-',
                                       input=probes, capture_output=True, text=True).stdout
                        plans = []
                        remaining = raw_plans.strip()
                        while remaining:
                            values, end = json.JSONDecoder().raw_decode(remaining)
                            plans.extend(values)
                            remaining = remaining[end:].strip()
                        (args.output_dir / (name + '-plans.json')).write_text(json.dumps(plans, indent=2) + '\n')
                    if result.returncode != 0:
                        raise RuntimeError('load comparison failed; inspect ' + str(report))
                finally:
                    for index, container in enumerate(names):
                        with (args.output_dir / (name + '-app-' + str(index) + '.log')).open('w') as log:
                            subprocess.run([args.engine, 'logs', container], stdout=log, stderr=subprocess.STDOUT)
                        subprocess.run([args.engine, 'rm', '-f', container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    sql('postgres', 'DROP DATABASE ' + target)
    manifest['ended_at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    manifest['passed'] = not failed
    manifest_path.write_text(json.dumps(manifest, indent=2) + '\n')
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
