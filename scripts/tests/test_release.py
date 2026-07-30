import hashlib
import os
from pathlib import Path
import pwd
import subprocess
import tarfile
import tempfile
import unittest


REPOSITORY = Path(__file__).resolve().parents[2]
PACKAGER = REPOSITORY / "scripts" / "package-release.sh"
VERSION_CLASSIFIER = REPOSITORY / "scripts" / "release-version.sh"
WORKFLOW = REPOSITORY / ".github" / "workflows" / "release.yml"


class ReleasePackageTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.output = self.root / "dist"
        self.administrator = pwd.getpwuid(os.getuid()).pw_name

    def run_packager(self, version, output=None, check=True):
        result = subprocess.run(
            [
                str(PACKAGER),
                version,
                str(output or self.output),
            ],
            cwd=REPOSITORY,
            check=False,
            capture_output=True,
            text=True,
        )
        if check and result.returncode != 0:
            self.fail(
                f"packager failed with {result.returncode}\n"
                f"stdout:\n{result.stdout}\n"
                f"stderr:\n{result.stderr}"
            )
        return result

    def test_builds_installable_versioned_linux_amd64_archive(self):
        version = "v0.1.0-test.1"
        archive_name = f"scrape-fs_{version}_linux_amd64.tar.gz"
        root_name = archive_name.removesuffix(".tar.gz")

        self.run_packager(version)

        archive = self.output / archive_name
        checksums = self.output / "SHA256SUMS"
        self.assertTrue(archive.is_file())
        self.assertTrue(checksums.is_file())
        checksum, recorded_name = checksums.read_text(
            encoding="utf-8"
        ).strip().split(maxsplit=1)
        self.assertEqual(recorded_name, archive_name)
        self.assertEqual(
            checksum,
            hashlib.sha256(archive.read_bytes()).hexdigest(),
        )

        expected_members = {
            root_name,
            f"{root_name}/install.sh",
            f"{root_name}/scrapefs",
            f"{root_name}/scrapefsd",
            f"{root_name}/scrapefsd.service",
        }
        extracted = self.root / "extracted"
        with tarfile.open(archive, "r:gz") as package:
            members = {member.name.rstrip("/") for member in package}
            self.assertEqual(members, expected_members)
            self.assertEqual(
                package.getmember(f"{root_name}/scrapefs").mode,
                0o755,
            )
            self.assertEqual(
                package.getmember(f"{root_name}/scrapefsd").mode,
                0o755,
            )
            self.assertEqual(
                package.getmember(f"{root_name}/install.sh").mode,
                0o755,
            )
            self.assertEqual(
                package.getmember(
                    f"{root_name}/scrapefsd.service"
                ).mode,
                0o644,
            )
            package.extractall(extracted)

        bundle = extracted / root_name
        for name in ("scrapefs", "scrapefsd"):
            version_result = subprocess.run(
                [str(bundle / name), "--version"],
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertEqual(version_result.returncode, 0)
            self.assertEqual(
                version_result.stdout.strip(),
                f"{name} {version}",
            )
            metadata = subprocess.run(
                ["go", "version", "-m", str(bundle / name)],
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertEqual(metadata.returncode, 0, metadata.stderr)
            self.assertIn("GOOS=linux", metadata.stdout)
            self.assertIn("GOARCH=amd64", metadata.stdout)
            self.assertIn("CGO_ENABLED=0", metadata.stdout)

        staged = self.root / "staged"
        environment = os.environ.copy()
        environment["DESTDIR"] = str(staged)
        install_result = subprocess.run(
            [
                str(bundle / "install.sh"),
                "--administrator",
                self.administrator,
            ],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
        )
        self.assertEqual(install_result.returncode, 0, install_result.stderr)
        self.assertTrue(
            (staged / "usr/local/bin/scrapefsd").is_file()
        )

    def test_rejects_invalid_release_versions_before_writing_output(self):
        for version in ("1.2.3", "v1/2", "v1 2"):
            with self.subTest(version=version):
                output = self.root / version.replace("/", "-").replace(" ", "-")
                result = self.run_packager(
                    version,
                    output=output,
                    check=False,
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn("invalid release version", result.stderr)
                self.assertFalse(output.exists())

    def test_rejects_nonempty_output_directory(self):
        self.output.mkdir()
        sentinel = self.output / "preserve.txt"
        sentinel.write_text("preserve\n", encoding="utf-8")

        result = self.run_packager("v0.1.0", check=False)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("output directory is not empty", result.stderr)
        self.assertEqual(
            sentinel.read_text(encoding="utf-8"),
            "preserve\n",
        )

    def test_classifies_stable_and_prerelease_versions(self):
        for version, expected in (
            ("v1.0.0", "stable"),
            ("v1.0.0+build.1", "stable"),
            ("v1.0.0-rc.1", "prerelease"),
            ("v0.1.0-test.1", "prerelease"),
        ):
            with self.subTest(version=version):
                result = subprocess.run(
                    [str(VERSION_CLASSIFIER), version],
                    check=False,
                    capture_output=True,
                    text=True,
                )

                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), expected)

    def test_workflow_marks_prereleases_and_excludes_them_from_latest(self):
        workflow = WORKFLOW.read_text(encoding="utf-8")

        self.assertIn(
            './scripts/release-version.sh "$GITHUB_REF_NAME"',
            workflow,
        )
        self.assertIn("--prerelease", workflow)
        self.assertIn("--latest=false", workflow)

    def test_workflow_does_not_require_host_systemd_verification(self):
        workflow = WORKFLOW.read_text(encoding="utf-8")

        self.assertNotIn("systemd-analyze verify", workflow)


if __name__ == "__main__":
    unittest.main()
