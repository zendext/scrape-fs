import os
from pathlib import Path
import pwd
import shutil
import stat
import subprocess
import tempfile
import unittest


REPOSITORY = Path(__file__).resolve().parents[2]
INSTALLER = REPOSITORY / "scripts" / "install.sh"


class InstallScriptTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.binary_directory = self.root / "build"
        self.binary_directory.mkdir()
        self.administrator = pwd.getpwuid(os.getuid()).pw_name
        for name in ("scrapefs", "scrapefsd"):
            binary = self.binary_directory / name
            binary.write_text(f"{name} first build\n", encoding="utf-8")
            binary.chmod(0o755)

    def run_installer(
        self,
        *arguments,
        check=True,
        destination_root=None,
        environment_overrides=None,
    ):
        environment = os.environ.copy()
        environment["DESTDIR"] = str(
            destination_root or self.root / "staged"
        )
        if environment_overrides:
            environment.update(environment_overrides)
        result = subprocess.run(
            [
                str(INSTALLER),
                "--administrator",
                self.administrator,
                "--binary-dir",
                str(self.binary_directory),
                *arguments,
            ],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
        )
        if check and result.returncode != 0:
            self.fail(
                f"installer failed with {result.returncode}\n"
                f"stdout:\n{result.stdout}\n"
                f"stderr:\n{result.stderr}"
            )
        return result

    def installed(self, path):
        return self.root / "staged" / path.lstrip("/")

    def assert_mode(self, path, expected):
        actual = stat.S_IMODE(path.stat().st_mode)
        self.assertEqual(actual, expected, f"{path} mode = {actual:o}")

    def test_stages_binaries_configuration_directories_and_service(self):
        self.run_installer()

        for name in ("scrapefs", "scrapefsd"):
            binary = self.installed(f"/usr/local/bin/{name}")
            self.assertEqual(
                binary.read_text(encoding="utf-8"),
                f"{name} first build\n",
            )
            self.assert_mode(binary, 0o755)

        mountpoint = self.installed("/mnt/scrapefs")
        state_directory = self.installed("/var/lib/scrapefs")
        self.assertTrue(mountpoint.is_dir())
        self.assertTrue(state_directory.is_dir())
        self.assert_mode(mountpoint, 0o710)
        self.assert_mode(state_directory, 0o700)

        config = self.installed("/etc/scrapefs/config.toml")
        self.assert_mode(config, 0o644)
        config_text = config.read_text(encoding="utf-8")
        self.assertIn('mountpoint = "/mnt/scrapefs"', config_text)
        self.assertIn(
            f"administrator_uid = {os.getuid()}",
            config_text,
        )

        service = self.installed(
            "/etc/systemd/system/scrapefsd.service"
        )
        self.assert_mode(service, 0o644)
        service_text = service.read_text(encoding="utf-8")
        self.assertIn("Type=notify", service_text)
        self.assertIn(
            "ExecStart=/usr/local/bin/scrapefsd "
            "--config /etc/scrapefs/config.toml",
            service_text,
        )
        self.assertNotIn("zpool", service_text.casefold())
        self.assertNotIn("zfs", service_text.casefold())

    def test_reinstall_updates_managed_files_but_preserves_configuration(self):
        self.run_installer()
        config = self.installed("/etc/scrapefs/config.toml")
        config.write_text("preserve this configuration\n", encoding="utf-8")
        for name in ("scrapefs", "scrapefsd"):
            (self.binary_directory / name).write_text(
                f"{name} second build\n",
                encoding="utf-8",
            )

        self.run_installer()

        self.assertEqual(
            config.read_text(encoding="utf-8"),
            "preserve this configuration\n",
        )
        for name in ("scrapefs", "scrapefsd"):
            self.assertEqual(
                self.installed(
                    f"/usr/local/bin/{name}"
                ).read_text(encoding="utf-8"),
                f"{name} second build\n",
            )

    def test_stages_from_self_contained_bundle_layout(self):
        bundle = self.root / "bundle"
        bundle.mkdir()
        installer = bundle / "install.sh"
        shutil.copy2(INSTALLER, installer)
        installer.chmod(0o755)
        shutil.copy2(
            REPOSITORY / "packaging" / "systemd" / "scrapefsd.service",
            bundle / "scrapefsd.service",
        )
        for name in ("scrapefs", "scrapefsd"):
            shutil.copy2(self.binary_directory / name, bundle / name)

        environment = os.environ.copy()
        environment["DESTDIR"] = str(self.root / "bundle-staged")
        result = subprocess.run(
            [
                str(installer),
                "--administrator",
                self.administrator,
            ],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(
            (
                self.root
                / "bundle-staged"
                / "usr"
                / "local"
                / "bin"
                / "scrapefsd"
            ).is_file()
        )

    def test_preserves_shared_install_directory_modes(self):
        binary_parent = self.installed("/usr/local/bin")
        unit_parent = self.installed("/etc/systemd/system")
        binary_parent.mkdir(parents=True)
        unit_parent.mkdir(parents=True)
        binary_parent.chmod(0o750)
        unit_parent.chmod(0o751)

        self.run_installer()

        self.assert_mode(binary_parent, 0o750)
        self.assert_mode(unit_parent, 0o751)

    def test_rejects_mounted_target_before_creating_installation(self):
        fake_commands = self.root / "fake-commands"
        fake_commands.mkdir()
        findmnt = fake_commands / "findmnt"
        findmnt.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        findmnt.chmod(0o755)

        result = self.run_installer(
            check=False,
            environment_overrides={
                "PATH": f"{fake_commands}:{os.environ['PATH']}",
            },
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("mountpoint is currently mounted", result.stderr)
        self.assertFalse((self.root / "staged").exists())

    def test_rejects_managed_directory_symlinks_without_touching_targets(self):
        for managed_path in (
            "etc/scrapefs",
            "var/lib/scrapefs",
            "mnt/scrapefs",
        ):
            with self.subTest(path=managed_path):
                case = self.root / managed_path.replace("/", "-")
                staged = case / "staged"
                target = case / "target"
                target.mkdir(parents=True)
                target.chmod(0o750)
                managed = staged / managed_path
                managed.parent.mkdir(parents=True)
                managed.symlink_to(target, target_is_directory=True)

                result = self.run_installer(
                    check=False,
                    destination_root=staged,
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn(
                    "managed directory is a symbolic link",
                    result.stderr,
                )
                self.assert_mode(target, 0o750)
                self.assertEqual(list(target.iterdir()), [])
                self.assertFalse(
                    (staged / "usr/local/bin/scrapefs").exists()
                )

    def test_rejects_managed_paths_that_are_not_directories(self):
        for managed_path in (
            "etc/scrapefs",
            "var/lib/scrapefs",
            "mnt/scrapefs",
        ):
            with self.subTest(path=managed_path):
                case = self.root / ("file-" + managed_path.replace("/", "-"))
                staged = case / "staged"
                managed = staged / managed_path
                managed.parent.mkdir(parents=True)
                managed.write_text("preserve this file\n", encoding="utf-8")
                managed.chmod(0o640)

                result = self.run_installer(
                    check=False,
                    destination_root=staged,
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn("managed path is not a directory", result.stderr)
                self.assertEqual(
                    managed.read_text(encoding="utf-8"),
                    "preserve this file\n",
                )
                self.assert_mode(managed, 0o640)
                self.assertFalse(
                    (staged / "usr/local/bin/scrapefs").exists()
                )

    def test_rejects_missing_binary_before_creating_installation(self):
        (self.binary_directory / "scrapefsd").unlink()

        result = self.run_installer(check=False)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing executable", result.stderr)
        self.assertFalse((self.root / "staged").exists())


if __name__ == "__main__":
    unittest.main()
