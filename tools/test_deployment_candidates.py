import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import deployment_candidates as c


class CandidateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = c.Repo(self.temp.name)
        self.repo.git('init', '-b', 'main')
        self.repo.git('config', 'user.name', 'test')
        self.repo.git('config', 'user.email', 'test@example.invalid')
        for path, content in {c.CATALOG: '[]\n', c.REVIEW: 'null\n', c.VERSION: '0.3.0+r17bi\n',
                              'frontend/src/view.ts': 'old\n'}.items():
            p = self.repo.root / path
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(content)
        self.repo.git('add', '.')
        self.repo.git('commit', '-m', 'baseline')
        self.base = self.repo.git('rev-parse', 'HEAD')
        self.source = self.repo.commit(self.base, {'frontend/src/view.ts': 'new\n'}, 'change')
        self.review = {'source': self.source, 'previous_source': self.base, 'status': 'approved',
                       'official_version': '0.2.17', 'official_source': 'a' * 40,
                       'integration_source': self.base, 'reviewed_by': 'fixture reviewer',
                       'evidence': 'Test-only review; no production assertions',
                       'summary': 'Fixture frontend change', 'official_evidence': 'fixture integration',
                       'compatibility': {'data': 'unchanged', 'config': 'unchanged', 'plugins': 'unchanged',
                                         'runtime': 'unchanged', 'rollback': 'compatible'}}
        self.head = self.with_review(self.source, self.review)
        self.url = 'https://github.com/jzg-lab/sub2api-r3-fix/actions/runs/1'

    def with_review(self, parent, review):
        return self.repo.commit(parent, {c.REVIEW: json.dumps(review)}, 'review')

    def test_freeze_and_publish_matches_existing_production_controller(self):
        plan = c.freeze(self.repo, self.head)
        self.assertTrue(plan['candidate'])
        self.assertEqual(plan['version'], 'v0.2.17+r17.1')
        self.assertEqual(self.repo.changed(self.source, plan['source']), [c.VERSION])
        self.assertEqual(self.repo.read(plan['source'], c.VERSION), '0.2.17+r17.1')
        head = c.add_candidate(self.repo, plan, self.head, self.url)
        items = json.loads(self.repo.read(head, c.CATALOG))
        import sys
        sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'deploy'))
        from managed_deploy import Controller
        self.assertEqual(Controller.validate_catalog(items), items)
        self.assertEqual(items[0]['source'], plan['source'])
        self.assertEqual(items[0]['previous_source'], self.base)
        self.assertIsNone(c.add_candidate(self.repo, plan, head, self.url))

    def test_sequence_continues_across_official_baselines_and_formal_tags(self):
        self.repo.git('tag', 'v0.2.16+r17.10', self.base)
        plan = c.freeze(self.repo, self.head)
        self.assertEqual(plan['version'], 'v0.2.17+r17.11')

    def test_automation_only_change_does_not_allocate(self):
        source = self.repo.commit(self.base, {'tools/deployment_candidates.py': '# automation only\n'}, 'ci')
        head = self.with_review(source, {**self.review, 'source': source})
        self.assertIn('NO_APPLICATION_CHANGE', c.freeze(self.repo, head)['reason'])

    def test_rerun_and_documentation_push_reuse_sha_and_sequence(self):
        first = c.freeze(self.repo, self.head)
        docs = self.repo.commit(self.head, {'docs/notes.md': 'docs only\n'}, 'documentation')
        second = c.freeze(self.repo, docs)
        self.assertEqual(first, second)
        self.assertEqual(len(self.repo.tags()), 1)

    def test_failed_tests_leave_reservation_but_no_catalog_entry(self):
        plan = c.freeze(self.repo, self.head)
        self.assertEqual(json.loads(self.repo.read(self.head, c.CATALOG)), [])
        self.assertEqual(c.freeze(self.repo, self.head)['source'], plan['source'])
        source2 = self.repo.commit(self.head, {'frontend/src/view.ts': 'fixed\n'}, 'fix tests')
        review2 = {**self.review, 'source': source2}
        plan2 = c.freeze(self.repo, self.with_review(source2, review2))
        self.assertEqual(plan2['version'], 'v0.2.17+r17.2')
        self.assertNotEqual(plan2['source'], plan['source'])

    def test_missing_unknown_and_stale_review_block_without_reservation(self):
        self.assertFalse(c.freeze(self.repo, self.source)['candidate'])
        for update in ({'status': 'pending'}, {'evidence': ''}, {'compatibility': {}},
                       {'source': 'main'}, {'official_source': ''}, {'integration_source': 'b' * 40}):
            with self.subTest(update=update):
                head = self.with_review(self.source, {**self.review, **update})
                self.assertFalse(c.freeze(self.repo, head)['candidate'])
        changed = self.repo.commit(self.head, {'frontend/src/view.ts': 'unreviewed\n'}, 'unreviewed')
        self.assertIn('STALE_REVIEW', c.freeze(self.repo, changed)['reason'])
        self.assertEqual(self.repo.tags(), [])

    def test_full_baseline_diff_blocks_special_changes_even_after_clean_push(self):
        for path in ('backend/migrations/999.sql', 'deploy/tool.py', 'backend/internal/config/config.go',
                     'backend/internal/service/plugin_service.go', 'backend/go.mod', 'Dockerfile',
                     'frontend/package.json', 'backend/ent/schema/user.go', 'unknown/runtime.sh'):
            with self.subTest(path=path):
                special = self.repo.commit(self.base, {path: 'changed\n'}, 'special')
                source = self.repo.commit(special, {'frontend/src/view.ts': 'ordinary\n'}, 'ordinary')
                head = self.with_review(source, {**self.review, 'source': source})
                plan = c.freeze(self.repo, head)
                self.assertFalse(plan['candidate'])
                self.assertIn('SPECIAL_REVIEW_REQUIRED', plan['reason'])
        self.assertEqual(self.repo.tags(), [])

    def test_deletion_and_rename_do_not_hide_protected_paths(self):
        original = self.repo.commit(self.base, {'deploy/old.py': 'protected\n'}, 'add old tool')
        # A deletion is still included by --no-renames; a move cannot erase the source path.
        tree = self.repo.git('rev-parse', self.base + '^{tree}')
        deleted = self.repo.git('commit-tree', tree, '-p', original, input=b'delete tool')
        head = self.with_review(deleted, {**self.review, 'source': deleted, 'previous_source': original})
        self.assertIn('deploy/old.py', c.freeze(self.repo, head)['reason'])

    def test_only_docs_do_not_allocate(self):
        source = self.repo.commit(self.base, {'docs/notes.md': 'docs\n'}, 'docs')
        head = self.with_review(source, {**self.review, 'source': source})
        self.assertIn('NO_APPLICATION_CHANGE', c.freeze(self.repo, head)['reason'])

    def test_publish_revalidates_review_and_current_code(self):
        plan = c.freeze(self.repo, self.head)
        changed = self.repo.commit(self.head, {'frontend/src/view.ts': 'later code\n'}, 'new push')
        with self.assertRaisesRegex(ValueError, 'STALE_REVIEW'):
            c.add_candidate(self.repo, plan, changed, self.url)
        revoked = self.with_review(self.head, {**self.review, 'status': 'pending'})
        with self.assertRaisesRegex(ValueError, 'REVIEW_REQUIRED'):
            c.add_candidate(self.repo, plan, revoked, self.url)

    def test_previous_synthetic_candidate_is_valid_baseline(self):
        plan = c.freeze(self.repo, self.head)
        registered = c.add_candidate(self.repo, plan, self.head, self.url)
        source2 = self.repo.commit(registered, {'frontend/src/view.ts': 'next\n'}, 'next change')
        review2 = {**self.review, 'source': source2, 'previous_source': plan['source']}
        plan2 = c.freeze(self.repo, self.with_review(source2, review2))
        self.assertTrue(plan2['candidate'])
        self.assertEqual(plan2['version'], 'v0.2.17+r17.2')

    def test_rewritten_tag_or_duplicate_version_is_rejected(self):
        plan = c.freeze(self.repo, self.head)
        forged = self.repo.commit(self.source, {c.VERSION: '0.2.17+r17.1\n',
                                              'frontend/src/view.ts': 'bad\n'}, 'forged')
        self.repo.git('update-ref', 'refs/tags/' + plan['tag'], forged)
        with self.assertRaisesRegex(ValueError, 'only a version change'):
            c.add_candidate(self.repo, plan, self.head, self.url)

    def test_atomic_publication_loses_race_without_overwriting_main(self):
        remote = self.repo.root / 'remote.git'
        subprocess.run(['git', 'init', '--bare', str(remote)], check=True, capture_output=True)
        self.repo.git('remote', 'add', 'origin', str(remote))
        self.repo.git('push', 'origin', self.head + ':refs/heads/main')
        plan = c.freeze(self.repo, self.head, publish=True)
        registration = c.add_candidate(self.repo, plan, self.head, self.url)
        other = self.repo.commit(self.head, {'docs/concurrent.md': 'concurrent\n'}, 'concurrent')
        self.repo.git('push', 'origin', other + ':refs/heads/main')
        with self.assertRaisesRegex(ValueError, 'git operation failed: push'):
            self.repo.git('push', 'origin', registration + ':refs/heads/main')
        self.assertTrue(self.repo.git('ls-remote', 'origin', 'refs/heads/main').startswith(other))

    def test_cli_freezes_publishes_and_retries_against_local_remote(self):
        remote = self.repo.root / 'remote.git'
        subprocess.run(['git', 'init', '--bare', str(remote)], check=True, capture_output=True)
        self.repo.git('remote', 'add', 'origin', str(remote))
        self.repo.git('push', 'origin', self.head + ':refs/heads/main')
        import sys
        env = {**os.environ, 'GITHUB_REPOSITORY': c.REPOSITORY, 'GITHUB_EVENT_NAME': 'push',
               'GITHUB_REF': 'refs/heads/main', 'GITHUB_SERVER_URL': 'https://github.com',
               'GITHUB_RUN_ID': '1', 'GITHUB_OUTPUT': str(self.repo.root / 'output'),
               'GITHUB_STEP_SUMMARY': str(self.repo.root / 'summary')}

        def run(action, head):
            subprocess.run([sys.executable, c.__file__, action, '--head', head],
                           cwd=self.repo.root, env=env, check=True, capture_output=True)

        run('freeze', self.head)
        plan = json.loads((self.repo.root / 'candidate-plan.json').read_text())
        self.assertIn('candidate=true', (self.repo.root / 'output').read_text())
        self.assertTrue(self.repo.git('ls-remote', 'origin', 'refs/tags/' + plan['tag'])
                        .startswith(plan['source']))
        self.assertEqual(json.loads(self.repo.read(self.head, c.CATALOG)), [])
        run('publish', self.head)
        self.repo.git('fetch', 'origin', 'main')
        published = self.repo.git('rev-parse', 'origin/main')
        self.assertEqual(self.repo.changed(self.head, published), [c.CATALOG])
        items = json.loads(self.repo.read(published, c.CATALOG))
        self.assertEqual(items[0]['source'], plan['source'])
        run('freeze', published)
        self.assertEqual(json.loads((self.repo.root / 'candidate-plan.json').read_text()), plan)
        run('publish', published)
        self.assertTrue(self.repo.git('ls-remote', 'origin', 'refs/heads/main').startswith(published))


if __name__ == '__main__':
    unittest.main()
