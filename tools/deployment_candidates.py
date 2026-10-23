#!/usr/bin/env python3
"""Freeze reviewed source and publish a catalog entry. Never contact production."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

CATALOG = '.github/deployment-candidates.json'
REVIEW = '.github/deployment-review.json'
VERSION = 'backend/cmd/server/VERSION'
TAG_PREFIX = 'deployment-candidate/r17.'
SHA = re.compile(r'[0-9a-f]{40}')
RELEASE = re.compile(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\+r17\.([1-9][0-9]*)')
REPOSITORY = 'jzg-lab/sub2api-r3-fix'


def require(ok, message):
    if not ok:
        raise ValueError(message)


def metadata(path):
    return (path in (CATALOG, REVIEW, 'AGENTS.md', 'README.md', 'README_CN.md',
                     'DEV_GUIDE.md', 'RELEASE-POLICY.md') or path.startswith('docs/'))


def blocked(path):
    # These are hard stops even when a review claims compatibility. Renames include both names.
    return (path.startswith(('deploy/', 'tools/', '.github/workflows/',
                             'backend/migrations/', 'backend/ent/',
                             'backend/internal/config/', 'backend/internal/setup/',
                             'backend/pkg/pluginapi/', 'backend/internal/pkg/deployment/')) or
            path in ('Dockerfile', 'Makefile', 'backend/Makefile', 'backend/go.mod', 'backend/go.sum',
                     'frontend/package.json', 'frontend/pnpm-lock.yaml',
                     'frontend/pnpm-workspace.yaml', '.npmrc', 'frontend/.npmrc') or
            (not path.startswith('backend/internal/web/dist/') and
             ('plugin' in path.lower() or 'migrations_runner' in path or
              'handoff' in path or
              (path.startswith('backend/') and 'deployment' in path))))


def ordinary_path(path):
    return (metadata(path) or path == VERSION or
            path.startswith(('frontend/src/', 'frontend/public/', 'backend/internal/',
                             'backend/cmd/', 'backend/pkg/', 'backend/tests/')))


class Repo:
    def __init__(self, root='.'):
        self.root = Path(root).resolve()

    def git(self, *args, input=None, env=None):
        result = subprocess.run(['git', '-C', str(self.root), *args], input=input,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                env={**os.environ, **(env or {})})
        require(result.returncode == 0, 'git operation failed: ' + args[0] + '\n' + result.stderr.decode())
        return result.stdout.decode().strip()

    def sha(self, ref):
        require(SHA.fullmatch(ref) is not None, 'full source SHA required')
        require(self.git('rev-parse', ref + '^{commit}') == ref, 'source is not a commit')
        return ref

    def read(self, ref, path):
        return self.git('show', ref + ':' + path)

    def changed(self, base, head):
        output = self.git('diff', '--no-renames', '--name-only', '-z', base, head)
        return [p for p in output.split('\0') if p]

    def ancestor(self, base, head):
        return self.git('merge-base', base, head) == base

    def tags(self):
        return self.git('tag', '--list', TAG_PREFIX + '*').splitlines()

    def commit(self, parent, changes, message):
        # A private index avoids touching a developer's index or worktree.
        import tempfile
        with tempfile.TemporaryDirectory() as temp:
            env = {'GIT_INDEX_FILE': temp + '/index',
                   'GIT_AUTHOR_NAME': 'github-actions[bot]',
                   'GIT_AUTHOR_EMAIL': '41898282+github-actions[bot]@users.noreply.github.com',
                   'GIT_COMMITTER_NAME': 'github-actions[bot]',
                   'GIT_COMMITTER_EMAIL': '41898282+github-actions[bot]@users.noreply.github.com'}
            self.git('read-tree', parent, env=env)
            for path, data in changes.items():
                blob = self.git('hash-object', '-w', '--stdin', input=data.encode())
                self.git('update-index', '--add', '--cacheinfo', '100644,' + blob + ',' + path, env=env)
            tree = self.git('write-tree', env=env)
            return self.git('commit-tree', tree, '-p', parent, input=message.encode(), env=env)


def validate_catalog(items):
    require(isinstance(items, list) and len(items) <= 100, 'invalid or full candidate catalog')
    sources, sequences, identifiers = set(), set(), set()
    for item in items:
        match = RELEASE.fullmatch(item['version'])
        require(match is not None, 'invalid catalog version')
        sequence = int(match[4])
        require(sequence not in sequences and item['source'] not in sources and item['id'] not in identifiers,
                'duplicate version, source or candidate ID')
        require(SHA.fullmatch(item['source']) and SHA.fullmatch(item['previous_source']), 'invalid catalog SHA')
        require(item['channel'] in ('ordinary', 'special') and item['review'].strip(), 'invalid catalog review')
        sources.add(item['source'])
        sequences.add(sequence)
        identifiers.add(item['id'])
    return items


def review_for(repo, head, allow_special=False):
    review = json.loads(repo.read(head, REVIEW))
    require(isinstance(review, dict), 'MISSING_REVIEW: a maintainer must review this source first')
    require(review.get('status') == 'approved', 'REVIEW_REQUIRED')
    source = repo.sha(review['source'])
    previous = repo.sha(review['previous_source'])
    require(repo.ancestor(source, head), 'review source is not an ancestor of the push')
    require(all(metadata(p) for p in repo.changed(source, head)), 'STALE_REVIEW: code changed after review')
    # A prior candidate is a version-only child of a main commit, not an ancestor of main.
    if not repo.ancestor(previous, source):
        parent = repo.git('rev-parse', previous + '^')
        require(repo.changed(parent, previous) == [VERSION], 'previous source has unrelated history')
        require(repo.ancestor(parent, source), 'previous source has unrelated history')
    paths = repo.changed(previous, source)
    denied = [p for p in paths if blocked(p) or not ordinary_path(p)]
    channel = review.get('channel', 'ordinary')
    require(channel in ('ordinary', 'special'), 'invalid review channel')
    if channel == 'special':
        require(allow_special, 'SPECIAL_MANUAL_DISPATCH_REQUIRED')
        require(any(not metadata(p) and p != VERSION for p in paths), 'NO_APPLICATION_CHANGE')
        compatibility = review.get('compatibility', {})
        require(set(compatibility) == {'data', 'config', 'plugins', 'runtime', 'rollback'} and
                all(compatibility[k] in ('unchanged', 'reviewed_change')
                    for k in ('data', 'config', 'plugins', 'runtime')) and
                compatibility['rollback'] in ('compatible', 'conditional'), 'COMPATIBILITY_UNKNOWN')
        for field in ('special_plan', 'rollback_plan'):
            require(isinstance(review.get(field), str) and 0 < len(review[field].strip()) <= 4000,
                    'missing or oversized ' + field)
    else:
        require(not denied, 'SPECIAL_REVIEW_REQUIRED: ' + ', '.join(denied))
        require(any(p != VERSION and p.startswith(('frontend/src/', 'frontend/public/', 'backend/'))
                    for p in paths), 'NO_APPLICATION_CHANGE')
        require(review.get('compatibility') == {
            'data': 'unchanged', 'config': 'unchanged', 'plugins': 'unchanged',
            'runtime': 'unchanged', 'rollback': 'compatible'}, 'COMPATIBILITY_UNKNOWN')
    for field in ('reviewed_by', 'evidence', 'summary', 'official_evidence'):
        require(isinstance(review.get(field), str) and 0 < len(review[field].strip()) <= 4000,
                'missing or oversized ' + field)
    require(re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)',
                         review.get('official_version', '')), 'official baseline version required')
    repo.sha(review['integration_source'])
    require(repo.ancestor(review['integration_source'], source), 'official integration is not in source history')
    require(SHA.fullmatch(review.get('official_source', '')), 'official source SHA required')
    return review


def reservation(repo, tag):
    source = repo.git('rev-parse', tag + '^{commit}')
    parent = repo.git('rev-parse', source + '^')
    require(repo.changed(parent, source) == [VERSION], 'candidate tag must contain only a version change')
    version = 'v' + repo.read(source, VERSION).removeprefix('v')
    require(RELEASE.fullmatch(version) and version.endswith('+r17.' + tag.removeprefix(TAG_PREFIX)),
            'candidate tag/version mismatch')
    return source, parent, version


def freeze(repo, head, publish=False, allow_special=False):
    repo.sha(head)
    plan = {'source': head, 'candidate': False}
    try:
        review = review_for(repo, head, allow_special)
    except (ValueError, KeyError, TypeError) as error:
        return {**plan, 'reason': str(error)}
    # Reviews are metadata; freeze the reviewed code, never later unreviewed commits.
    original = review['source']
    catalog = validate_catalog(json.loads(repo.read(head, CATALOG)))
    sequences = [int(RELEASE.fullmatch(c['version'])[4]) for c in catalog]
    # Formal releases and an explicitly committed version share the same global counter.
    versions = repo.git('tag', '--list', 'v*+r17.*').splitlines()
    versions.append('v' + repo.read(original, VERSION).removeprefix('v'))
    sequences.extend(int(match[4]) for version in versions if (match := RELEASE.fullmatch(version)))
    found = None
    for tag in repo.tags():
        suffix = tag.removeprefix(TAG_PREFIX)
        require(re.fullmatch(r'[1-9][0-9]*', suffix), 'invalid reserved sequence')
        sequences.append(int(suffix))
        source, parent, version = reservation(repo, tag)
        if parent == original:
            require(found is None, 'multiple reservations for the same source')
            found = (source, version, tag)
    if found:
        source, version, tag = found
        require(version.split('+')[0] == 'v' + review['official_version'], 'reserved official version changed')
    else:
        number = max(sequences, default=0) + 1
        version = 'v' + review['official_version'] + '+r17.' + str(number)
        source = repo.commit(original, {VERSION: version.removeprefix('v') + '\n'},
                             'chore: freeze deployment candidate ' + version + '\n')
        tag = TAG_PREFIX + str(number)
        if publish:
            # A non-force ref creation reserves the number even if later CI fails.
            repo.git('push', 'origin', source + ':refs/tags/' + tag)
        repo.git('update-ref', 'refs/tags/' + tag, source, '0' * 40)
    return {'source': source, 'input_source': original, 'version': version,
            'tag': tag, 'candidate': True, 'review': review}


def add_candidate(repo, plan, head, run_url, allow_special=False):
    require(plan.get('candidate') is True, 'not an eligible candidate')
    review = review_for(repo, head, allow_special)
    require(review == plan['review'], 'candidate review changed during CI')
    source, parent, version = reservation(repo, plan['tag'])
    require((source, parent, version) == (plan['source'], plan['input_source'], plan['version']),
            'frozen candidate identity changed')
    items = validate_catalog(json.loads(repo.read(head, CATALOG)))
    existing = [c for c in items if c['source'] == source]
    if existing:
        require(existing[0]['version'] == version and existing[0]['previous_source'] == review['previous_source'],
                'published candidate identity changed')
        return None
    require(len(items) < 100, 'catalog full: archive deliberately; never silently remove candidates')
    require(re.fullmatch(r'https://github\.com/' + re.escape(REPOSITORY) + r'/actions/runs/[1-9][0-9]*', run_url),
            'CI run URL required')
    item = {'id': 'r17-' + version.rsplit('.', 1)[1], 'source': source,
            'previous_source': review['previous_source'], 'version': version,
            'channel': review.get('channel', 'ordinary'),
            'summary': review['summary'], 'review': review['evidence'],
            'reviewed_by': review['reviewed_by'], 'input_source': parent,
            'official_version': review['official_version'], 'official_source': review['official_source'],
            'ci_run': run_url}
    if item['channel'] == 'special':
        item['reason'] = review['special_plan']
        item['rollback_plan'] = review['rollback_plan']
    items.append(item)
    validate_catalog(items)
    content = json.dumps(items, ensure_ascii=False, indent=2) + '\n'
    require(len(content.encode()) <= 131072, 'catalog exceeds controller byte limit')
    return repo.commit(head, {CATALOG: content}, 'chore: register ' + version + ' deployment candidate\n')


def output(plan):
    Path('candidate-plan.json').write_text(json.dumps(plan, ensure_ascii=False, indent=2) + '\n')
    if os.environ.get('GITHUB_OUTPUT'):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as f:
            f.write('source=' + plan['source'] + '\ncandidate=' + str(plan['candidate']).lower() + '\n')
    message = ('Candidate frozen: ' + plan['source'] if plan['candidate'] else
               'Candidate publication blocked: ' + plan.get('reason', 'not a main push'))
    print(message)
    if os.environ.get('GITHUB_STEP_SUMMARY'):
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as f:
            f.write(message + '\n\nCI does not prepare or activate production.\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['freeze', 'publish'])
    parser.add_argument('--head', required=True)
    args = parser.parse_args()
    repo = Repo()
    require(os.environ.get('GITHUB_REPOSITORY') == REPOSITORY, 'unexpected repository')
    require(os.environ.get('GITHUB_EVENT_NAME') in ('push', 'workflow_dispatch'), 'untrusted event')
    require(os.environ.get('GITHUB_REF') == 'refs/heads/main', 'main only')
    allow_special = (os.environ.get('GITHUB_EVENT_NAME') == 'workflow_dispatch' and
                     os.environ.get('PUBLISH_SPECIAL') == 'true')
    repo.git('fetch', 'origin', 'main', '--tags')
    if args.action == 'freeze':
        # Obsolete runs still test their own original SHA, but must not allocate versions.
        if repo.git('rev-parse', 'origin/main') != args.head:
            output({'source': args.head, 'candidate': False, 'reason': 'SUPERSEDED_PUSH'})
        else:
            output(freeze(repo, args.head, publish=True, allow_special=allow_special))
    else:
        plan = json.loads(Path('candidate-plan.json').read_text())
        # Read the latest metadata, then use a normal fast-forward push. A concurrent push fails closed.
        head = repo.git('rev-parse', 'origin/main')
        commit = add_candidate(repo, plan, head, os.environ['GITHUB_SERVER_URL'] + '/' +
                               REPOSITORY + '/actions/runs/' + os.environ['GITHUB_RUN_ID'],
                               allow_special=allow_special)
        if commit:
            repo.git('push', 'origin', commit + ':refs/heads/main')
            print('Catalog registered at ' + commit + '; production remains unchanged.')
        else:
            print('Candidate already registered; no new version or catalog commit.')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, TypeError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
