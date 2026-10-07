import test from 'node:test'
import assert from 'node:assert/strict'
import { chmod, mkdir, mkdtemp, rm, stat, symlink, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { preparePrivateProfileRoot } from '../automate.mjs'

test('authorization profiles tighten existing permissions and reject symlinks', async () => {
  const root = await mkdtemp(join(tmpdir(), 'sub2api-profile-permissions-'))
  try {
    const profile = join(root, 'profiles')
    await preparePrivateProfileRoot(profile)
    assert.equal((await stat(profile)).mode & 0o777, 0o700)
    await chmod(profile, 0o777)
    await preparePrivateProfileRoot(profile)
    assert.equal((await stat(profile)).mode & 0o777, 0o700)

    const outside = join(root, 'outside')
    const linked = join(root, 'linked')
    await mkdir(outside)
    await chmod(outside, 0o755)
    await symlink(outside, linked)
    await assert.rejects(preparePrivateProfileRoot(linked))
    assert.equal((await stat(outside)).mode & 0o777, 0o755)

    const file = join(root, 'file')
    await writeFile(file, 'fixture')
    await assert.rejects(preparePrivateProfileRoot(file))
  } finally {
    await rm(root, { recursive: true, force: true })
  }
})
