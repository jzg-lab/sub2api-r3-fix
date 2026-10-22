import configparser
from pathlib import Path
import re
import shlex
import shutil
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

DEPLOY = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(DEPLOY))
import fast_deploy as d
import managed_deploy as m
import verify_fast_release as v


class InstallationTests(unittest.TestCase):
    def test_controller_and_reconciler_share_installed_entrypoint(self):
        commands = []
        for name in ("sub2api-deployment.service", "sub2api-handoff-reconcile.service"):
            unit = configparser.ConfigParser(interpolation=None)
            unit.read(DEPLOY / name)
            commands.append(shlex.split(unit["Service"]["ExecStart"]))
        self.assertEqual([args[2] for args in commands], ["serve", "reconcile"])
        for args in commands:
            self.assertEqual(args[:2], ["/usr/bin/python3", "/opt/sub2api-control/tools/managed_deploy.py"])
            self.assertEqual(args[args.index("--config") + 1], "/opt/sub2api-control/config.json")

    def test_documented_tool_installation_supports_verification_context(self):
        guide = (DEPLOY.parent / "docs/merge-deploy-guide.md").read_text()
        paragraph = guide.split("- 安装同一已审核版本的 ", 1)[1].split("仅 root 可写。", 1)[0]
        names = [name for name in re.findall(r"`([^`]+)`", paragraph) if "/" not in name]
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            tools = root / "tools"
            tools.mkdir()
            for name in names:
                shutil.copyfile(DEPLOY / name, tools / name)
            d.save(root / "candidate.json", {})
            d.save(root / "deployment.json", {})
            d.atomic(root / "app.env", b"EXISTING=1\n")
            controller = object.__new__(m.Controller)
            controller.c = {"deployment_config": str(root / "deployment.json")}
            host = Mock(c={"env_file": str(root / "app.env")})
            with patch.object(m, "__file__", str(tools / "managed_deploy.py")), \
                    patch.object(v, "approved_inputs", return_value=({}, [])):
                context = controller.verification_context(root, host)
            self.assertEqual(set(context["tools"]), set(names))


if __name__ == "__main__":
    unittest.main()
