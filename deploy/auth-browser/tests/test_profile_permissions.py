import importlib.util
from pathlib import Path
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[1] / "proxy_address.py"
SPEC = importlib.util.spec_from_file_location("profile_directory", SOURCE)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class ProfilePermissionTests(unittest.TestCase):
    def test_existing_permissions_are_tightened_and_links_rejected(self):
        with tempfile.TemporaryDirectory(prefix="sub2api-profile-permissions-") as value:
            root = Path(value)
            profile = root / "profiles"
            MODULE.prepare_private_directory(profile)
            self.assertEqual(profile.stat().st_mode & 0o777, 0o700)
            profile.chmod(0o777)
            MODULE.prepare_private_directory(profile)
            self.assertEqual(profile.stat().st_mode & 0o777, 0o700)
            outside = root / "outside"
            outside.mkdir()
            outside.chmod(0o755)
            linked = root / "linked"
            linked.symlink_to(outside, target_is_directory=True)
            with self.assertRaises(ValueError):
                MODULE.prepare_private_directory(linked)
            self.assertEqual(outside.stat().st_mode & 0o777, 0o755)
            regular = root / "file"
            regular.touch()
            with self.assertRaises(ValueError):
                MODULE.prepare_private_directory(regular)


if __name__ == "__main__":
    unittest.main()
