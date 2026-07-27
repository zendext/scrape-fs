import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest


SKILL_DIR = Path(__file__).resolve().parents[1]
SCRIPT = SKILL_DIR / "scripts" / "scrapefs_media.py"
FIXTURES = Path(__file__).resolve().parent / "fixtures"
SKILL_DOCUMENT = SKILL_DIR / "SKILL.md"


class ScrapeFSMediaTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.ffprobe = self.root / "ffprobe"
        self.ffprobe.write_text(
            textwrap.dedent(
                """\
                #!/usr/bin/env python3
                import json
                import pathlib
                import sys

                source = pathlib.Path(sys.argv[-1])
                if "probe-fail" in source.name:
                    print("fixture probe failure", file=sys.stderr)
                    raise SystemExit(1)
                print(json.dumps({
                    "streams": [
                        {"index": 0, "codec_type": "video", "codec_name": "h264"},
                        {"index": 1, "codec_type": "audio", "codec_name": "aac"}
                    ],
                    "format": {"duration": "7200.0", "tags": {}}
                }))
                """
            ),
            encoding="utf-8",
        )
        self.ffprobe.chmod(0o755)

    def run_tool(self, *arguments, check=True, env=None):
        result = subprocess.run(
            [sys.executable, str(SCRIPT), *map(str, arguments)],
            check=False,
            capture_output=True,
            text=True,
            env=env,
        )
        if check and result.returncode != 0:
            self.fail(
                f"command failed with {result.returncode}\n"
                f"stdout:\n{result.stdout}\n"
                f"stderr:\n{result.stderr}"
            )
        return result

    def test_skill_uses_semantic_approval_and_retains_validated_digest(self):
        skill = SKILL_DOCUMENT.read_text(encoding="utf-8")
        help_text = " ".join(self.run_tool("apply", "--help").stdout.split())

        self.assertIn("explicitly approve or reject the reviewed batch", skill)
        self.assertIn("does not need to transcribe the digest", skill)
        self.assertIn('--confirm-digest "$VALIDATED_DIGEST"', skill)
        self.assertNotIn("confirm the exact digest string", skill)
        self.assertIn("retained from the user-approved validation", help_text)
        self.assertNotIn("explicitly confirmed by the user", help_text)

    def copy_fixture(self, name):
        destination = self.root / name
        shutil.copytree(FIXTURES / name, destination)
        return destination

    def plan(self, fixture, profile):
        batch = self.root / f"{fixture.name}-{profile}.batch.json"
        artifacts = self.root / f"{fixture.name}-{profile}.artifacts.json"
        result = self.run_tool(
            "plan",
            "--source",
            fixture,
            "--library",
            "media",
            "--profile",
            profile,
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
        )
        return json.loads(result.stdout), batch, artifacts

    def test_plans_jellyfin_movie_with_sample_subtitle_nfo_and_artwork(self):
        source = self.copy_fixture("movie-directory")

        report, batch_path, artifacts_path = self.plan(source, "jellyfin")

        self.assertEqual(report["status"], "ready")
        self.assertEqual(report["kind"], "movie")
        self.assertEqual(
            [Path(item["path"]).name for item in report["ignored"]],
            ["Dune.2021.sample.mkv"],
        )
        batch = json.loads(batch_path.read_text(encoding="utf-8"))
        self.assertEqual(
            batch,
            {
                "schema": "scrapefs.projection-batch/v1",
                "library": "media",
                "entries": [
                    {
                        "source": str(source / "Dune.2021.mkv"),
                        "target": (
                            "Movies/Dune (2021) [tmdbid-438631]/"
                            "Dune (2021) [tmdbid-438631].mkv"
                        ),
                    },
                    {
                        "source": str(source / "Dune.2021.en.forced.srt"),
                        "target": (
                            "Movies/Dune (2021) [tmdbid-438631]/"
                            "Dune (2021) [tmdbid-438631].en.forced.srt"
                        ),
                    },
                ],
            },
        )
        artifacts = json.loads(artifacts_path.read_text(encoding="utf-8"))
        self.assertEqual(
            [(item["kind"], Path(item["source"]).name, item["target"]) for item in artifacts["artifacts"]],
            [
                (
                    "nfo",
                    "movie.nfo",
                    "media/Movies/Dune (2021) [tmdbid-438631]/movie.nfo",
                ),
                (
                    "artwork",
                    "poster.jpg",
                    "media/Movies/Dune (2021) [tmdbid-438631]/poster.jpg",
                ),
            ],
        )

    def test_plans_plex_movie_with_deterministic_provider_hint(self):
        source = self.copy_fixture("movie-directory")

        report, batch_path, _ = self.plan(source, "plex")

        self.assertEqual(report["status"], "ready")
        targets = [
            entry["target"]
            for entry in json.loads(batch_path.read_text(encoding="utf-8"))["entries"]
        ]
        self.assertEqual(
            targets,
            [
                "Movies/Dune (2021) {tmdb-438631}/Dune (2021) {tmdb-438631}.mkv",
                (
                    "Movies/Dune (2021) {tmdb-438631}/"
                    "Dune (2021) {tmdb-438631}.en.forced.srt"
                ),
            ],
        )

    def test_ignores_lower_priority_nfo_provider_when_tmdb_is_valid(self):
        source = self.copy_fixture("movie-directory")
        nfo = source / "movie.nfo"
        nfo.write_text(
            nfo.read_text(encoding="utf-8").replace(
                "</movie>",
                '<uniqueid type="wikidata">Q123185887</uniqueid></movie>',
            ),
            encoding="utf-8",
        )

        report, batch_path, _ = self.plan(source, "jellyfin")

        self.assertEqual(report["status"], "ready")
        self.assertEqual(
            json.loads(batch_path.read_text(encoding="utf-8"))["entries"][0][
                "target"
            ],
            (
                "Movies/Dune (2021) [tmdbid-438631]/"
                "Dune (2021) [tmdbid-438631].mkv"
            ),
        )
        self.assertIn(
            {
                "code": "unsupported_provider_id",
                "message": (
                    "ignored lower-priority NFO provider ID "
                    "'wikidata'='Q123185887'"
                ),
                "path": str(nfo),
            },
            report["warnings"],
        )

    def test_rejects_invalid_supported_nfo_provider(self):
        source = self.copy_fixture("movie-directory")
        nfo = source / "movie.nfo"
        nfo.write_text(
            nfo.read_text(encoding="utf-8").replace(
                ">438631<",
                ">not-a-tmdb-id<",
            ),
            encoding="utf-8",
        )

        result = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--batch",
            self.root / "invalid-provider.batch.json",
            "--artifacts",
            self.root / "invalid-provider.artifacts.json",
            "--ffprobe",
            self.ffprobe,
            check=False,
        )

        self.assertEqual(result.returncode, 3)
        report = json.loads(result.stdout)
        self.assertEqual(report["status"], "manual")
        self.assertIn("invalid 'tmdb' provider ID", report["manual"][0]["message"])

    def test_plans_single_file_movie_from_agent_metadata_and_generates_nfo(self):
        source = self.root / "unidentified.mkv"
        source.write_text("fixture movie", encoding="utf-8")
        metadata = self.root / "metadata.json"
        metadata.write_text(
            json.dumps(
                {
                    "kind": "movie",
                    "title": "Dune: Part One",
                    "year": 2021,
                    "provider_ids": {"tmdb": "438631"},
                }
            ),
            encoding="utf-8",
        )
        batch = self.root / "single.batch.json"
        artifacts = self.root / "single.artifacts.json"

        result = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--metadata",
            metadata,
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
        )

        self.assertEqual(json.loads(result.stdout)["status"], "ready")
        self.assertEqual(
            json.loads(batch.read_text())["entries"][0]["target"],
            (
                "Movies/Dune - Part One (2021) [tmdbid-438631]/"
                "Dune - Part One (2021) [tmdbid-438631].mkv"
            ),
        )
        artifact = json.loads(artifacts.read_text())["artifacts"][0]
        self.assertEqual(artifact["kind"], "nfo")
        self.assertNotIn("source", artifact)
        self.assertIn("<title>Dune: Part One</title>", artifact["content"])

    def test_plans_supported_sidecars_for_single_file_movie(self):
        source = self.root / "Dune.2021.mkv"
        source.write_text("fixture movie", encoding="utf-8")
        subtitle = self.root / "Dune.2021.en.srt"
        subtitle.write_text("fixture subtitle", encoding="utf-8")
        nfo = self.root / "movie.nfo"
        nfo.write_text(
            "<movie><title>Dune</title><year>2021</year></movie>\n",
            encoding="utf-8",
        )
        poster = self.root / "poster.jpg"
        poster.write_text("fixture poster", encoding="utf-8")
        (self.root / "Other.2020.mkv").write_text(
            "unrelated sibling video",
            encoding="utf-8",
        )
        batch = self.root / "single-sidecars.batch.json"
        artifacts = self.root / "single-sidecars.artifacts.json"

        result = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
        )

        self.assertEqual(json.loads(result.stdout)["status"], "ready")
        batch_entries = json.loads(batch.read_text(encoding="utf-8"))["entries"]
        self.assertEqual(
            [(entry["source"], entry["target"]) for entry in batch_entries],
            [
                (
                    str(source),
                    "Movies/Dune (2021)/Dune (2021).mkv",
                ),
                (
                    str(subtitle),
                    "Movies/Dune (2021)/Dune (2021).en.srt",
                ),
            ],
        )
        artifact_entries = json.loads(
            artifacts.read_text(encoding="utf-8")
        )["artifacts"]
        self.assertEqual(
            [
                (artifact["kind"], artifact.get("source"), artifact["target"])
                for artifact in artifact_entries
            ],
            [
                (
                    "nfo",
                    str(nfo),
                    "media/Movies/Dune (2021)/movie.nfo",
                ),
                (
                    "artwork",
                    str(poster),
                    "media/Movies/Dune (2021)/poster.jpg",
                ),
            ],
        )

    def test_rejects_non_regular_adjacent_single_file_sidecars(self):
        for sidecar_name in ("poster.jpg", "movie.nfo"):
            with self.subTest(sidecar=sidecar_name):
                case_root = self.root / sidecar_name
                case_root.mkdir()
                source = case_root / "Dune.2021.mkv"
                source.write_text("fixture movie", encoding="utf-8")
                sidecar = case_root / sidecar_name
                sidecar.mkdir()
                batch = case_root / "batch.json"
                artifacts = case_root / "artifacts.json"

                result = self.run_tool(
                    "plan",
                    "--source",
                    source,
                    "--library",
                    "media",
                    "--profile",
                    "jellyfin",
                    "--batch",
                    batch,
                    "--artifacts",
                    artifacts,
                    "--ffprobe",
                    self.ffprobe,
                    check=False,
                )

                self.assertEqual(result.returncode, 3)
                report = json.loads(result.stdout)
                self.assertEqual(report["status"], "manual")
                self.assertIn(
                    {
                        "code": "non_regular_source",
                        "message": "source contains a non-regular filesystem object",
                        "path": str(sidecar),
                    },
                    report["manual"],
                )
                self.assertFalse(batch.exists())
                self.assertFalse(artifacts.exists())

    def test_normalizes_parenthesized_and_bracketed_movie_years(self):
        cases = {
            "Dune (2021).mkv": "Movies/Dune (2021)/Dune (2021).mkv",
            "Arrival [2016].mkv": "Movies/Arrival (2016)/Arrival (2016).mkv",
        }
        for name, expected_target in cases.items():
            with self.subTest(name=name):
                source = self.root / name
                source.write_text("fixture movie", encoding="utf-8")
                batch = self.root / f"{source.stem}.batch.json"
                artifacts = self.root / f"{source.stem}.artifacts.json"

                result = self.run_tool(
                    "plan",
                    "--source",
                    source,
                    "--library",
                    "media",
                    "--profile",
                    "jellyfin",
                    "--batch",
                    batch,
                    "--artifacts",
                    artifacts,
                    "--ffprobe",
                    self.ffprobe,
                )

                self.assertEqual(json.loads(result.stdout)["status"], "ready")
                self.assertEqual(
                    json.loads(batch.read_text())["entries"][0]["target"],
                    expected_target,
                )

    def test_plans_multiple_standard_episodes_and_subtitles(self):
        source = self.copy_fixture("episodes")

        jellyfin_report, jellyfin_batch, _ = self.plan(source, "jellyfin")
        plex_report, plex_batch, _ = self.plan(source, "plex")

        self.assertEqual(jellyfin_report["status"], "ready")
        self.assertEqual(jellyfin_report["kind"], "episodes")
        self.assertEqual(plex_report["status"], "ready")
        self.assertEqual(
            [entry["target"] for entry in json.loads(jellyfin_batch.read_text())["entries"]],
            [
                (
                    "Shows/The Expanse (2015) [tvdbid-280619]/Season 01/"
                    "The Expanse (2015) S01E01 Dulcinea.mkv"
                ),
                (
                    "Shows/The Expanse (2015) [tvdbid-280619]/Season 01/"
                    "The Expanse (2015) S01E01 Dulcinea.en.srt"
                ),
                (
                    "Shows/The Expanse (2015) [tvdbid-280619]/Season 01/"
                    "The Expanse (2015) S01E02 The Big Empty.mkv"
                ),
            ],
        )
        self.assertEqual(
            [entry["target"] for entry in json.loads(plex_batch.read_text())["entries"]],
            [
                (
                    "TV Shows/The Expanse (2015) {tvdb-280619}/Season 01/"
                    "The Expanse (2015) - S01E01 - Dulcinea.mkv"
                ),
                (
                    "TV Shows/The Expanse (2015) {tvdb-280619}/Season 01/"
                    "The Expanse (2015) - S01E01 - Dulcinea.en.srt"
                ),
                (
                    "TV Shows/The Expanse (2015) {tvdb-280619}/Season 01/"
                    "The Expanse (2015) - S01E02 - The Big Empty.mkv"
                ),
            ],
        )

    def test_returns_manual_for_mixed_series_episode_directory(self):
        source = self.root / "mixed-series"
        source.mkdir()
        (source / "Alpha.S01E01.One.mkv").write_text(
            "fixture episode",
            encoding="utf-8",
        )
        (source / "Beta.S01E02.Two.mkv").write_text(
            "fixture episode",
            encoding="utf-8",
        )
        batch = self.root / "mixed-series.batch.json"
        artifacts = self.root / "mixed-series.artifacts.json"

        result = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
            check=False,
        )

        self.assertEqual(result.returncode, 3)
        self.assertIn(
            "mixed_series",
            [item["code"] for item in json.loads(result.stdout)["manual"]],
        )
        self.assertFalse(batch.exists())
        self.assertFalse(artifacts.exists())

    def test_returns_manual_for_all_multi_episode_filename_forms(self):
        names = [
            "Show.S01E01E02.mkv",
            "Show.S01E01-E02.mkv",
            "Show.S01E01.S01E02.mkv",
        ]
        for index, name in enumerate(names):
            with self.subTest(name=name):
                source = self.root / f"multi-episode-{index}"
                source.mkdir()
                (source / name).write_text("fixture episode", encoding="utf-8")
                batch = self.root / f"multi-episode-{index}.batch.json"
                artifacts = self.root / f"multi-episode-{index}.artifacts.json"

                result = self.run_tool(
                    "plan",
                    "--source",
                    source,
                    "--library",
                    "media",
                    "--profile",
                    "jellyfin",
                    "--batch",
                    batch,
                    "--artifacts",
                    artifacts,
                    "--ffprobe",
                    self.ffprobe,
                    check=False,
                )

                self.assertEqual(result.returncode, 3)
                self.assertIn(
                    "multi_episode_file",
                    [item["code"] for item in json.loads(result.stdout)["manual"]],
                )
                self.assertFalse(batch.exists())
                self.assertFalse(artifacts.exists())

    def test_infers_filename_only_show_year_without_changing_numeric_title(self):
        cases = {
            "Show.2015.S01E01.Pilot.mkv": (
                "Shows/Show (2015)/Season 01/"
                "Show (2015) S01E01 Pilot.mkv"
            ),
            "1923.S01E01.Pilot.mkv": (
                "Shows/1923/Season 01/1923 S01E01 Pilot.mkv"
            ),
        }
        for index, (name, expected_target) in enumerate(cases.items()):
            with self.subTest(name=name):
                source = self.root / f"show-year-{index}"
                source.mkdir()
                (source / name).write_text("fixture episode", encoding="utf-8")
                batch = self.root / f"show-year-{index}.batch.json"
                artifacts = self.root / f"show-year-{index}.artifacts.json"

                result = self.run_tool(
                    "plan",
                    "--source",
                    source,
                    "--library",
                    "media",
                    "--profile",
                    "jellyfin",
                    "--batch",
                    batch,
                    "--artifacts",
                    artifacts,
                    "--ffprobe",
                    self.ffprobe,
                )

                self.assertEqual(json.loads(result.stdout)["status"], "ready")
                self.assertEqual(
                    json.loads(batch.read_text())["entries"][0]["target"],
                    expected_target,
                )

    def test_returns_explicit_manual_outcomes_for_unsupported_layouts(self):
        cases = {
            "multi-episode": "multi_episode_file",
            "disc-layout": "disc_layout",
            "ambiguous-movies": "ambiguous_layout",
            "probe-failure": "probe_failed",
        }
        for fixture_name, expected_code in cases.items():
            with self.subTest(fixture=fixture_name):
                source = self.copy_fixture(fixture_name)
                batch = self.root / f"{fixture_name}.json"
                artifacts = self.root / f"{fixture_name}.artifacts.json"
                result = self.run_tool(
                    "plan",
                    "--source",
                    source,
                    "--library",
                    "media",
                    "--profile",
                    "jellyfin",
                    "--batch",
                    batch,
                    "--artifacts",
                    artifacts,
                    "--ffprobe",
                    self.ffprobe,
                    check=False,
                )
                self.assertEqual(result.returncode, 3)
                report = json.loads(result.stdout)
                self.assertEqual(report["status"], "manual")
                self.assertIn(expected_code, [item["code"] for item in report["manual"]])
                self.assertFalse(batch.exists())
                self.assertFalse(artifacts.exists())

    def test_returns_manual_when_ffprobe_json_is_not_an_object(self):
        source = self.root / "invalid-probe.mkv"
        source.write_text("fixture movie", encoding="utf-8")
        ffprobe = self.root / "invalid-ffprobe"
        ffprobe.write_text(
            "#!/usr/bin/env python3\nprint('[]')\n",
            encoding="utf-8",
        )
        ffprobe.chmod(0o755)
        batch = self.root / "invalid-probe.batch.json"
        artifacts = self.root / "invalid-probe.artifacts.json"

        result = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            ffprobe,
            check=False,
        )

        self.assertEqual(result.returncode, 3)
        report = json.loads(result.stdout)
        self.assertEqual(report["status"], "manual")
        self.assertIn(
            "probe_failed",
            [item["code"] for item in report["manual"]],
        )
        self.assertEqual(result.stderr, "")
        self.assertFalse(batch.exists())
        self.assertFalse(artifacts.exists())

    def test_returns_manual_when_ffprobe_stream_fields_are_malformed(self):
        cases = {
            "codec-type": {
                "streams": [
                    {"codec_type": ["video"], "codec_name": "h264"},
                    {"codec_type": "video", "codec_name": "h265"},
                ],
                "format": {},
            },
            "codec-name": {
                "streams": [
                    {"codec_type": "video", "codec_name": ["h264"]},
                ],
                "format": {},
            },
        }
        for case, probe_document in cases.items():
            with self.subTest(case=case):
                case_root = self.root / case
                case_root.mkdir()
                source = case_root / "movie.mkv"
                source.write_text("fixture movie", encoding="utf-8")
                ffprobe = case_root / "ffprobe"
                ffprobe.write_text(
                    "#!/usr/bin/env python3\n"
                    f"print({json.dumps(probe_document)!r})\n",
                    encoding="utf-8",
                )
                ffprobe.chmod(0o755)
                batch = case_root / "batch.json"
                artifacts = case_root / "artifacts.json"

                result = self.run_tool(
                    "plan",
                    "--source",
                    source,
                    "--library",
                    "media",
                    "--profile",
                    "jellyfin",
                    "--batch",
                    batch,
                    "--artifacts",
                    artifacts,
                    "--ffprobe",
                    ffprobe,
                    check=False,
                )

                self.assertEqual(result.returncode, 3)
                report = json.loads(result.stdout)
                self.assertEqual(report["status"], "manual")
                self.assertIn(
                    "probe_failed",
                    [item["code"] for item in report["manual"]],
                )
                self.assertEqual(result.stderr, "")
                self.assertFalse(batch.exists())
                self.assertFalse(artifacts.exists())

    def test_manual_rerun_invalidates_previous_plan_outputs(self):
        source = self.copy_fixture("movie-directory")
        report, batch, artifacts = self.plan(source, "jellyfin")
        self.assertEqual(report["status"], "ready")
        self.assertTrue(batch.exists())
        self.assertTrue(artifacts.exists())
        (source / "Other.2022.mkv").write_text("second movie", encoding="utf-8")

        rerun = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
            check=False,
        )

        self.assertEqual(rerun.returncode, 3)
        self.assertEqual(json.loads(rerun.stdout)["status"], "manual")
        self.assertFalse(batch.exists())
        self.assertFalse(artifacts.exists())

    def test_rejects_duplicate_keys_in_metadata_and_artifact_manifest(self):
        source = self.root / "movie.mkv"
        source.write_text("fixture movie", encoding="utf-8")
        metadata = self.root / "duplicate-metadata.json"
        metadata.write_text(
            '{"kind":"movie","title":"Reviewed","title":"Executed","year":2020}',
            encoding="utf-8",
        )
        batch = self.root / "duplicate.batch.json"
        artifacts = self.root / "duplicate.artifacts.json"

        planned = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--metadata",
            metadata,
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
            check=False,
        )
        self.assertEqual(planned.returncode, 3)
        self.assertIn(
            "metadata_invalid",
            [item["code"] for item in json.loads(planned.stdout)["manual"]],
        )
        self.assertFalse(batch.exists())
        self.assertFalse(artifacts.exists())

        mountpoint = self.root / "mount"
        target_directory = mountpoint / "media" / "Movies" / "Movie"
        target_directory.mkdir(parents=True)
        reviewed = self.root / "reviewed.nfo"
        executed = self.root / "executed.nfo"
        reviewed.write_text("<movie><title>Reviewed</title></movie>\n", encoding="utf-8")
        executed.write_text("<movie><title>Executed</title></movie>\n", encoding="utf-8")
        manifest = self.root / "duplicate-manifest.json"
        manifest.write_text(
            (
                '{"schema":"scrapefs.agent-artifacts/v1","library":"media",'
                '"artifacts":[{"kind":"nfo",'
                f'"source":{json.dumps(str(reviewed))},'
                f'"source":{json.dumps(str(executed))},'
                '"target":"media/Movies/Movie/movie.nfo"}]}'
            ),
            encoding="utf-8",
        )

        artifact_result = self.run_tool(
            "artifacts",
            "--manifest",
            manifest,
            "--mountpoint",
            mountpoint,
            check=False,
        )
        self.assertEqual(artifact_result.returncode, 5)
        payload = json.loads(artifact_result.stdout)
        self.assertEqual(payload["failed"][0]["code"], "invalid_manifest")
        self.assertFalse((target_directory / "movie.nfo").exists())

    def test_does_not_follow_symlink_or_accept_remote_source(self):
        source = self.root / "symlink-source"
        source.mkdir()
        (source / "movie.mkv").symlink_to(FIXTURES / "movie-directory" / "Dune.2021.mkv")
        batch = self.root / "unsafe.batch.json"
        artifacts = self.root / "unsafe.artifacts.json"

        symlink = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
            check=False,
        )
        self.assertEqual(symlink.returncode, 3)
        self.assertIn(
            "symlink_source",
            [item["code"] for item in json.loads(symlink.stdout)["manual"]],
        )

        real_parent = self.root / "real-parent"
        real_source = real_parent / "media"
        real_source.mkdir(parents=True)
        (real_source / "movie.mkv").write_text("fixture movie", encoding="utf-8")
        linked_parent = self.root / "linked-parent"
        linked_parent.symlink_to(real_parent, target_is_directory=True)
        ancestor_symlink = self.run_tool(
            "plan",
            "--source",
            linked_parent / "media",
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
            check=False,
        )
        self.assertEqual(ancestor_symlink.returncode, 3)
        self.assertIn(
            "symlink_source",
            [
                item["code"]
                for item in json.loads(ancestor_symlink.stdout)["manual"]
            ],
        )

        remote = self.run_tool(
            "plan",
            "--source",
            "https://example.invalid/movie.mkv",
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
            check=False,
        )
        self.assertEqual(remote.returncode, 3)
        self.assertIn(
            "remote_source",
            [item["code"] for item in json.loads(remote.stdout)["manual"]],
        )

    def test_rejects_generated_profile_unsafe_subtitle_target(self):
        source = self.root / "unsafe-subtitle"
        source.mkdir()
        (source / "Movie.2020.mkv").write_text("fixture movie", encoding="utf-8")
        (source / "Movie.2020.en?.srt").write_text("fixture subtitle", encoding="utf-8")
        batch = self.root / "unsafe-subtitle.batch.json"
        artifacts = self.root / "unsafe-subtitle.artifacts.json"

        result = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "jellyfin",
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
            check=False,
        )

        self.assertEqual(result.returncode, 3)
        self.assertIn(
            "unsafe_generated_name",
            [item["code"] for item in json.loads(result.stdout)["manual"]],
        )
        self.assertFalse(batch.exists())

    def test_allows_profile_reserved_characters_in_library_name(self):
        source = self.copy_fixture("movie-directory")
        batch = self.root / "library-name.batch.json"
        artifacts = self.root / "library-name.artifacts.json"

        result = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media:main",
            "--profile",
            "jellyfin",
            "--batch",
            batch,
            "--artifacts",
            artifacts,
            "--ffprobe",
            self.ffprobe,
        )

        self.assertEqual(json.loads(result.stdout)["status"], "ready")
        self.assertEqual(
            json.loads(batch.read_text(encoding="utf-8"))["library"],
            "media:main",
        )
        artifact_targets = [
            artifact["target"]
            for artifact in json.loads(
                artifacts.read_text(encoding="utf-8")
            )["artifacts"]
        ]
        self.assertTrue(artifact_targets)
        self.assertTrue(
            all(target.startswith("media:main/") for target in artifact_targets)
        )

    def test_returns_manual_for_overlong_generated_target_components(self):
        cases = [
            (
                "episode",
                "Show.S01E01.Pilot.mkv",
                {
                    "kind": "episodes",
                    "show_title": "S" * 180,
                    "episodes": {"S01E01": {"title": "E" * 180}},
                },
            ),
            (
                "provider",
                "movie.mkv",
                {
                    "kind": "movie",
                    "title": "Dune",
                    "provider_ids": {"tmdb": "9" * 260},
                },
            ),
        ]
        for case, source_name, metadata_value in cases:
            with self.subTest(case=case):
                case_root = self.root / case
                case_root.mkdir()
                source = case_root / source_name
                source.write_text("fixture video", encoding="utf-8")
                metadata = case_root / "metadata.json"
                metadata.write_text(
                    json.dumps(metadata_value),
                    encoding="utf-8",
                )
                batch = case_root / "batch.json"
                artifacts = case_root / "artifacts.json"

                result = self.run_tool(
                    "plan",
                    "--source",
                    source,
                    "--library",
                    "media",
                    "--profile",
                    "jellyfin",
                    "--metadata",
                    metadata,
                    "--batch",
                    batch,
                    "--artifacts",
                    artifacts,
                    "--ffprobe",
                    self.ffprobe,
                    check=False,
                )

                self.assertEqual(result.returncode, 3)
                report = json.loads(result.stdout)
                self.assertEqual(report["status"], "manual")
                self.assertIn(
                    "unsafe_generated_name",
                    [item["code"] for item in report["manual"]],
                )
                self.assertFalse(batch.exists())
                self.assertFalse(artifacts.exists())

    def test_returns_manual_for_duplicate_episode_number(self):
        source = self.root / "duplicate-episode"
        source.mkdir()
        (source / "Show.S01E01.First.mkv").write_text("fixture episode", encoding="utf-8")
        (source / "Show.S01E01.Second.mkv").write_text("fixture episode", encoding="utf-8")

        result = self.run_tool(
            "plan",
            "--source",
            source,
            "--library",
            "media",
            "--profile",
            "plex",
            "--batch",
            self.root / "duplicate.batch.json",
            "--artifacts",
            self.root / "duplicate.artifacts.json",
            "--ffprobe",
            self.ffprobe,
            check=False,
        )

        self.assertEqual(result.returncode, 3)
        self.assertIn(
            "duplicate_episode",
            [item["code"] for item in json.loads(result.stdout)["manual"]],
        )

    def test_validate_and_apply_require_exact_fresh_digest(self):
        batch = self.root / "batch.json"
        batch.write_text(
            json.dumps(
                {
                    "schema": "scrapefs.projection-batch/v1",
                    "library": "media",
                    "entries": [
                        {
                            "source": "/srv/media/Dune.mkv",
                            "target": "Movies/Dune/Dune.mkv",
                        }
                    ],
                }
            ),
            encoding="utf-8",
        )
        log = self.root / "scrapefs.log"
        scrapefs = self.make_fake_scrapefs(log)
        environment = os.environ.copy()
        environment["VALIDATION_DIGEST"] = "sha256:" + "a" * 64

        validate = self.run_tool(
            "validate",
            "--batch",
            batch,
            "--scrapefs",
            scrapefs,
            env=environment,
        )
        self.assertEqual(json.loads(validate.stdout)["digest"], environment["VALIDATION_DIGEST"])
        self.assertEqual(len(log.read_text().splitlines()), 1)
        self.assertIn("projection batch validate", log.read_text())
        self.assertNotIn("projection batch apply", log.read_text())

        log.write_text("", encoding="utf-8")
        missing = self.run_tool(
            "apply",
            "--batch",
            batch,
            "--scrapefs",
            scrapefs,
            check=False,
            env=environment,
        )
        self.assertEqual(missing.returncode, 2)
        self.assertEqual(log.read_text(encoding="utf-8"), "")

        wrong = self.run_tool(
            "apply",
            "--batch",
            batch,
            "--confirm-digest",
            "sha256:" + "b" * 64,
            "--scrapefs",
            scrapefs,
            check=False,
            env=environment,
        )
        self.assertEqual(wrong.returncode, 4)
        calls = log.read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(calls), 1)
        self.assertIn("projection batch validate", calls[0])

        log.write_text("", encoding="utf-8")
        applied = self.run_tool(
            "apply",
            "--batch",
            batch,
            "--confirm-digest",
            environment["VALIDATION_DIGEST"],
            "--scrapefs",
            scrapefs,
            env=environment,
        )
        payload = json.loads(applied.stdout)
        self.assertEqual(payload["validation"]["digest"], environment["VALIDATION_DIGEST"])
        self.assertEqual(payload["result"]["batch_id"], "batch-42")
        calls = log.read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(calls), 2)
        self.assertIn("projection batch validate", calls[0])
        self.assertIn("projection batch apply", calls[1])
        self.assertIn(f"--expected-digest {environment['VALIDATION_DIGEST']}", calls[1])

    def test_cli_failures_stop_at_the_failing_authorization_boundary(self):
        batch = self.root / "batch.json"
        batch.write_text(
            json.dumps(
                {
                    "schema": "scrapefs.projection-batch/v1",
                    "library": "media",
                    "entries": [
                        {
                            "source": "/srv/media/Dune.mkv",
                            "target": "Movies/Dune/Dune.mkv",
                        }
                    ],
                }
            ),
            encoding="utf-8",
        )
        log = self.root / "scrapefs.log"
        scrapefs = self.make_fake_scrapefs(log)
        environment = os.environ.copy()
        environment["VALIDATION_DIGEST"] = "sha256:" + "a" * 64
        environment["VALIDATE_FAIL"] = "1"

        validation_failure = self.run_tool(
            "apply",
            "--batch",
            batch,
            "--confirm-digest",
            environment["VALIDATION_DIGEST"],
            "--scrapefs",
            scrapefs,
            check=False,
            env=environment,
        )
        self.assertEqual(validation_failure.returncode, 1)
        self.assertEqual(len(log.read_text().splitlines()), 1)
        self.assertNotIn("projection batch apply", log.read_text())

        log.write_text("", encoding="utf-8")
        environment.pop("VALIDATE_FAIL")
        environment["APPLY_FAIL"] = "1"
        apply_failure = self.run_tool(
            "apply",
            "--batch",
            batch,
            "--confirm-digest",
            environment["VALIDATION_DIGEST"],
            "--scrapefs",
            scrapefs,
            check=False,
            env=environment,
        )
        self.assertEqual(apply_failure.returncode, 1)
        payload = json.loads(apply_failure.stdout)
        self.assertEqual(payload["schema"], "scrapefs.agent-apply-error/v1")
        self.assertEqual(payload["validation"]["digest"], environment["VALIDATION_DIGEST"])
        self.assertEqual(len(log.read_text().splitlines()), 2)

    def test_show_and_undo_use_existing_cli_contract(self):
        log = self.root / "scrapefs.log"
        scrapefs = self.make_fake_scrapefs(log)

        shown = self.run_tool("show", "batch-42", "--scrapefs", scrapefs)
        undone = self.run_tool("undo", "batch-42", "--scrapefs", scrapefs)

        self.assertEqual(json.loads(shown.stdout)["batch"]["batch_id"], "batch-42")
        self.assertEqual(json.loads(undone.stdout)["status"], "undone")
        calls = log.read_text(encoding="utf-8").splitlines()
        self.assertIn("projection batch show batch-42", calls[0])
        self.assertIn("projection batch undo batch-42", calls[1])

    def test_artifacts_are_written_independently_without_overwrite(self):
        target_directory = self.root / "mount" / "media" / "Movies" / "Dune"
        target_directory.mkdir(parents=True)
        nfo_source = self.root / "movie.nfo"
        nfo_source.write_text("<movie><title>Dune</title></movie>\n", encoding="utf-8")
        manifest = self.root / "artifacts.json"
        manifest.write_text(
            json.dumps(
                {
                    "schema": "scrapefs.agent-artifacts/v1",
                    "library": "media",
                    "artifacts": [
                        {
                            "kind": "nfo",
                            "source": str(nfo_source),
                            "target": "media/Movies/Dune/movie.nfo",
                        },
                        {
                            "kind": "artwork",
                            "source": str(self.root / "missing.jpg"),
                            "target": "media/Movies/Dune/poster.jpg",
                        },
                    ],
                }
            ),
            encoding="utf-8",
        )

        result = self.run_tool(
            "artifacts",
            "--manifest",
            manifest,
            "--mountpoint",
            self.root / "mount",
            check=False,
        )

        self.assertEqual(result.returncode, 5)
        report = json.loads(result.stdout)
        self.assertEqual(len(report["created"]), 1)
        self.assertEqual(len(report["failed"]), 1)
        created = target_directory / "movie.nfo"
        self.assertEqual(created.read_text(), nfo_source.read_text())

        second = self.run_tool(
            "artifacts",
            "--manifest",
            manifest,
            "--mountpoint",
            self.root / "mount",
            check=False,
        )
        self.assertEqual(second.returncode, 5)
        second_report = json.loads(second.stdout)
        self.assertEqual(len(second_report["created"]), 0)
        self.assertEqual(len(second_report["failed"]), 2)
        self.assertEqual(created.read_text(), nfo_source.read_text())

    def test_skill_runs_after_clean_copy_install(self):
        codex_home = self.root / "codex-home"
        installed = codex_home / "skills" / SKILL_DIR.name
        installed.parent.mkdir(parents=True)
        shutil.copytree(SKILL_DIR, installed)
        source = self.copy_fixture("movie-directory")
        batch = self.root / "installed.batch.json"
        artifacts = self.root / "installed.artifacts.json"

        help_result = subprocess.run(
            [sys.executable, str(installed / "scripts" / "scrapefs_media.py"), "--help"],
            check=False,
            capture_output=True,
            text=True,
            env={"PATH": os.environ["PATH"], "CODEX_HOME": str(codex_home)},
        )
        self.assertEqual(help_result.returncode, 0, help_result.stderr)
        plan_result = subprocess.run(
            [
                sys.executable,
                str(installed / "scripts" / "scrapefs_media.py"),
                "plan",
                "--source",
                str(source),
                "--library",
                "media",
                "--profile",
                "jellyfin",
                "--batch",
                str(batch),
                "--artifacts",
                str(artifacts),
                "--ffprobe",
                str(self.ffprobe),
            ],
            check=False,
            capture_output=True,
            text=True,
            env={"PATH": os.environ["PATH"], "CODEX_HOME": str(codex_home)},
        )
        self.assertEqual(plan_result.returncode, 0, plan_result.stderr)
        self.assertEqual(json.loads(plan_result.stdout)["status"], "ready")
        self.assertTrue(batch.exists())
        self.assertTrue(artifacts.exists())

    def make_fake_scrapefs(self, log):
        executable = self.root / "scrapefs"
        executable.write_text(
            textwrap.dedent(
                f"""\
                #!/usr/bin/env python3
                import json
                import os
                import pathlib
                import sys

                pathlib.Path({str(log)!r}).open("a", encoding="utf-8").write(
                    " ".join(sys.argv[1:]) + "\\n"
                )
                command = " ".join(sys.argv[1:])
                digest = os.environ.get("VALIDATION_DIGEST", "sha256:" + "a" * 64)
                if "projection batch validate" in command:
                    if os.environ.get("VALIDATE_FAIL"):
                        print(json.dumps({{
                            "schema": "scrapefs.error/v1",
                            "code": "TargetConflict",
                            "message": "target conflict"
                        }}), file=sys.stderr)
                        raise SystemExit(1)
                    print(json.dumps({{
                        "schema": "scrapefs.projection-batch-validation/v1",
                        "digest": digest,
                        "namespace_version": 7,
                        "valid": True,
                        "entries": [{{
                            "source": "/srv/media/Dune.mkv",
                            "target": "media:/Movies/Dune/Dune.mkv",
                            "action": "create"
                        }}],
                        "warnings": []
                    }}))
                elif "projection batch apply" in command:
                    if os.environ.get("APPLY_FAIL"):
                        print(json.dumps({{
                            "schema": "scrapefs.error/v1",
                            "code": "SourceChanged",
                            "message": "source changed after validation"
                        }}), file=sys.stderr)
                        raise SystemExit(1)
                    print(json.dumps({{
                        "schema": "scrapefs.projection-batch-result/v1",
                        "batch_id": "batch-42",
                        "digest": digest,
                        "namespace_version": 8
                    }}))
                elif "projection batch show" in command:
                    print(json.dumps({{
                        "schema": "scrapefs.projection-batch-detail/v1",
                        "batch": {{"batch_id": "batch-42"}},
                        "entries": [],
                        "operations": []
                    }}))
                elif "projection batch undo" in command:
                    print(json.dumps({{
                        "schema": "scrapefs.projection-batch-undo/v1",
                        "batch_id": "batch-42",
                        "status": "undone",
                        "namespace_version": 9,
                        "retained_overlay_files": []
                    }}))
                else:
                    raise SystemExit(2)
                """
            ),
            encoding="utf-8",
        )
        executable.chmod(0o755)
        return executable


if __name__ == "__main__":
    unittest.main()
