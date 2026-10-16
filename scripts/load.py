#!/usr/bin/env python3
"""Bounded closed-loop HTTP load with exact-money reconciliation and JSON evidence."""

import argparse
import collections
import concurrent.futures
import datetime
import http.client
import json
import math
import os
from pathlib import Path
import platform
import re
import threading
import time
import urllib.parse
import uuid


class API:
    def __init__(self, urls, issuer, timeout):
        self.urls = urls
        self.issuer = issuer.rstrip('/')
        self.timeout = timeout
        self.local = threading.local()
        self.lock = threading.Lock()
        self.tokens = {}

    def close(self):
        # Connections are owned by their worker thread; request failures close them early.
        for connection in getattr(self.local, 'connections', {}).values():
            connection.close()
        self.local.connections = {}

    def request(self, url, method='GET', payload=None, headers=None):
        parsed = urllib.parse.urlsplit(url)
        connections = getattr(self.local, 'connections', {})
        self.local.connections = connections
        origin = (parsed.scheme, parsed.netloc)
        connection = connections.get(origin)
        if connection is None:
            factory = http.client.HTTPSConnection if parsed.scheme == 'https' else http.client.HTTPConnection
            connection = factory(parsed.hostname, parsed.port, timeout=self.timeout)
            connections[origin] = connection
        path = parsed.path or '/'
        if parsed.query:
            path += '?' + parsed.query
        try:
            connection.request(method, path, body=payload, headers=headers or {})
            response = connection.getresponse()
            body = response.read(1048577)
            if len(body) > 1048576:
                raise ValueError('response exceeds size limit')
            return response.status, body
        except Exception:
            connection.close()
            connections.pop(origin, None)
            raise

    def token(self, client):
        with self.lock:
            token, deadline = self.tokens.get(client, ('', 0))
            if time.monotonic() < deadline:
                return token
            secret = os.environ.get(client.upper().replace('-', '_') + '_SECRET', client + '-local-secret')
            data = urllib.parse.urlencode({'grant_type': 'client_credentials', 'client_id': client, 'client_secret': secret}).encode()
            # Token renewal is outside financial writes; discard an idle IdP connection.
            parsed = urllib.parse.urlsplit(self.issuer)
            connection = getattr(self.local, 'connections', {}).pop((parsed.scheme, parsed.netloc), None)
            if connection is not None:
                connection.close()
            status, raw = self.request(self.issuer + '/protocol/openid-connect/token', 'POST', data, {'Content-Type': 'application/x-www-form-urlencoded'})
            if status != 200:
                raise RuntimeError('IdP authentication failed with status ' + str(status))
            result = json.loads(raw)
            lifetime = int(result['expires_in'])
            if lifetime <= 0:
                raise ValueError('IdP issued a nonpositive token lifetime')
            self.tokens[client] = (result['access_token'], time.monotonic() + max(1, lifetime - min(30, lifetime // 2)))
            return result['access_token']

    def call(self, path, client='jungle-internal', method='GET', payload=None, key=None, api=0):
        headers = {'Authorization': 'Bearer ' + self.token(client)}
        data = None
        if payload is not None:
            data = json.dumps(payload, separators=(',', ':')).encode()
            headers['Content-Type'] = 'application/json'
        if key is not None:
            headers['Idempotency-Key'] = key
        status, raw = self.request(self.urls[api % len(self.urls)] + path, method, data, headers)
        return status, json.loads(raw)


def minor_units(value):
    if not re.fullmatch(r'[0-9]+\.[0-9]{2}', value):
        raise ValueError('invalid money in API response')
    whole, fractional = value.split('.')
    return int(whole) * 100 + int(fractional)


def percentile(values, quantile):
    if not values:
        return None
    ordered = sorted(values)
    return ordered[max(0, math.ceil(quantile * len(ordered)) - 1)]


def operation(wallet, run_id, kind='BET'):
    external = run_id + ':' + str(uuid.uuid4())
    return {
        'providerId': 'provider-a', 'externalTransactionId': external,
        'playerId': wallet['playerId'], 'walletId': wallet['id'],
        'roundId': run_id, 'gameId': 'load-test', 'kind': kind,
        'money': {'amount': '0.01', 'currency': 'BRL'},
    }


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def metrics(api):
    result = {}
    for index, url in enumerate(api.urls):
        try:
            status, raw = api.request(url + '/metrics')
            require(status == 200, 'metrics status ' + str(status))
            samples = {}
            for line in raw.decode().splitlines():
                if not line or line.startswith('#'):
                    continue
                name, value = line.rsplit(' ', 1)
                if name.startswith(('jungle_outbox_', 'jungle_conflicts_total', 'jungle_database_conflicts_total', 'jungle_database_collection_success', 'jungle_database_last_success_timestamp_seconds')):
                    # Financial values never use float; these are telemetry seconds/counters.
                    number = float(value)
                    samples[name] = number if math.isfinite(number) else None
            result[url] = samples
        except (OSError, ValueError, RuntimeError, http.client.HTTPException):
            result[url] = None
    return result


def sample_metrics(api, stop, samples, started):
    try:
        while not stop.is_set():
            samples.append({'elapsed_seconds': time.monotonic() - started, 'instances': metrics(api)})
            stop.wait(1)
    finally:
        api.close()


def prepare(api, args, run_id):
    wallets = []
    for index in range(args.wallets):
        status, wallet = api.call('/wallets', method='POST', payload={
            'playerId': str(uuid.uuid4()),
            'initialBalance': {'amount': '1000000.00', 'currency': 'BRL'},
        }, api=index)
        require(status == 201, 'wallet creation failed: ' + str(status))
        wallets.append(wallet)

    def seed(pair):
        index, wallet = pair
        try:
            for _ in range(args.history):
                body = operation(wallet, run_id, 'WIN')
                status, result = api.call('/wagering/transactions', 'provider-a', 'POST', body, body['externalTransactionId'], index)
                require(status == 200 and result.get('status') == 'PROCESSED' and result.get('idempotentReplay') is False, 'history seeding failed: ' + str(status))
        finally:
            api.close()

    with concurrent.futures.ThreadPoolExecutor(max_workers=min(args.wallets, args.concurrency)) as executor:
        list(executor.map(seed, enumerate(wallets)))

    api.close()
    controls = []
    if args.scenario != 'write':
        for index, wallet in enumerate(wallets):
            body = operation(wallet, run_id)
            status, result = api.call('/wagering/transactions', 'provider-a', 'POST', body, body['externalTransactionId'], index)
            require(status == 200 and result.get('status') == 'PROCESSED', 'control transaction failed')
            if args.scenario == 'conflict':
                body['money'] = {'amount': '0.02', 'currency': 'BRL'}
            controls.append(body)
    return wallets, controls


def exercise(api, args, wallets, controls, run_id, duration):
    started = time.monotonic()
    deadline = started + duration
    barrier = threading.Barrier(args.concurrency + 1)
    lock = threading.Lock()
    issued = 0

    def worker(worker_index):
        nonlocal issued
        latencies = []
        statuses = collections.Counter()
        outcomes = collections.Counter()
        applied = collections.Counter()
        errors = collections.Counter()
        unknown = []
        try:
            barrier.wait()
            while time.monotonic() < deadline:
                with lock:
                    if issued >= args.max_requests:
                        break
                    sequence = issued
                    issued += 1
                index = sequence % len(wallets)
                body = operation(wallets[index], run_id) if args.scenario == 'write' else controls[index]
                request_started = time.monotonic()
                try:
                    status, result = api.call('/wagering/transactions', 'provider-a', 'POST', body, body['externalTransactionId'], worker_index + sequence)
                    statuses[str(status)] += 1
                    if args.scenario == 'conflict' and status == 409 and result.get('code') == 'CONFLICT':
                        outcomes['expected_conflict'] += 1
                    elif status == 200 and result.get('status') == 'PROCESSED':
                        replay = result.get('idempotentReplay')
                        if args.scenario == 'write' and replay is not False:
                            errors['unexpected_replay_result'] += 1
                        elif args.scenario == 'replay' and replay is not True:
                            errors['unexpected_replay_result'] += 1
                        elif args.scenario == 'conflict':
                            errors['conflicting_body_accepted'] += 1
                        else:
                            outcomes['replay' if replay else 'processed'] += 1
                            if args.scenario == 'write':
                                if replay:
                                    errors['unique_operation_replayed'] += 1
                                applied[index] += 1
                    else:
                        errors['http_' + str(status) + '_' + str(result.get('code', result.get('failureCode', 'unexpected')))] += 1
                        if args.scenario == 'write' and status >= 500:
                            unknown.append((index, body['externalTransactionId']))
                except (OSError, ValueError, RuntimeError, http.client.HTTPException) as error:
                    statuses['transport_error'] += 1
                    errors[type(error).__name__] += 1
                    if args.scenario == 'write':
                        unknown.append((index, body['externalTransactionId']))
                latencies.append((time.monotonic() - request_started) * 1000)
        finally:
            api.close()
        return latencies, statuses, outcomes, applied, errors, unknown

    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as executor:
        futures = [executor.submit(worker, index) for index in range(args.concurrency)]
        # Readiness and auth happen before the timed phase. The deadline starts here.
        started = time.monotonic()
        deadline = started + duration
        barrier.wait()
        results = [future.result() for future in futures]
    elapsed = time.monotonic() - started
    merged = ([], collections.Counter(), collections.Counter(), collections.Counter(), collections.Counter(), [])
    for result in results:
        for destination, source in zip(merged, result):
            if isinstance(destination, list):
                destination.extend(source)
            else:
                destination.update(source)
    return elapsed, merged


def verify(api, wallets, baseline, applied, unknown):
    unresolved = []
    for index, external in unknown:
        status, result = api.call('/providers/provider-a/wagering/transactions/' + urllib.parse.quote(external, safe=''), 'provider-a')
        if status == 200 and result.get('status') == 'PROCESSED':
            applied[index] += 1
        elif status != 404:
            unresolved.append(external)
    results = []
    for index, wallet in enumerate(wallets):
        status, snapshot = api.call('/wallets/' + wallet['id'])
        require(status == 200, 'final wallet read failed')
        status, reconciliation = api.call('/wallets/' + wallet['id'] + '/reconciliation', method='POST')
        require(status == 200, 'reconciliation request failed')
        expected_minor = baseline[index]['minor'] - applied[index]
        expected_entries = baseline[index]['version'] + applied[index]
        consistent = (
            not unresolved and reconciliation.get('consistent') is True
            and snapshot['balance']['currency'] == 'BRL'
            and reconciliation['storedBalance']['currency'] == 'BRL'
            and reconciliation['calculatedBalance']['currency'] == 'BRL'
            and reconciliation['difference']['currency'] == 'BRL'
            and minor_units(snapshot['balance']['amount']) == expected_minor
            and snapshot['version'] == expected_entries
            and reconciliation['checkedEntries'] == expected_entries
            and minor_units(reconciliation['storedBalance']['amount']) == expected_minor
            and minor_units(reconciliation['calculatedBalance']['amount']) == expected_minor
            and reconciliation['difference']['amount'] == '0.00'
        )
        results.append({'wallet_id': wallet['id'], 'consistent': consistent,
                        'balance': snapshot['balance'], 'version': snapshot['version'],
                        'expected_minor_units': expected_minor, 'checked_entries': reconciliation['checkedEntries']})
    return results, unresolved


def summarize_telemetry(samples):
    ages = []
    pending = []
    counter_series = collections.defaultdict(list)
    unavailable = 0
    for sample in samples:
        for url, values in sample['instances'].items():
            if values is None or values.get('jungle_database_collection_success') != 1:
                unavailable += 1
                continue
            age = values.get('jungle_outbox_oldest_seconds')
            depth = values.get('jungle_outbox_pending')
            if age is not None:
                ages.append(age)
            if depth is not None:
                pending.append(depth)
            for name, value in values.items():
                if name.startswith(('jungle_conflicts_total', 'jungle_database_conflicts_total')) and value is not None:
                    counter_series[(url, name)].append(value)
    resets = []
    deltas = {}
    baseline = samples[0]['instances'] if samples else {}
    for (url, name), values in counter_series.items():
        if any(b < a for a, b in zip(values, values[1:])):
            resets.append({'api': url, 'metric': name})
        initial = baseline.get(url)
        if initial is None:
            deltas[url + ' ' + name] = None
        else:
            deltas[url + ' ' + name] = max(0, values[-1] - initial.get(name, 0))
    return {'outbox_oldest_seconds_max': max(ages, default=None),
            'outbox_pending_max': max(pending, default=None),
            'unavailable_samples': unavailable, 'counter_deltas': deltas, 'counter_resets': resets}


def drain_outbox(api, timeout):
    started = time.monotonic()
    fresh_after = time.time()
    observations = []
    while True:
        values = metrics(api)
        observations.append({'elapsed_seconds': time.monotonic() - started, 'instances': values})
        drained = all(
            item is not None and item.get('jungle_database_collection_success') == 1
            and item.get('jungle_database_last_success_timestamp_seconds', 0) >= fresh_after
            and item.get('jungle_outbox_pending') == 0
            for item in values.values()
        )
        if drained or time.monotonic() - started >= timeout:
            return {'drained': drained, 'seconds': time.monotonic() - started, 'instances': values,
                    'started_at': datetime.datetime.fromtimestamp(fresh_after, datetime.timezone.utc).isoformat(),
                    'ended_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                    'observations': observations}
        time.sleep(1)


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apis', default=os.environ.get('TEST_API_URLS', 'http://localhost:8081'))
    parser.add_argument('--issuer', default=os.environ.get('TEST_OIDC_ISSUER', 'http://localhost:8080/realms/jungle'))
    parser.add_argument('--scenario', choices=['write', 'replay', 'conflict'], default='write')
    parser.add_argument('--wallets', type=int, default=1)
    parser.add_argument('--history', type=int, default=0, help='WIN entries seeded per wallet, outside timing')
    parser.add_argument('--concurrency', type=int, default=16)
    parser.add_argument('--duration', type=float, default=30)
    parser.add_argument('--warmup', type=float, default=3)
    parser.add_argument('--max-requests', type=int, default=100000)
    parser.add_argument('--timeout', type=float, default=15)
    parser.add_argument('--drain-timeout', type=float, default=300)
    parser.add_argument('--output', type=Path, default=Path('.local/load/result.json'))
    parser.add_argument('--fixture', type=Path, help='resume an unchanged prepared fixture, with --warmup 0')
    args = parser.parse_args()
    for name in ['wallets', 'concurrency', 'max_requests']:
        if getattr(args, name) <= 0 or getattr(args, name) > 100000:
            parser.error(name + ' must be between 1 and 100000')
    if args.concurrency > 256 or args.wallets > 256:
        parser.error('concurrency and wallets must be at most 256')
    if args.history < 0 or args.history > 100000:
        parser.error('history must be between 0 and 100000')
    for name in ['duration', 'timeout', 'drain_timeout']:
        if not math.isfinite(getattr(args, name)) or getattr(args, name) <= 0:
            parser.error(name + ' must be positive and finite')
    if not math.isfinite(args.warmup) or args.warmup < 0:
        parser.error('warmup must be nonnegative and finite')
    if args.fixture is not None and args.warmup != 0:
        parser.error('fixture reuse requires --warmup 0')
    for url in [*args.apis.split(','), args.issuer]:
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme not in ['http', 'https'] or not parsed.netloc or parsed.username or parsed.password:
            parser.error('API and issuer URLs must be HTTP(S) without embedded credentials')
    return args


def main():
    args = parse_args()
    run_id = 'load-' + uuid.uuid4().hex
    api = API([url.strip().rstrip('/') for url in args.apis.split(',')], args.issuer, args.timeout)
    for url in api.urls:
        status, _ = api.request(url + '/health/ready')
        require(status == 200, url + ' is not ready')
    api.token('jungle-internal')
    api.token('provider-a')
    print('Preparing ' + run_id + '; wallets=' + str(args.wallets) + '; history=' + str(args.history), flush=True)
    prepare_started = time.monotonic()
    if args.fixture is not None:
        fixture = json.loads(args.fixture.read_text())
        require(fixture['scenario'] == args.scenario and fixture['history'] == args.history, 'fixture configuration differs')
        wallets, controls, baseline = fixture['wallets'], fixture['controls'], fixture['baseline']
        require(len(wallets) == args.wallets and len(baseline) == args.wallets, 'fixture wallet count differs')
        for wallet, expected in zip(wallets, baseline):
            status, snapshot = api.call('/wallets/' + wallet['id'])
            require(status == 200 and snapshot['playerId'] == wallet['playerId'], 'fixture wallet identity differs')
            require(snapshot['version'] == expected['version'], 'fixture has changed since preparation')
            require(minor_units(snapshot['balance']['amount']) == expected['minor'], 'fixture balance differs')
    else:
        wallets, controls = prepare(api, args, run_id)
        warmup_applied = collections.Counter()
        if args.warmup:
            _, warmup_result = exercise(api, args, wallets, controls, run_id, args.warmup)
            require(not warmup_result[4], 'warmup returned unexpected errors: ' + str(dict(warmup_result[4])))
            warmup_applied = warmup_result[3]
        baseline = []
        for index, wallet in enumerate(wallets):
            status, snapshot = api.call('/wallets/' + wallet['id'])
            require(status == 200, 'baseline read failed')
            debits = warmup_applied[index] + (1 if controls else 0)
            require(snapshot['version'] == 1 + args.history + debits, 'unexpected baseline version')
            require(minor_units(snapshot['balance']['amount']) == 100000000 + args.history - debits, 'unexpected baseline balance')
            baseline.append({'minor': minor_units(snapshot['balance']['amount']), 'version': snapshot['version']})
    fixture_path = args.output.with_suffix('.fixture.json')
    fixture_path.parent.mkdir(parents=True, exist_ok=True)
    fixture_path.write_text(json.dumps({'run_id': run_id, 'scenario': args.scenario,
                                      'history': args.history, 'wallets': wallets,
                                      'controls': controls, 'baseline': baseline}, indent=2) + '\n')
    drain_before = drain_outbox(api, args.drain_timeout)
    if not drain_before['drained']:
        args.output.write_text(json.dumps({'run_id': run_id, 'passed': False, 'stage': 'prepare',
                                          'error': 'outbox did not drain before measurement',
                                          'outbox_before': drain_before, 'fixture': str(fixture_path)}, indent=2) + '\n')
        raise RuntimeError('outbox did not drain before measurement; fixture saved at ' + str(fixture_path))
    api.token('provider-a')
    prepare_seconds = time.monotonic() - prepare_started
    started_at = datetime.datetime.now(datetime.timezone.utc).isoformat()
    samples = [{'elapsed_seconds': 0, 'instances': metrics(api)}]
    stop = threading.Event()
    sampler = threading.Thread(target=sample_metrics, args=(api, stop, samples, time.monotonic()), daemon=True)
    sampler.start()
    print('Measuring ' + str(args.duration) + ' seconds at concurrency ' + str(args.concurrency), flush=True)
    try:
        elapsed, (latencies, statuses, outcomes, applied, errors, unknown) = exercise(api, args, wallets, controls, run_id, args.duration)
    finally:
        stop.set()
        sampler.join()
    ended_at = datetime.datetime.now(datetime.timezone.utc).isoformat()
    verification, unresolved = verify(api, wallets, baseline, applied, unknown)
    samples.append({'elapsed_seconds': elapsed, 'instances': metrics(api)})
    drain_after = drain_outbox(api, args.drain_timeout)
    requests = len(latencies)
    successful = sum(outcomes.values())
    report = {
        'run_id': run_id, 'started_at': started_at, 'ended_at': ended_at,
        'configuration': {key: str(value) if isinstance(value, Path) else value for key, value in vars(args).items()},
        'environment': {'python': platform.python_version(), 'platform': platform.platform(), 'logical_cpus': os.cpu_count()},
        'prepare_seconds': prepare_seconds, 'measurement_seconds': elapsed,
        'baseline': [{'wallet_id': wallet['id'], **state} for wallet, state in zip(wallets, baseline)],
        'requests': requests, 'requests_per_second': requests / elapsed,
        'expected_results_per_second': successful / elapsed,
        'latency_ms': {name: percentile(latencies, q) for name, q in [('p50', .50), ('p95', .95), ('p99', .99)]},
        'http_statuses': dict(statuses), 'outcomes': dict(outcomes), 'errors': dict(errors),
        'error_rate': sum(errors.values()) / requests if requests else None,
        'telemetry': summarize_telemetry(samples), 'telemetry_samples': samples,
        'verification': verification, 'unresolved_operations': unresolved,
        'outbox_before': drain_before, 'outbox_after': drain_after,
        'drain_telemetry': summarize_telemetry(drain_after['observations']),
    }
    report['passed'] = bool(requests) and not errors and drain_after['drained'] and all(item['consistent'] for item in verification)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2, allow_nan=False) + '\n')
    api.close()
    print(json.dumps({key: report[key] for key in ['run_id', 'requests', 'requests_per_second', 'latency_ms', 'http_statuses', 'errors', 'passed']}, indent=2))
    print('Report: ' + str(args.output))
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__':
    main()
