import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import check_pnpm_audit_exceptions as audit


class AuditResultTests(unittest.TestCase):
    def test_unavailable_or_malformed_audit_is_not_a_clean_scan(self):
        for data, expected in [({}, 1), ({'error': {'code': 'ECONNRESET'}}, 1),
                               ({'metadata': {'vulnerabilities': {}}, 'advisories': {}}, 0)]:
            with self.subTest(data=data), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / 'audit.json').write_text(json.dumps(data))
                (root / 'exceptions.yml').write_text('version: 1\nexceptions: []\n')
                with patch('sys.argv', ['audit', '--audit', str(root / 'audit.json'),
                                        '--exceptions', str(root / 'exceptions.yml')]), \
                        contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                    self.assertEqual(audit.main(), expected)

    def test_only_live_matching_withdrawal_passes(self):
        ghsa = 'GHSA-fc5q-39xc-vjpf'
        valid = {'ghsa_id': ghsa, 'withdrawn_at': '2026-01-01T00:00:00Z',
                 'vulnerabilities': [{'package': {'ecosystem': 'npm', 'name': 'picomatch'}}]}
        cases = [(valid, True), ({**valid, 'withdrawn_at': None}, False),
                 ({**valid, 'withdrawn_at': 'invalid'}, False),
                 ({**valid, 'withdrawn_at': '2999-01-01T00:00:00Z'}, False),
                 ({**valid, 'ghsa_id': 'GHSA-xxxx-xxxx-xxxx'}, False),
                 ({**valid, 'vulnerabilities': []}, False),
                 ({**valid, 'vulnerabilities': [{'package': {'ecosystem': 'pip', 'name': 'picomatch'}}]}, False),
                 ([], False)]
        for data, expected in cases:
            with self.subTest(data=data):
                response = io.StringIO(json.dumps(data))
                response.url = 'https://api.github.com/advisories/' + ghsa.upper()
                response.status = 200
                with patch.object(audit, 'urlopen', return_value=response), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                    self.assertEqual(audit.advisory_withdrawn('picomatch', ghsa), expected)
        for identifier in ['https://attacker.invalid/advisories/' + ghsa, '../secret', 'CVE-2026-1']:
            with patch.object(audit, 'urlopen') as fetch:
                self.assertFalse(audit.advisory_withdrawn('picomatch', identifier))
                fetch.assert_not_called()
        with patch.object(audit, 'urlopen', side_effect=OSError('offline')), contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(audit.advisory_withdrawn('picomatch', ghsa))
        response = io.StringIO(json.dumps(valid))
        response.url = 'https://attacker.invalid/evidence'
        response.status = 200
        with patch.object(audit, 'urlopen', return_value=response):
            self.assertFalse(audit.advisory_withdrawn('picomatch', ghsa))

    def test_withdrawal_does_not_hide_another_active_advisory(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            data = {'metadata': {'vulnerabilities': {'high': 2}}, 'advisories': {
                'one': {'module_name': 'picomatch', 'severity': 'high', 'github_advisory_id': 'GHSA-fc5q-39xc-vjpf'},
                'two': {'module_name': 'another', 'severity': 'high', 'github_advisory_id': 'GHSA-xxxx-xxxx-xxxx'}}}
            (root / 'audit.json').write_text(json.dumps(data))
            (root / 'exceptions.yml').write_text('version: 1\nexceptions: []\n')
            args = ['audit', '--audit', str(root / 'audit.json'), '--exceptions', str(root / 'exceptions.yml'), '--verify-withdrawals']
            with patch('sys.argv', args), patch.object(audit, 'advisory_withdrawn', side_effect=lambda name, _: name == 'picomatch'), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(audit.main(), 1)
                del data['advisories']['two']
                (root / 'audit.json').write_text(json.dumps(data))
                self.assertEqual(audit.main(), 0)
