import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[2]


class DesktopInstallTests(unittest.TestCase):
    def test_stages_only_desktop_components_and_rejects_symlink_helper(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binaries = root / "bin"
            binaries.mkdir()
            for name in ("scrapefs-gui", "scrapefs-admin"):
                path = binaries / name
                path.write_text(name)
                path.chmod(0o755)
            destination = root / "staged"
            env = {**os.environ, "DESTDIR": str(destination)}
            command = ["sh", str(REPOSITORY / "scripts/install-desktop.sh"), "--binary-dir", str(binaries)]
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            helper = destination / "usr/local/libexec/scrapefs-admin"
            self.assertEqual(helper.read_text(), "scrapefs-admin")
            self.assertEqual(stat.S_IMODE(helper.stat().st_mode), 0o755)
            self.assertFalse((destination / "etc").exists())
            self.assertFalse((destination / "var").exists())
            self.assertTrue((destination / "usr/local/share/applications/io.github.zendext.scrapefs.desktop").is_file())
            helper.unlink()
            outside = root / "outside"
            outside.write_text("preserved")
            helper.symlink_to(outside)
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(outside.read_text(), "preserved")
