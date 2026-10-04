import hashlib
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import unittest


SOURCE = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("proxy_address", SOURCE / "proxy_address.py")
PROXY_ADDRESS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROXY_ADDRESS)
AUTH_URL = (
    "https://auth.openai.com/oauth/authorize?"
    "response_type=code&client_id=test&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback"
    "&code_challenge=" + ("A" * 43) + "&code_challenge_method=S256&state=" + ("a" * 64)
)


def auth_url_for(character):
    return AUTH_URL.replace("a" * 64, character * 64)


class ProxyAddressTests(unittest.TestCase):
    def test_original_exit_normalization_and_non_public_rejection(self):
        self.assertEqual(
            PROXY_ADDRESS.validate_exit_ip(
                "::ffff:198.51.100.25", pinned_ip="198.51.100.25"
            ),
            "198.51.100.25",
        )
        for invalid in (
            "127.0.0.1", "::ffff:127.0.0.1", "10.0.0.1", "192.168.1.3",
            "172.16.1.1", "fe80::1%en0", "fc00::1", "::", "224.0.0.1",
            "255.255.255.255",
        ):
            with self.subTest(invalid=invalid):
                with self.assertRaises(ValueError):
                    PROXY_ADDRESS.validate_exit_ip(invalid, pinned_ip=invalid)

    def test_supported_addresses(self):
        cases = {
            "192.0.2.10:8080": "http://192.0.2.10:8080",
            " http://127.0.0.1:17933/ ": "http://127.0.0.1:17933",
            "HTTPS://Proxy.Example:443": "https://proxy.example:443",
            "socks5h://localhost:1080": "socks5://localhost:1080",
            "[::1]:1080": "http://[::1]:1080",
        }
        for raw, expected in cases.items():
            with self.subTest(raw=raw):
                self.assertEqual(PROXY_ADDRESS.normalize_proxy(raw), expected)

    def test_rejected_addresses(self):
        cases = [
            "", "192.0.2.10", "host:0", "host:65536", "ftp://host:80",
            "host:80/path", "bad host:80", "999.1.1.1:80",
            "http://user:example-password@host:80", '$(touch injected):80',
        ]
        for raw in cases:
            with self.subTest(raw=raw):
                with self.assertRaises(ValueError):
                    PROXY_ADDRESS.normalize_proxy(raw)

    def test_only_official_openai_authorization_url_is_allowed(self):
        self.assertEqual(PROXY_ADDRESS.normalize_auth_url(AUTH_URL), AUTH_URL)
        self.assertEqual(
            PROXY_ADDRESS.auth_profile_tag(AUTH_URL),
            "auth-" + hashlib.sha256(("a" * 64).encode("ascii")).hexdigest(),
        )
        for raw in [
            "http://auth.openai.com/oauth/authorize?state=x",
            "https://example.com/oauth/authorize?state=x",
            "https://auth.openai.com/other?state=x",
            "https://user@example.com/oauth/authorize?state=x",
            "https://auth.openai.com/oauth/authorize#fragment",
            AUTH_URL.replace("&state=" + ("a" * 64), ""),
            AUTH_URL.replace("state=" + ("a" * 64), "state=" + ("A" * 64)),
            AUTH_URL.replace("code_challenge_method=S256", "code_challenge_method=plain"),
            AUTH_URL + "&state=" + ("b" * 64),
        ]:
            with self.subTest(raw=raw):
                with self.assertRaises(ValueError):
                    PROXY_ADDRESS.normalize_auth_url(raw)

    def test_prune_removes_only_expired_unlocked_auth_profiles(self):
        with tempfile.TemporaryDirectory(prefix="sub2api-profile-prune-") as root_value:
            root = Path(root_value)
            current = "auth-" + ("a" * 64)
            expired = root / ("auth-" + ("b" * 64))
            fresh = root / ("auth-" + ("c" * 64))
            unrelated = root / "account-profile"
            for directory in (expired, fresh, unrelated):
                directory.mkdir()
            old = time.time() - 100 * 3600
            os.utime(expired, (old, old))
            os.utime(unrelated, (old, old))

            removed = PROXY_ADDRESS.prune_stale_profiles(
                root, current, 72, now=time.time()
            )

            self.assertEqual(removed, 1)
            self.assertFalse(expired.exists())
            self.assertTrue(fresh.exists())
            self.assertTrue(unrelated.exists())

    def test_prune_preserves_ambiguous_and_symlinked_profiles(self):
        with tempfile.TemporaryDirectory(prefix="sub2api-profile-prune-") as root_value:
            root = Path(root_value)
            ambiguous = root / ("auth-" + ("d" * 64))
            ambiguous.mkdir()
            (ambiguous / "SingletonLock").write_text("unknown")
            outside = root / "outside"
            outside.mkdir()
            linked = root / ("auth-" + ("e" * 64))
            linked.symlink_to(outside, target_is_directory=True)
            old = time.time() - 100 * 3600
            os.utime(ambiguous, (old, old))

            removed = PROXY_ADDRESS.prune_stale_profiles(
                root, "auth-" + ("f" * 64), 72, now=time.time()
            )

            self.assertEqual(removed, 0)
            self.assertTrue(ambiguous.exists())
            self.assertTrue(linked.is_symlink())
            self.assertTrue(outside.exists())


class LauncherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="sub2api-auth-browser-")
        self.root = Path(self.temp.name)
        self.chrome_record = self.root / "chrome.json"
        self.curl_record = self.root / "curl.json"
        self.profile_root = self.root / "profiles"

        self.chrome = self.root / "chrome-fixture"
        self.chrome.write_text(
            "#!/usr/bin/python3\n"
            "import json, os, pathlib, sys, time\n"
            "pathlib.Path(os.environ['TEST_CHROME_RECORD']).write_text(json.dumps("
            "{'args': sys.argv[1:], 'tz': os.environ.get('TZ'), 'pid': os.getpid()}))\n"
            "if os.environ.get('TEST_CHROME_EXIT') == '1': sys.exit(1)\n"
            "time.sleep(30)\n"
        )
        self.chrome.chmod(0o755)

        self.curl = self.root / "curl-fixture"
        self.curl.write_text(
            "#!/usr/bin/python3\n"
            "import json, os, pathlib, sys\n"
            "pathlib.Path(os.environ['TEST_CURL_RECORD']).write_text(json.dumps(sys.argv[1:]))\n"
            "print(os.environ.get('TEST_EXIT_IP', '198.51.100.25'))\n"
            "sys.exit(int(os.environ.get('TEST_CURL_EXIT', '0')))\n"
        )
        self.curl.chmod(0o755)

        self.env = dict(
            os.environ,
            SUB2API_AUTH_BROWSER_CHROME=str(self.chrome),
            SUB2API_AUTH_BROWSER_CURL=str(self.curl),
            SUB2API_AUTH_BROWSER_PROFILE_ROOT=str(self.profile_root),
            SUB2API_AUTH_BROWSER_LOG_FILE=str(self.root / "launcher.log"),
            SUB2API_AUTH_BROWSER_REQUIRE_STATIC_EXIT_CHECKS="true",
            SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17933="198.51.100.25",
            TEST_CHROME_RECORD=str(self.chrome_record),
            TEST_CURL_RECORD=str(self.curl_record),
        )

    def tearDown(self):
        if self.chrome_record.exists():
            try:
                os.kill(json.loads(self.chrome_record.read_text())["pid"], signal.SIGTERM)
            except ProcessLookupError:
                pass
        self.temp.cleanup()

    def launch(self, *args):
        return subprocess.run(
            [str(SOURCE / "launch.sh"), *args],
            env=self.env,
            capture_output=True,
            text=True,
            timeout=8,
        )

    def profile_tag(self, character):
        state = character * 64
        return "auth-" + hashlib.sha256(state.encode("ascii")).hexdigest()

    def test_proxy_reaches_preflight_and_chrome(self):
        result = self.launch(self.profile_tag("a"), AUTH_URL, "127.0.0.1:17933")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        curl_args = json.loads(self.curl_record.read_text())
        chrome = json.loads(self.chrome_record.read_text())
        self.assertEqual(curl_args[curl_args.index("--proxy") + 1], "http://127.0.0.1:17933")
        self.assertEqual(curl_args[curl_args.index("--noproxy") + 1], "")
        self.assertIn("--proxy-server=http://127.0.0.1:17933", chrome["args"])
        self.assertIn("--disable-quic", chrome["args"])
        self.assertEqual(chrome["args"][-1], AUTH_URL)
        self.assertEqual(chrome["tz"], "America/New_York")

    def test_original_login_ip_is_pinned_for_non_static_ingress(self):
        result = self.launch(
            self.profile_tag("a"), AUTH_URL, "127.0.0.1:8080", "198.51.100.25"
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(self.chrome_record.exists())

    def test_original_ip_mismatch_cannot_be_overridden_by_current_bucket(self):
        result = self.launch(
            self.profile_tag("a"), AUTH_URL, "127.0.0.1:17933", "198.51.100.99"
        )
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.chrome_record.exists())
        self.assertFalse(self.profile_root.exists())

    def test_bad_original_ip_never_launches_browser(self):
        for original in ("", "invalid", "198.51.100.26"):
            with self.subTest(original=original):
                result = self.launch(
                    self.profile_tag("a"), AUTH_URL, "127.0.0.1:8080", original
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.chrome_record.exists())

    def test_original_ip_does_not_disable_ingress_identity_check(self):
        self.env["SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17933"] = "198.51.100.99"
        result = self.launch(
            self.profile_tag("a"), AUTH_URL, "127.0.0.1:17933", "198.51.100.25"
        )
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.chrome_record.exists())

    def test_socks_proxy_uses_proxy_dns(self):
        result = self.launch(
            self.profile_tag("b"),
            auth_url_for("b"),
            "socks5h://[::1]:1080",
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        curl_args = json.loads(self.curl_record.read_text())
        self.assertIn("socks5h://[::1]:1080", curl_args)
        chrome = json.loads(self.chrome_record.read_text())
        self.assertIn("--proxy-server=socks5://[::1]:1080", chrome["args"])

    def test_missing_proxy_never_falls_back_or_reaches_network(self):
        result = self.launch(self.profile_tag("c"), auth_url_for("c"))
        self.assertEqual(result.returncode, 1)
        self.assertFalse(self.curl_record.exists())
        self.assertFalse(self.chrome_record.exists())

    def test_invalid_url_never_reaches_network(self):
        result = self.launch(
            self.profile_tag("d"),
            "https://example.com/oauth/authorize?state=x",
            "127.0.0.1:17933",
        )
        self.assertEqual(result.returncode, 1)
        self.assertFalse(self.curl_record.exists())

    def test_profile_tag_must_match_authorization_state(self):
        result = self.launch(self.profile_tag("b"), AUTH_URL, "127.0.0.1:17933")
        self.assertEqual(result.returncode, 1)
        self.assertFalse(self.curl_record.exists())
        self.assertFalse(self.chrome_record.exists())

    def test_network_failure_does_not_create_profile(self):
        self.env["TEST_CURL_EXIT"] = "7"
        result = self.launch(
            self.profile_tag("e"),
            auth_url_for("e"),
            "127.0.0.1:17933",
        )
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.profile_root.exists())
        self.assertFalse(self.chrome_record.exists())

    def test_malformed_exit_ip_is_rejected(self):
        self.env["TEST_EXIT_IP"] = "<html>proxy authentication required</html>"
        result = self.launch(
            self.profile_tag("f"),
            auth_url_for("f"),
            "127.0.0.1:17933",
        )
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.chrome_record.exists())

    def test_static_ingress_exit_must_match_expected_identity(self):
        self.env["SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17933"] = "198.51.100.26"
        result = self.launch(
            self.profile_tag("0"),
            auth_url_for("0"),
            "127.0.0.1:17933",
        )
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.chrome_record.exists())

    def test_static_ingress_requires_expected_identity_when_enforced(self):
        self.env.pop("SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17933")
        result = self.launch(
            self.profile_tag("0"),
            auth_url_for("0"),
            "127.0.0.1:17933",
        )
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.chrome_record.exists())

    def test_launch_prunes_expired_auth_profile_only(self):
        expired = self.profile_root / self.profile_tag("3")
        unrelated = self.profile_root / "account-profile"
        expired.mkdir(parents=True)
        unrelated.mkdir()
        old = time.time() - 100 * 3600
        os.utime(expired, (old, old))
        os.utime(unrelated, (old, old))

        result = self.launch(
            self.profile_tag("4"),
            auth_url_for("4"),
            "127.0.0.1:17933",
        )

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(expired.exists())
        self.assertTrue(unrelated.exists())

    def test_running_profile_cannot_reuse_old_proxy(self):
        child = subprocess.Popen(["/bin/sleep", "30"])
        try:
            profile = self.profile_root / self.profile_tag("1")
            profile.mkdir(parents=True)
            (profile / "SingletonLock").symlink_to("test-host-{}".format(child.pid))
            result = self.launch(
                profile.name,
                auth_url_for("1"),
                "127.0.0.1:17933",
            )
            self.assertEqual(result.returncode, 3)
            self.assertFalse(self.curl_record.exists())
        finally:
            child.terminate()
            child.wait(timeout=5)

    def test_ambiguous_regular_lock_fails_closed(self):
        profile = self.profile_root / self.profile_tag("a")
        profile.mkdir(parents=True)
        (profile / "SingletonLock").write_text("ambiguous")
        result = self.launch(profile.name, AUTH_URL, "127.0.0.1:17933")
        self.assertEqual(result.returncode, 3)
        self.assertFalse(self.curl_record.exists())

    def test_early_browser_exit_is_failure(self):
        self.env["TEST_CHROME_EXIT"] = "1"
        result = self.launch(
            self.profile_tag("2"),
            auth_url_for("2"),
            "127.0.0.1:17933",
        )
        self.assertEqual(result.returncode, 3)


if __name__ == "__main__":
    unittest.main(verbosity=2)
