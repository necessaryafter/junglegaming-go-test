"""Unit tests for the load report calculations; no external dependencies."""

import unittest
import argparse
import json
from pathlib import Path
import tempfile
from unittest import mock

import load


class ReportTests(unittest.TestCase):
    def test_access_token_is_renewed_before_expiration(self):
        api = load.API(['http://localhost:8081'], 'http://localhost:8080', 1)
        raw = b'{"access_token":"test-credential","expires_in":60}'
        with mock.patch.object(api, 'request', return_value=(200, raw)) as request:
            with mock.patch.object(load.time, 'monotonic', return_value=0):
                self.assertEqual(api.token('provider-a'), 'test-credential')
            with mock.patch.object(load.time, 'monotonic', return_value=29):
                api.token('provider-a')
            self.assertEqual(request.call_count, 1)
            with mock.patch.object(load.time, 'monotonic', return_value=31):
                api.token('provider-a')
            self.assertEqual(request.call_count, 2)

    def test_unavailable_outbox_is_not_treated_as_empty(self):
        clock = iter([0, .5, 1.5, 2])
        with mock.patch.object(load, 'metrics', return_value={'api': None}):
            with mock.patch.object(load.time, 'monotonic', side_effect=lambda: next(clock)):
                result = load.drain_outbox(None, 1)
        self.assertFalse(result['drained'])
        self.assertEqual(len(result['observations']), 1)

    def test_fixture_changed_since_preparation_is_rejected(self):
        class Client:
            def __init__(self, *args):
                self.urls = ['http://localhost:8081']

            def request(self, *args):
                return 200, b'{}'

            def token(self, *args):
                return 'unused-test-credential'

            def call(self, *args):
                return 200, {'playerId': 'player', 'version': 3, 'balance': {'amount': '1000000.00'}}

        with tempfile.TemporaryDirectory() as directory:
            fixture = Path(directory) / 'fixture.json'
            fixture.write_text(json.dumps({'scenario': 'write', 'history': 0,
                                           'wallets': [{'id': 'wallet', 'playerId': 'player'}],
                                           'controls': [], 'baseline': [{'minor': 100000000, 'version': 1}]}))
            args = argparse.Namespace(apis='http://localhost:8081', issuer='http://localhost:8080',
                                      timeout=1, wallets=1, history=0, scenario='write', fixture=fixture)
            with mock.patch.object(load, 'parse_args', return_value=args):
                with mock.patch.object(load, 'API', Client):
                    with self.assertRaisesRegex(RuntimeError, 'changed since preparation'):
                        load.main()

    def test_financial_verification_checks_balance_version_entries_and_currency(self):
        for name, change, expected in [
            ('valid', lambda s, r: None, True),
            ('wrong balance', lambda s, r: s['balance'].update(amount='999999.98'), False),
            ('wrong version', lambda s, r: s.update(version=3), False),
            ('extra entry', lambda s, r: r.update(checkedEntries=3), False),
            ('wrong currency', lambda s, r: s['balance'].update(currency='USD'), False),
            ('inconsistent ledger', lambda s, r: r.update(consistent=False), False),
        ]:
            with self.subTest(name=name):
                snapshot = {'version': 2, 'balance': {'amount': '999999.99', 'currency': 'BRL'}}
                result = {'consistent': True, 'checkedEntries': 2,
                          'storedBalance': {'amount': '999999.99', 'currency': 'BRL'},
                          'calculatedBalance': {'amount': '999999.99', 'currency': 'BRL'},
                          'difference': {'amount': '0.00', 'currency': 'BRL'}}
                change(snapshot, result)
                api = mock.Mock()
                api.call.side_effect = [(200, snapshot), (200, result)]
                verified, unresolved = load.verify(api, [{'id': 'wallet'}],
                                                    [{'minor': 100000000, 'version': 1}], {0: 1}, [])
                self.assertEqual(verified[0]['consistent'], expected)
                self.assertEqual(unresolved, [])

    def test_money_remains_exact(self):
        for value, expected in [('0.00', 0), ('0.01', 1), ('1000000.00', 100000000), ('9007199254740993.01', 900719925474099301)]:
            with self.subTest(value=value):
                self.assertEqual(load.minor_units(value), expected)
        for value in ['0.1', 'NaN', '-1.00', '1e3', '1.001', '1', '1.00\n']:
            with self.subTest(value=value):
                with self.assertRaises(ValueError):
                    load.minor_units(value)

    def test_nearest_rank_percentiles(self):
        values = list(range(100, 0, -1))
        for quantile, expected in [(.50, 50), (.95, 95), (.99, 99), (1, 100)]:
            with self.subTest(quantile=quantile):
                self.assertEqual(load.percentile(values, quantile), expected)
        self.assertIsNone(load.percentile([], .95))

    def test_missing_counter_at_baseline_starts_at_zero(self):
        metric = 'jungle_database_conflicts_total{kind="unique"}'
        samples = [
            {'instances': {'api': {'jungle_database_collection_success': 1}}},
            {'instances': {'api': {'jungle_database_collection_success': 1, metric: 7}}},
        ]
        report = load.summarize_telemetry(samples)
        self.assertEqual(report['counter_deltas']['api ' + metric], 7)
        self.assertIsNone(report['outbox_oldest_seconds_max'])

    def test_reset_and_collection_failure_are_visible(self):
        metric = 'jungle_conflicts_total{transport="http"}'
        samples = [
            {'instances': {'api': {'jungle_database_collection_success': 1, metric: 5}}},
            {'instances': {'api': {'jungle_database_collection_success': 1, metric: 3}}},
            {'instances': {'api': None}},
        ]
        report = load.summarize_telemetry(samples)
        self.assertEqual(len(report['counter_resets']), 1)
        self.assertEqual(report['unavailable_samples'], 1)

    def test_global_backlog_is_not_summed_across_instances(self):
        values = {'jungle_database_collection_success': 1, 'jungle_outbox_pending': 10, 'jungle_outbox_oldest_seconds': 2}
        report = load.summarize_telemetry([{'instances': {'a': values, 'b': values}}])
        self.assertEqual(report['outbox_pending_max'], 10)
        self.assertEqual(report['outbox_oldest_seconds_max'], 2)


if __name__ == '__main__':
    unittest.main()
