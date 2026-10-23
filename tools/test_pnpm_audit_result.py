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
