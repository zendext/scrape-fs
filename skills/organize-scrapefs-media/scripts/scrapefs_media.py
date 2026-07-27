#!/usr/bin/env python3
"""Deterministic local-media planning and ScrapeFS CLI orchestration."""

import argparse
import ctypes
import errno
import json
import os
from pathlib import Path, PurePosixPath
import re
import secrets
import shutil
import stat
import subprocess
import sys
import tempfile
import unicodedata
import xml.etree.ElementTree as ET


BATCH_SCHEMA = "scrapefs.projection-batch/v1"
VALIDATION_SCHEMA = "scrapefs.projection-batch-validation/v1"
RESULT_SCHEMA = "scrapefs.projection-batch-result/v1"
ARTIFACT_SCHEMA = "scrapefs.agent-artifacts/v1"
PLAN_SCHEMA = "scrapefs.agent-plan/v1"
DIGEST_PATTERN = re.compile(r"sha256:[0-9a-f]{64}\Z")
VIDEO_EXTENSIONS = {
    ".avi",
    ".m2ts",
    ".m4v",
    ".mkv",
    ".mov",
    ".mp4",
    ".mpeg",
    ".mpg",
    ".ts",
    ".webm",
}
SUBTITLE_EXTENSIONS = {".ass", ".smi", ".srt", ".ssa", ".sub", ".vtt"}
ARTWORK_EXTENSIONS = {".jpeg", ".jpg", ".png", ".tbn", ".webp"}
ARTWORK_STEMS = ("poster", "folder", "cover", "movie")
SUPPORTED_PROVIDER_IDS = {"imdb", "tmdb", "tvdb"}
DISC_COMPONENTS = {"bdmv", "video_ts"}
EXTRA_COMPONENTS = {
    "behind the scenes",
    "deleted scenes",
    "featurettes",
    "interviews",
    "other",
    "scenes",
    "shorts",
    "trailers",
}
EXTRA_SUFFIXES = {
    "behindthescenes",
    "deleted",
    "featurette",
    "interview",
    "other",
    "scene",
    "short",
    "trailer",
}
EDITION_TOKENS = {
    "directors cut",
    "director s cut",
    "extended",
    "extended edition",
    "final cut",
    "theatrical",
    "unrated",
}
MULTI_EPISODE_PATTERN = re.compile(
    r"(?i)S\d{1,2}E\d{1,3}(?:E\d{1,3}|-E\d{1,3})"
)
EPISODE_PATTERN = re.compile(r"(?i)S(?P<season>\d{1,2})E(?P<episode>\d{1,3})")
YEAR_PATTERN = re.compile(r"(?<!\d)((?:19|20)\d{2})(?!\d)")


class ManualHandling(Exception):
    def __init__(self, code, message, path=None):
        super().__init__(message)
        self.item = {"code": code, "message": message}
        if path is not None:
            self.item["path"] = os.fspath(path)


class CommandFailure(Exception):
    def __init__(self, command, returncode, stdout, stderr):
        super().__init__(f"{command} failed with exit code {returncode}")
        self.command = command
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


def build_parser():
    parser = argparse.ArgumentParser(
        description=(
            "Inspect local media, generate strict ScrapeFS Projection Batches, "
            "and orchestrate explicitly confirmed batch operations."
        )
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    plan = subparsers.add_parser(
        "plan",
        help="Inspect a local file or directory and write a batch plus artifact manifest.",
    )
    plan.add_argument("--source", required=True, help="Local media file or directory.")
    plan.add_argument("--library", required=True, help="ACTIVE ScrapeFS Library name.")
    plan.add_argument(
        "--profile",
        required=True,
        choices=("jellyfin", "plex"),
        help="Built-in Library Profile naming rules.",
    )
    plan.add_argument("--batch", required=True, help="Output Projection Batch JSON path.")
    plan.add_argument(
        "--artifacts",
        required=True,
        help="Output post-apply Overlay artifact manifest path.",
    )
    plan.add_argument(
        "--metadata",
        help="Optional Agent-resolved metadata JSON. No provider is called by this tool.",
    )
    plan.add_argument(
        "--ffprobe",
        default="ffprobe",
        help="ffprobe executable path or command name.",
    )
    plan.set_defaults(handler=handle_plan)

    validate = subparsers.add_parser(
        "validate",
        help="Run stateless batch validation and print the complete JSON review.",
    )
    add_control_arguments(validate)
    validate.add_argument("--batch", required=True, help="Projection Batch JSON path.")
    validate.set_defaults(handler=handle_validate)

    apply = subparsers.add_parser(
        "apply",
        help=(
            "Revalidate and apply only when --confirm-digest exactly matches the "
            "fresh validation digest."
        ),
    )
    add_control_arguments(apply)
    apply.add_argument("--batch", required=True, help="Projection Batch JSON path.")
    apply.add_argument(
        "--confirm-digest",
        required=True,
        help="Exact digest retained from the user-approved validation.",
    )
    apply.set_defaults(handler=handle_apply)

    show = subparsers.add_parser("show", help="Show applied or undone batch provenance.")
    add_control_arguments(show)
    show.add_argument("batch_id")
    show.set_defaults(handler=handle_show)

    undo = subparsers.add_parser("undo", help="Undo an applied Projection Batch.")
    add_control_arguments(undo)
    undo.add_argument("batch_id")
    undo.set_defaults(handler=handle_undo)

    artifacts = subparsers.add_parser(
        "artifacts",
        help="Create NFO and artwork through the mounted filesystem after apply.",
    )
    artifacts.add_argument("--manifest", required=True)
    artifacts.add_argument("--mountpoint", required=True)
    artifacts.set_defaults(handler=handle_artifacts)
    return parser


def add_control_arguments(parser):
    parser.add_argument("--scrapefs", default="scrapefs", help="scrapefs CLI path.")
    parser.add_argument("--socket", help="Optional ScrapeFS control socket path.")


def handle_plan(arguments):
    output_paths = plan_output_paths(arguments.batch, arguments.artifacts)
    invalidate_plan_outputs(output_paths)
    try:
        report, batch, artifacts = create_plan(
            arguments.source,
            arguments.library,
            arguments.profile,
            arguments.ffprobe,
            arguments.metadata,
        )
    except ManualHandling as error:
        report = manual_report(arguments.source, [error.item])
        write_json(sys.stdout, report)
        return 3

    if report["manual"]:
        report["status"] = "manual"
        write_json(sys.stdout, report)
        return 3

    try:
        write_json_file(output_paths[0], batch)
        write_json_file(output_paths[1], artifacts)
    except Exception:
        invalidate_plan_outputs(output_paths)
        raise
    report["batch_file"] = os.fspath(output_paths[0])
    report["artifacts_file"] = os.fspath(output_paths[1])
    write_json(sys.stdout, report)
    return 0


def handle_validate(arguments):
    try:
        result = validate_batch(arguments)
    except CommandFailure as error:
        write_json(sys.stdout, command_error(error))
        return 1
    write_json(sys.stdout, result)
    return 0


def handle_apply(arguments):
    if not DIGEST_PATTERN.fullmatch(arguments.confirm_digest):
        write_json(
            sys.stdout,
            {
                "schema": "scrapefs.agent-confirmation-error/v1",
                "code": "invalid_confirmation_digest",
                "message": "confirmation digest must use sha256:<64 lowercase hex>",
            },
        )
        return 4
    try:
        validation = validate_batch(arguments)
    except CommandFailure as error:
        write_json(sys.stdout, command_error(error))
        return 1
    if validation["digest"] != arguments.confirm_digest:
        write_json(
            sys.stdout,
            {
                "schema": "scrapefs.agent-confirmation-error/v1",
                "code": "confirmation_digest_mismatch",
                "message": (
                    "fresh validation digest does not match the retained "
                    "approved digest; review and approve the fresh validation"
                ),
                "confirmed_digest": arguments.confirm_digest,
                "validation": validation,
            },
        )
        return 4

    command = control_command(arguments) + [
        "projection",
        "batch",
        "apply",
        "--file",
        normalized_existing_file(arguments.batch),
        "--expected-digest",
        arguments.confirm_digest,
    ]
    try:
        result = run_json_command(command, "projection batch apply")
    except CommandFailure as error:
        write_json(
            sys.stdout,
            {
                "schema": "scrapefs.agent-apply-error/v1",
                "validation": validation,
                "error": command_error(error),
            },
        )
        return 1
    require_schema(result, RESULT_SCHEMA, "apply")
    if result.get("digest") != arguments.confirm_digest:
        raise RuntimeError("apply result digest does not match confirmed digest")
    write_json(
        sys.stdout,
        {
            "schema": "scrapefs.agent-apply/v1",
            "validation": validation,
            "result": result,
        },
    )
    return 0


def handle_show(arguments):
    return proxy_control(
        control_command(arguments)
        + ["projection", "batch", "show", arguments.batch_id],
        "projection batch show",
    )


def handle_undo(arguments):
    return proxy_control(
        control_command(arguments)
        + ["projection", "batch", "undo", arguments.batch_id],
        "projection batch undo",
    )


def handle_artifacts(arguments):
    manifest_path = normalized_existing_file(arguments.manifest)
    mountpoint = normalized_absolute_path(arguments.mountpoint)
    try:
        manifest = read_json_file(manifest_path)
        validate_artifact_manifest(manifest)
    except (OSError, ValueError) as error:
        write_json(
            sys.stdout,
            {
                "schema": "scrapefs.agent-artifact-result/v1",
                "created": [],
                "failed": [
                    {
                        "code": "invalid_manifest",
                        "message": str(error),
                    }
                ],
            },
        )
        return 5

    created = []
    failed = []
    for artifact in manifest["artifacts"]:
        try:
            create_artifact(mountpoint, artifact)
            created.append(
                {"kind": artifact["kind"], "target": artifact["target"]}
            )
        except (OSError, ValueError) as error:
            failed.append(
                {
                    "kind": artifact.get("kind"),
                    "target": artifact.get("target"),
                    "code": classify_artifact_error(error),
                    "message": str(error),
                }
            )
    write_json(
        sys.stdout,
        {
            "schema": "scrapefs.agent-artifact-result/v1",
            "created": created,
            "failed": failed,
        },
    )
    return 5 if failed else 0


def create_plan(source_value, library_name, profile, ffprobe, metadata_path):
    validate_library_name(library_name)
    if re.match(r"^[A-Za-z][A-Za-z0-9+.-]*://", os.fspath(source_value)):
        return (
            manual_report(
                source_value,
                [
                    manual_item(
                        "remote_source",
                        "remote and network source URLs require manual handling in v1",
                        source_value,
                    )
                ],
            ),
            None,
            None,
        )
    source = normalized_absolute_path(source_value)
    linked_component = first_symlink_component(source)
    if linked_component is not None:
        return (
            manual_report(
                source,
                [
                    manual_item(
                        "symlink_source",
                        "source path contains a symbolic-link component",
                        linked_component,
                    )
                ],
            ),
            None,
            None,
        )
    metadata = load_metadata(metadata_path)
    files, enumeration_manual = enumerate_source(source)
    ignored: list[dict] = []
    manual = list(enumeration_manual)

    for path in files:
        if any(
            part.casefold() in DISC_COMPONENTS
            for part in relative_parts(path, source)
        ):
            manual.append(
                manual_item(
                    "disc_layout",
                    "BDMV and VIDEO_TS layouts require manual handling in v1",
                    path,
                )
            )
    if manual:
        return manual_report(source, manual, ignored), None, None

    videos = []
    subtitles = []
    nfos = []
    artwork = []
    for path in files:
        if is_sample(path, source):
            ignored.append(
                {
                    "path": os.fspath(path),
                    "reason": "sample",
                }
            )
            continue
        if is_complex_extra(path, source):
            manual.append(
                manual_item(
                    "complex_extras",
                    "movie and show extras require manual handling in v1",
                    path,
                )
            )
            continue
        if is_edition(path):
            manual.append(
                manual_item(
                    "edition_detection",
                    "automatic edition detection requires manual handling in v1",
                    path,
                )
            )
            continue

        extension = path.suffix.casefold()
        if extension in VIDEO_EXTENSIONS:
            videos.append(path)
        elif extension in SUBTITLE_EXTENSIONS:
            subtitles.append(path)
        elif extension == ".nfo":
            nfos.append(path)
        elif extension in ARTWORK_EXTENSIONS:
            artwork.append(path)

    if manual:
        return manual_report(source, manual, ignored), None, None
    if not videos:
        return (
            manual_report(
                source,
                [
                    manual_item(
                        "unsupported_media",
                        "no supported local video files were found",
                        source,
                    )
                ],
                ignored,
            ),
            None,
            None,
        )

    probes = []
    for video in videos:
        try:
            probes.append(probe_video(video, ffprobe))
        except ManualHandling as error:
            manual.append(error.item)
    if manual:
        return manual_report(source, manual, ignored, probes), None, None

    layout, layout_manual = classify_layout(videos, nfos, metadata)
    if layout_manual:
        return manual_report(source, layout_manual, ignored, probes), None, None

    try:
        if layout == "movie":
            entries, artifact_items, warnings = plan_movie(
                source,
                videos[0],
                subtitles,
                nfos,
                artwork,
                library_name,
                profile,
                metadata,
            )
        else:
            entries, artifact_items, warnings = plan_episodes(
                source,
                videos,
                subtitles,
                nfos,
                artwork,
                library_name,
                profile,
                metadata,
            )
    except ManualHandling as error:
        return manual_report(source, [error.item], ignored, probes), None, None

    batch = {
        "schema": BATCH_SCHEMA,
        "library": library_name,
        "entries": entries,
    }
    artifacts = {
        "schema": ARTIFACT_SCHEMA,
        "library": library_name,
        "artifacts": artifact_items,
    }
    report = {
        "schema": PLAN_SCHEMA,
        "status": "ready",
        "source": os.fspath(source),
        "profile": f"{profile}@1",
        "kind": layout,
        "entries": entries,
        "ignored": sorted(ignored, key=lambda item: item["path"]),
        "warnings": warnings,
        "manual": [],
        "probes": probes,
        "artifacts": artifact_items,
    }
    return report, batch, artifacts


def enumerate_source(source):
    manual = []
    files = []

    def visit(path, recurse_directories=True):
        try:
            status = path.lstat()
        except OSError as error:
            manual.append(
                manual_item(
                    "source_access_failed",
                    f"cannot inspect source with invoking-user permissions: {error}",
                    path,
                )
            )
            return
        if stat.S_ISLNK(status.st_mode):
            manual.append(
                manual_item(
                    "symlink_source",
                    "source enumeration does not follow symbolic links",
                    path,
                )
            )
            return
        if stat.S_ISREG(status.st_mode):
            files.append(path)
            return
        if not stat.S_ISDIR(status.st_mode) or not recurse_directories:
            manual.append(
                manual_item(
                    "non_regular_source",
                    "source contains a non-regular filesystem object",
                    path,
                )
            )
            return
        try:
            with os.scandir(path) as iterator:
                children = sorted(iterator, key=lambda item: item.name)
        except OSError as error:
            manual.append(
                manual_item(
                    "source_access_failed",
                    f"cannot enumerate source with invoking-user permissions: {error}",
                    path,
                )
            )
            return
        for child in children:
            visit(path / child.name)

    visit(source)
    if files == [source] and EPISODE_PATTERN.search(source.stem) is None:
        try:
            with os.scandir(source.parent) as iterator:
                siblings = sorted(iterator, key=lambda item: item.name)
        except OSError as error:
            manual.append(
                manual_item(
                    "source_access_failed",
                    f"cannot enumerate adjacent sidecars with invoking-user permissions: {error}",
                    source.parent,
                )
            )
        else:
            for sibling in siblings:
                path = source.parent / sibling.name
                if path != source and is_adjacent_movie_sidecar(path, source):
                    visit(path, recurse_directories=False)
    return files, manual


def is_adjacent_movie_sidecar(path, video):
    extension = path.suffix.casefold()
    if extension in SUBTITLE_EXTENSIONS:
        return path.stem == video.stem or path.name.startswith(video.stem + ".")
    if extension == ".nfo":
        return path.name.casefold() == "movie.nfo" or path.stem == video.stem
    return (
        extension in ARTWORK_EXTENSIONS
        and path.stem.casefold() in ARTWORK_STEMS
    )


def first_symlink_component(path):
    current = Path(path.anchor)
    for component in path.parts[1:]:
        current /= component
        try:
            if stat.S_ISLNK(current.lstat().st_mode):
                return current
        except OSError:
            return None
    return None


def is_sample(path, source):
    relevant = relative_parts(path, source)
    for component in relevant:
        tokens = tokenize(component)
        if "sample" in tokens or component.casefold() in {"sample", "samples"}:
            return True
    return False


def is_complex_extra(path, source):
    relevant = relative_parts(path, source)
    if any(component.casefold() in EXTRA_COMPONENTS for component in relevant[:-1]):
        return True
    tokens = tokenize(path.stem)
    return bool(tokens and tokens[-1] in EXTRA_SUFFIXES)


def is_edition(path):
    normalized = " ".join(tokenize(path.stem))
    return any(token in normalized for token in EDITION_TOKENS)


def relative_parts(path, source):
    base = source if source.is_dir() else source.parent
    try:
        return path.relative_to(base).parts
    except ValueError:
        return path.parts


def tokenize(value):
    return [
        token
        for token in re.split(r"[^a-z0-9]+", value.casefold().replace("'", " "))
        if token
    ]


def probe_video(path, ffprobe):
    executable = shutil.which(ffprobe) if os.sep not in ffprobe else ffprobe
    if not executable or not os.path.isfile(executable):
        raise ManualHandling(
            "ffprobe_unavailable",
            f"ffprobe executable is unavailable: {ffprobe}",
            path,
        )
    command = [
        executable,
        "-v",
        "error",
        "-show_entries",
        "format=duration:format_tags=title,date:"
        "stream=index,codec_type,codec_name",
        "-of",
        "json",
        "--",
        os.fspath(path),
    ]
    try:
        completed = subprocess.run(
            command,
            check=False,
            capture_output=True,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ManualHandling(
            "probe_failed",
            f"ffprobe could not inspect the media: {error}",
            path,
        ) from error
    if completed.returncode != 0:
        detail = completed.stderr.strip() or f"exit code {completed.returncode}"
        raise ManualHandling(
            "probe_failed",
            f"ffprobe rejected the media: {detail}",
            path,
        )
    try:
        document = json.loads(completed.stdout)
    except json.JSONDecodeError as error:
        raise ManualHandling(
            "probe_failed",
            f"ffprobe returned invalid JSON: {error}",
            path,
        ) from error
    if not isinstance(document, dict):
        raise ManualHandling(
            "probe_failed",
            "ffprobe returned JSON that is not an object",
            path,
        )
    streams = document.get("streams")
    if not isinstance(streams, list):
        raise ManualHandling(
            "probe_failed",
            "ffprobe streams must be an array",
            path,
        )
    for stream in streams:
        if not isinstance(stream, dict):
            raise ManualHandling(
                "probe_failed",
                "ffprobe stream entries must be objects",
                path,
            )
        codec_type = stream.get("codec_type")
        codec_name = stream.get("codec_name")
        if codec_type is not None and not isinstance(codec_type, str):
            raise ManualHandling(
                "probe_failed",
                "ffprobe stream codec_type must be a string",
                path,
            )
        if codec_name is not None and not isinstance(codec_name, str):
            raise ManualHandling(
                "probe_failed",
                "ffprobe stream codec_name must be a string",
                path,
            )
    if not any(stream.get("codec_type") == "video" for stream in streams):
        raise ManualHandling(
            "probe_failed",
            "ffprobe found no video stream",
            path,
        )
    format_data = document.get("format")
    if not isinstance(format_data, dict):
        format_data = {}
    return {
        "path": os.fspath(path),
        "duration_seconds": parse_duration(format_data.get("duration")),
        "video_codecs": sorted(
            {
                stream.get("codec_name", "")
                for stream in streams
                if isinstance(stream, dict)
                and stream.get("codec_type") == "video"
                and stream.get("codec_name")
            }
        ),
        "audio_codecs": sorted(
            {
                stream.get("codec_name", "")
                for stream in streams
                if isinstance(stream, dict)
                and stream.get("codec_type") == "audio"
                and stream.get("codec_name")
            }
        ),
    }


def parse_duration(value):
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def classify_layout(videos, nfos, metadata):
    kind = metadata.get("kind")
    if kind not in (None, "movie", "episodes"):
        return None, [
            manual_item(
                "metadata_invalid",
                'metadata "kind" must be "movie" or "episodes"',
            )
        ]
    multi_episode_videos = [
        video for video in videos if is_multi_episode_filename(video.stem)
    ]
    if multi_episode_videos:
        return None, [
            manual_item(
                "multi_episode_file",
                "a file containing multiple episodes requires manual handling in v1",
                video,
            )
            for video in multi_episode_videos
        ]

    episode_matches = [EPISODE_PATTERN.search(video.stem) for video in videos]
    has_tvshow_nfo = any(path.name.casefold() == "tvshow.nfo" for path in nfos)
    if all(episode_matches):
        detected = "episodes"
    elif any(episode_matches):
        return None, [
            manual_item(
                "ambiguous_layout",
                "source mixes standard episodes with non-episode video files",
            )
        ]
    elif len(videos) == 1 and not has_tvshow_nfo:
        detected = "movie"
    elif has_tvshow_nfo:
        return None, [
            manual_item(
                "absolute_episode_numbering",
                "TV sources without standard SxxEyy numbering require manual handling",
            )
        ]
    else:
        return None, [
            manual_item(
                "ambiguous_layout",
                "multiple non-episode video files cannot be identified as one v1 item",
            )
        ]
    if kind is not None and kind != detected:
        return None, [
            manual_item(
                "metadata_layout_mismatch",
                f'metadata kind "{kind}" conflicts with detected {detected} layout',
            )
        ]
    return detected, []


def plan_movie(
    source,
    video,
    subtitles,
    nfos,
    artwork,
    library,
    profile,
    metadata,
):
    warnings = []
    nfo_path = first_existing(
        [
            video.with_suffix(".nfo"),
            video.parent / "movie.nfo",
        ],
        nfos,
    )
    nfo = parse_nfo(nfo_path, "movie", warnings) if nfo_path else {}
    title = metadata_text(metadata, "title") or nfo.get("title")
    inferred_title, inferred_year = infer_movie_name(video.stem)
    title = title or inferred_title
    year = metadata_year(metadata, "year") or nfo.get("year") or inferred_year
    provider_ids = merge_provider_ids(nfo.get("provider_ids"), metadata.get("provider_ids"))
    validate_title(title, video)
    display = title_with_year(sanitize_name(title, profile), year)
    hint = provider_hint(profile, provider_ids, movie=True)
    item_name = display + hint
    directory = PurePosixPath("Movies") / item_name
    target_base = item_name

    entries = [
        {
            "source": os.fspath(video),
            "target": (directory / (target_base + video.suffix.casefold())).as_posix(),
        }
    ]
    matched_subtitles, unmatched = associate_subtitles([video], subtitles)
    for subtitle in matched_subtitles.get(video, []):
        suffix = subtitle.name[len(video.stem) :]
        entries.append(
            {
                "source": os.fspath(subtitle),
                "target": (directory / (target_base + suffix)).as_posix(),
            }
        )
    warnings.extend(unmatched_subtitle_warnings(unmatched))

    artifacts = []
    if nfo_path:
        artifacts.append(
            artifact_from_source(
                "nfo",
                nfo_path,
                library_path(library, directory / "movie.nfo"),
            )
        )
    else:
        artifacts.append(
            artifact_from_content(
                "nfo",
                movie_nfo(title, year, provider_ids),
                library_path(library, directory / "movie.nfo"),
            )
        )
    selected_artwork, artwork_warnings = choose_artwork(artwork, source)
    warnings.extend(artwork_warnings)
    if selected_artwork:
        artifacts.append(
            artifact_from_source(
                "artwork",
                selected_artwork,
                library_path(
                    library,
                    directory / ("poster" + selected_artwork.suffix.casefold()),
                ),
            )
        )
    validate_targets(entries, artifacts, profile)
    return entries, artifacts, warnings


def plan_episodes(
    source,
    videos,
    subtitles,
    nfos,
    artwork,
    library,
    profile,
    metadata,
):
    warnings = []
    tvshow_path = first_named(nfos, "tvshow.nfo")
    tvshow = parse_nfo(tvshow_path, "tvshow", warnings) if tvshow_path else {}
    inferred_show_title, inferred_show_year = infer_episode_series(videos)
    show_title = (
        metadata_text(metadata, "show_title")
        or tvshow.get("title")
        or inferred_show_title
    )
    show_year = (
        metadata_year(metadata, "show_year")
        or tvshow.get("year")
        or inferred_show_year
    )
    provider_ids = merge_provider_ids(
        tvshow.get("provider_ids"),
        metadata.get("provider_ids"),
    )
    validate_title(show_title, videos[0])
    show_display = title_with_year(sanitize_name(show_title, profile), show_year)
    show_name = show_display + provider_hint(profile, provider_ids, movie=False)
    root_name = "Shows" if profile == "jellyfin" else "TV Shows"
    show_directory = PurePosixPath(root_name) / show_name
    matched_subtitles, unmatched = associate_subtitles(videos, subtitles)
    warnings.extend(unmatched_subtitle_warnings(unmatched))
    entries = []
    artifacts = []
    seen_episodes = set()

    if tvshow_path:
        artifacts.append(
            artifact_from_source(
                "nfo",
                tvshow_path,
                library_path(library, show_directory / "tvshow.nfo"),
            )
        )
    else:
        artifacts.append(
            artifact_from_content(
                "nfo",
                tvshow_nfo(show_title, show_year, provider_ids),
                library_path(library, show_directory / "tvshow.nfo"),
            )
        )

    for video in sorted(videos, key=lambda path: path.name):
        match = EPISODE_PATTERN.search(video.stem)
        if match is None:
            raise ManualHandling(
                "ambiguous_layout",
                "episode planning requires standard SxxEyy filenames",
                video,
            )
        season = int(match.group("season"))
        episode = int(match.group("episode"))
        episode_key = f"S{season:02d}E{episode:02d}"
        if episode_key in seen_episodes:
            raise ManualHandling(
                "duplicate_episode",
                f"multiple files claim episode {episode_key}",
                video,
            )
        seen_episodes.add(episode_key)
        episode_nfo_path = first_existing([video.with_suffix(".nfo")], nfos)
        episode_nfo_data = (
            parse_nfo(episode_nfo_path, "episodedetails", warnings)
            if episode_nfo_path
            else {}
        )
        title = (
            metadata_episode_title(metadata, episode_key)
            or episode_nfo_data.get("title")
            or infer_episode_title(video.stem, match.end())
        )
        title = sanitize_name(title, profile) if title else ""
        if profile == "jellyfin":
            episode_base = f"{show_display} {episode_key}"
            if title:
                episode_base += f" {title}"
        else:
            episode_base = f"{show_display} - {episode_key}"
            if title:
                episode_base += f" - {title}"
        season_directory = show_directory / f"Season {season:02d}"
        entries.append(
            {
                "source": os.fspath(video),
                "target": (
                    season_directory / (episode_base + video.suffix.casefold())
                ).as_posix(),
            }
        )
        for subtitle in matched_subtitles.get(video, []):
            suffix = subtitle.name[len(video.stem) :]
            entries.append(
                {
                    "source": os.fspath(subtitle),
                    "target": (season_directory / (episode_base + suffix)).as_posix(),
                }
            )
        nfo_target = season_directory / (episode_base + ".nfo")
        if episode_nfo_path:
            artifacts.append(
                artifact_from_source(
                    "nfo",
                    episode_nfo_path,
                    library_path(library, nfo_target),
                )
            )
        else:
            artifacts.append(
                artifact_from_content(
                    "nfo",
                    episode_nfo(title, season, episode),
                    library_path(library, nfo_target),
                )
            )

    selected_artwork, artwork_warnings = choose_artwork(artwork, source)
    warnings.extend(artwork_warnings)
    if selected_artwork:
        artifacts.append(
            artifact_from_source(
                "artwork",
                selected_artwork,
                library_path(
                    library,
                    show_directory / ("poster" + selected_artwork.suffix.casefold()),
                ),
            )
        )
    validate_targets(entries, artifacts, profile)
    return entries, artifacts, warnings


def first_existing(candidates, available):
    available_set = set(available)
    return next((path for path in candidates if path in available_set), None)


def first_named(paths, name):
    matching = [path for path in paths if path.name.casefold() == name.casefold()]
    return sorted(matching, key=os.fspath)[0] if matching else None


def parse_nfo(path, expected_root, warnings=None):
    try:
        root = ET.parse(path).getroot()
    except (OSError, ET.ParseError) as error:
        raise ManualHandling(
            "nfo_invalid",
            f"cannot read existing NFO: {error}",
            path,
        ) from error
    if root.tag.casefold() != expected_root:
        raise ManualHandling(
            "nfo_invalid",
            f"expected <{expected_root}> NFO root, found <{root.tag}>",
            path,
        )
    result = {}
    title = child_text(root, "title")
    if title:
        result["title"] = title
    year_text = child_text(root, "year")
    if not year_text:
        premiered = child_text(root, "premiered") or child_text(root, "aired")
        if premiered:
            match = YEAR_PATTERN.search(premiered)
            year_text = match.group(1) if match else None
    if year_text:
        try:
            result["year"] = validate_year(int(year_text))
        except (TypeError, ValueError):
            raise ManualHandling(
                "nfo_invalid",
                f"NFO year is invalid: {year_text!r}",
                path,
            )
    provider_ids = {}
    for child in root.findall("uniqueid"):
        provider = (child.get("type") or "").casefold()
        value = (child.text or "").strip()
        if provider and value:
            if provider not in SUPPORTED_PROVIDER_IDS:
                if warnings is not None:
                    warnings.append(
                        {
                            "code": "unsupported_provider_id",
                            "message": (
                                "ignored lower-priority NFO provider ID "
                                f"{provider!r}={value!r}"
                            ),
                            "path": os.fspath(path),
                        }
                    )
                continue
            try:
                provider_ids[provider] = validate_provider_id(provider, value)
            except ValueError as error:
                raise ManualHandling(
                    "nfo_invalid",
                    f"NFO provider ID is invalid: {error}",
                    path,
                ) from error
    if provider_ids:
        result["provider_ids"] = provider_ids
    for field in ("season", "episode"):
        value = child_text(root, field)
        if value is None:
            continue
        try:
            result[field] = int(value)
        except ValueError as error:
            raise ManualHandling(
                "nfo_invalid",
                f"NFO {field} is invalid: {value!r}",
                path,
            ) from error
    return result


def child_text(root, name):
    child = root.find(name)
    if child is None or child.text is None:
        return None
    value = child.text.strip()
    return value or None


def infer_movie_name(stem):
    year_match = YEAR_PATTERN.search(stem)
    year = int(year_match.group(1)) if year_match else None
    title_part = stem
    if year_match:
        title_part = stem[: year_match.start()]
        before = title_part.rstrip()
        after = stem[year_match.end() :].lstrip()
        delimiters = {"(": ")", "[": "]", "{": "}"}
        if before and before[-1] in delimiters and after.startswith(
            delimiters[before[-1]]
        ):
            title_part = before[:-1].rstrip()
    return humanize_filename(title_part), year


def is_multi_episode_filename(stem):
    return bool(MULTI_EPISODE_PATTERN.search(stem)) or sum(
        1 for _ in EPISODE_PATTERN.finditer(stem)
    ) > 1


def infer_show_title_and_year(stem, episode_start):
    prefix = stem[:episode_start].rstrip(" ._-")
    matches = list(YEAR_PATTERN.finditer(prefix))
    if not matches:
        return humanize_filename(prefix), None
    year_match = matches[-1]
    trailing = prefix[year_match.end() :].strip()
    before = prefix[: year_match.start()].rstrip()
    if trailing:
        delimiters = {")": "(", "]": "[", "}": "{"}
        if trailing not in delimiters or not before.endswith(delimiters[trailing]):
            return humanize_filename(prefix), None
        before = before[:-1].rstrip()
    elif year_match.end() != len(prefix):
        return humanize_filename(prefix), None
    title = humanize_filename(before)
    if not title:
        return humanize_filename(prefix), None
    return title, int(year_match.group(1))


def infer_episode_series(videos):
    inferred = []
    identities: dict[str, tuple[str, Path]] = {}
    years: dict[int, Path] = {}
    for video in sorted(videos, key=os.fspath):
        match = EPISODE_PATTERN.search(video.stem)
        if match is None:
            raise ManualHandling(
                "ambiguous_layout",
                "episode planning requires standard SxxEyy filenames",
                video,
            )
        title, year = infer_show_title_and_year(video.stem, match.start())
        if not title:
            continue
        identity = title.casefold()
        identities.setdefault(identity, (title, video))
        inferred.append(title)
        if year is not None:
            years.setdefault(year, video)
    if len(identities) > 1:
        titles = ", ".join(sorted(value[0] for value in identities.values()))
        conflicting = sorted(
            (value[1] for value in identities.values()),
            key=os.fspath,
        )[1]
        raise ManualHandling(
            "mixed_series",
            f"episode directory contains multiple inferred series: {titles}",
            conflicting,
        )
    if len(years) > 1:
        values = ", ".join(str(year) for year in sorted(years))
        conflicting = sorted(years.values(), key=os.fspath)[1]
        raise ManualHandling(
            "mixed_series",
            f"episode directory contains multiple inferred show years: {values}",
            conflicting,
        )
    year = next(iter(years), None)
    return (inferred[0] if inferred else None), year


def infer_episode_title(stem, episode_end):
    remainder = stem[episode_end:]
    return humanize_filename(remainder) if remainder.strip(" ._-") else None


def humanize_filename(value):
    value = re.sub(r"[._]+", " ", value)
    value = re.sub(r"\s*-\s*", " ", value)
    return re.sub(r"\s+", " ", value).strip()


def metadata_text(metadata, key):
    value = metadata.get(key)
    if value is None:
        return None
    if not isinstance(value, str) or not value.strip():
        raise ManualHandling(
            "metadata_invalid",
            f'metadata "{key}" must be a non-empty string',
        )
    return value.strip()


def metadata_year(metadata, key):
    value = metadata.get(key)
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int):
        raise ManualHandling(
            "metadata_invalid",
            f'metadata "{key}" must be an integer year',
        )
    try:
        return validate_year(value)
    except ValueError as error:
        raise ManualHandling("metadata_invalid", str(error)) from error


def metadata_episode_title(metadata, key):
    episodes = metadata.get("episodes")
    if episodes is None:
        return None
    if not isinstance(episodes, dict):
        raise ManualHandling(
            "metadata_invalid",
            'metadata "episodes" must be an object keyed by SxxEyy',
        )
    record = episodes.get(key)
    if record is None:
        return None
    if not isinstance(record, dict):
        raise ManualHandling(
            "metadata_invalid",
            f'metadata episode "{key}" must be an object',
        )
    value = record.get("title")
    if value is None:
        return None
    if not isinstance(value, str) or not value.strip():
        raise ManualHandling(
            "metadata_invalid",
            f'metadata episode "{key}" title must be a non-empty string',
        )
    return value.strip()


def merge_provider_ids(nfo_ids, metadata_ids):
    merged = dict(nfo_ids or {})
    if metadata_ids is None:
        return merged
    if not isinstance(metadata_ids, dict):
        raise ManualHandling(
            "metadata_invalid",
            'metadata "provider_ids" must be an object',
        )
    for provider, value in metadata_ids.items():
        if not isinstance(provider, str) or not isinstance(value, str):
            raise ManualHandling(
                "metadata_invalid",
                "metadata provider IDs must map string names to string values",
            )
        try:
            merged[provider.casefold()] = validate_provider_id(provider, value)
        except ValueError as error:
            raise ManualHandling("metadata_invalid", str(error)) from error
    return merged


def validate_provider_id(provider, value):
    provider = provider.casefold()
    value = value.strip()
    if provider in {"tmdb", "tvdb"}:
        valid = value.isdigit()
    elif provider == "imdb":
        valid = bool(re.fullmatch(r"tt\d+", value))
    else:
        valid = False
    if not valid:
        raise ValueError(f"unsupported or invalid {provider!r} provider ID {value!r}")
    return value


def validate_year(year):
    if year < 1870 or year > 2200:
        raise ValueError(f"year {year!r} is outside the supported range")
    return year


def validate_title(title, path):
    if not title or not title.strip():
        raise ManualHandling(
            "ambiguous_metadata",
            "media title could not be determined",
            path,
        )


def sanitize_name(value, profile):
    value = "".join(" " if ord(character) < 32 else character for character in value)
    value = value.replace("/", " - ").replace("\\", " - ")
    if profile == "jellyfin":
        value = re.sub(r'[<>:"|?*]', "-", value)
    value = re.sub(r"\s+", " ", value).strip(" .")
    value = re.sub(r"\s*-\s*", " - ", value)
    if not value:
        raise ManualHandling(
            "unsafe_generated_name",
            "metadata produced an empty target name after path sanitization",
        )
    if len(value.encode("utf-8")) > 180:
        raise ManualHandling(
            "unsafe_generated_name",
            "metadata target name exceeds the v1 deterministic naming limit",
        )
    return value


def title_with_year(title, year):
    return f"{title} ({year})" if year else title


def provider_hint(profile, provider_ids, movie):
    order = ("tmdb", "imdb") if movie else ("tvdb", "tmdb", "imdb")
    for provider in order:
        value = provider_ids.get(provider)
        if not value:
            continue
        if profile == "jellyfin":
            return f" [{provider}id-{value}]"
        return f" {{{provider}-{value}}}"
    return ""


def associate_subtitles(videos, subtitles):
    associations: dict[Path, list[Path]] = {video: [] for video in videos}
    unmatched = []
    ordered_videos = sorted(videos, key=lambda path: (-len(path.stem), path.name))
    for subtitle in sorted(subtitles, key=os.fspath):
        matches = [
            video
            for video in ordered_videos
            if subtitle.parent == video.parent
            and (
                subtitle.stem == video.stem
                or subtitle.name.startswith(video.stem + ".")
            )
        ]
        if len(matches) == 1:
            associations[matches[0]].append(subtitle)
        else:
            unmatched.append(subtitle)
    return associations, unmatched


def unmatched_subtitle_warnings(paths):
    return [
        {
            "code": "unmatched_subtitle",
            "message": "subtitle was not projected because it did not match one video stem",
            "path": os.fspath(path),
        }
        for path in paths
    ]


def choose_artwork(paths, source):
    base = source if source.is_dir() else source.parent
    candidates = [
        path
        for path in paths
        if path.parent == base and path.stem.casefold() in ARTWORK_STEMS
    ]
    candidates.sort(
        key=lambda path: (
            ARTWORK_STEMS.index(path.stem.casefold()),
            path.suffix.casefold(),
            path.name,
        )
    )
    warnings = []
    if len(candidates) > 1:
        warnings.append(
            {
                "code": "multiple_artwork_candidates",
                "message": "only the highest-priority poster artwork was selected",
                "paths": [os.fspath(path) for path in candidates],
            }
        )
    return (candidates[0] if candidates else None), warnings


def artifact_from_source(kind, source, target):
    return {
        "kind": kind,
        "source": os.fspath(source),
        "target": target,
    }


def artifact_from_content(kind, content, target):
    return {
        "kind": kind,
        "content": content,
        "target": target,
    }


def library_path(library, path):
    return (PurePosixPath(library) / path).as_posix()


def movie_nfo(title, year, provider_ids):
    return build_nfo("movie", title, year, provider_ids)


def tvshow_nfo(title, year, provider_ids):
    return build_nfo("tvshow", title, year, provider_ids)


def episode_nfo(title, season, episode):
    root = ET.Element("episodedetails")
    if title:
        ET.SubElement(root, "title").text = title
    ET.SubElement(root, "season").text = str(season)
    ET.SubElement(root, "episode").text = str(episode)
    return serialize_xml(root)


def build_nfo(root_name, title, year, provider_ids):
    root = ET.Element(root_name)
    ET.SubElement(root, "title").text = title
    if year:
        ET.SubElement(root, "year").text = str(year)
    for provider in sorted(provider_ids):
        child = ET.SubElement(root, "uniqueid")
        child.set("type", provider)
        child.text = provider_ids[provider]
    return serialize_xml(root)


def serialize_xml(root):
    ET.indent(root, space="  ")
    return ET.tostring(root, encoding="unicode", xml_declaration=True) + "\n"


def validate_targets(entries, artifacts, profile):
    sources = set()
    targets = set()
    for entry in entries:
        source = entry["source"]
        target = entry["target"]
        if source in sources:
            raise ManualHandling(
                "duplicate_source",
                f"generated batch duplicates source {source!r}",
            )
        if target in targets:
            raise ManualHandling(
                "duplicate_target",
                f"generated batch duplicates target {target!r}",
            )
        sources.add(source)
        targets.add(target)
        validate_generated_target(target, profile)
    for artifact in artifacts:
        validate_generated_target(
            artifact["target"],
            profile,
            includes_library=True,
        )


def validate_generated_target(target, profile, includes_library=False):
    try:
        parts = validate_relative_posix_path(target)
    except ValueError as error:
        raise ManualHandling(
            "unsafe_generated_name",
            f"generated target path is invalid: {error}",
            target,
        ) from error
    if includes_library:
        if len(parts) < 2:
            raise ManualHandling(
                "unsafe_generated_name",
                "generated artifact target must include a Library name and path",
                target,
            )
        parts = parts[1:]
    validate_profile_target(target, profile, parts)


def validate_profile_target(target, profile, components=None):
    if components is None:
        components = PurePosixPath(target).parts
    for component in components:
        if any(unicodedata.category(character) == "Cc" for character in component):
            raise ManualHandling(
                "unsafe_generated_name",
                f"generated {profile}@1 target contains a control character",
                target,
            )
        if profile == "jellyfin" and any(
            character in '<>:"\\|?*' for character in component
        ):
            raise ManualHandling(
                "unsafe_generated_name",
                f"generated jellyfin@1 target contains a reserved character",
                target,
            )


def load_metadata(path):
    if not path:
        return {}
    try:
        value = read_json_file(normalized_existing_file(path))
    except (OSError, ValueError) as error:
        raise ManualHandling(
            "metadata_invalid",
            f"cannot read metadata JSON: {error}",
            path,
        ) from error
    if not isinstance(value, dict):
        raise ManualHandling(
            "metadata_invalid",
            "metadata JSON must contain an object",
            path,
        )
    allowed = {
        "kind",
        "title",
        "year",
        "show_title",
        "show_year",
        "provider_ids",
        "episodes",
    }
    unknown = sorted(set(value) - allowed)
    if unknown:
        raise ManualHandling(
            "metadata_invalid",
            f"metadata JSON has unknown fields: {', '.join(unknown)}",
            path,
        )
    return value


def manual_report(source, manual, ignored=None, probes=None):
    return {
        "schema": PLAN_SCHEMA,
        "status": "manual",
        "source": os.fspath(source),
        "kind": None,
        "entries": [],
        "ignored": sorted(ignored or [], key=lambda item: item["path"]),
        "warnings": [],
        "manual": manual,
        "probes": probes or [],
        "artifacts": [],
    }


def manual_item(code, message, path=None):
    item = {"code": code, "message": message}
    if path is not None:
        item["path"] = os.fspath(path)
    return item


def validate_batch(arguments):
    command = control_command(arguments) + [
        "projection",
        "batch",
        "validate",
        "--file",
        normalized_existing_file(arguments.batch),
        "--json",
    ]
    result = run_json_command(command, "projection batch validate")
    require_schema(result, VALIDATION_SCHEMA, "validate")
    digest = result.get("digest")
    if not isinstance(digest, str) or not DIGEST_PATTERN.fullmatch(digest):
        raise RuntimeError("validate returned an invalid confirmation digest")
    if result.get("valid") is not True:
        raise RuntimeError("validate returned a non-valid result without an error")
    if not isinstance(result.get("entries"), list) or not isinstance(
        result.get("warnings"), list
    ):
        raise RuntimeError("validate returned invalid entries or warnings")
    return result


def control_command(arguments):
    command = [arguments.scrapefs]
    if arguments.socket:
        command.extend(["--socket", arguments.socket])
    return command


def proxy_control(command, name):
    try:
        result = run_json_command(command, name)
    except CommandFailure as error:
        write_json(sys.stdout, command_error(error))
        return 1
    write_json(sys.stdout, result)
    return 0


def run_json_command(command, name):
    try:
        completed = subprocess.run(
            command,
            check=False,
            capture_output=True,
            text=True,
        )
    except OSError as error:
        raise CommandFailure(name, 127, "", str(error)) from error
    if completed.returncode != 0:
        raise CommandFailure(
            name,
            completed.returncode,
            completed.stdout,
            completed.stderr,
        )
    try:
        result = json.loads(completed.stdout)
    except json.JSONDecodeError as error:
        raise CommandFailure(
            name,
            completed.returncode,
            completed.stdout,
            f"invalid JSON output: {error}",
        ) from error
    if not isinstance(result, dict):
        raise CommandFailure(
            name,
            completed.returncode,
            completed.stdout,
            "JSON output is not an object",
        )
    return result


def require_schema(document, schema, operation):
    if document.get("schema") != schema:
        raise RuntimeError(
            f"{operation} returned schema {document.get('schema')!r}, want {schema!r}"
        )


def command_error(error):
    parsed = None
    for candidate in (error.stderr.strip(), error.stdout.strip()):
        if not candidate:
            continue
        try:
            parsed = json.loads(candidate)
        except json.JSONDecodeError:
            continue
        break
    return {
        "schema": "scrapefs.agent-command-error/v1",
        "command": error.command,
        "exit_code": error.returncode,
        "error": parsed
        if isinstance(parsed, dict)
        else (error.stderr.strip() or error.stdout.strip() or str(error)),
    }


def validate_artifact_manifest(manifest):
    if not isinstance(manifest, dict):
        raise ValueError("artifact manifest must be a JSON object")
    if set(manifest) != {"schema", "library", "artifacts"}:
        raise ValueError("artifact manifest must contain only schema, library, artifacts")
    if manifest["schema"] != ARTIFACT_SCHEMA:
        raise ValueError(f"artifact manifest schema must be {ARTIFACT_SCHEMA}")
    validate_library_name(manifest["library"])
    if not isinstance(manifest["artifacts"], list):
        raise ValueError("artifact manifest artifacts must be an array")
    targets = set()
    for index, artifact in enumerate(manifest["artifacts"]):
        if not isinstance(artifact, dict):
            raise ValueError(f"artifact {index} must be an object")
        expected = {"kind", "target"}
        sources = int("source" in artifact) + int("content" in artifact)
        if sources != 1:
            raise ValueError(
                f"artifact {index} must contain exactly one of source or content"
            )
        expected.add("source" if "source" in artifact else "content")
        if set(artifact) != expected:
            raise ValueError(f"artifact {index} contains unknown fields")
        if artifact["kind"] not in {"nfo", "artwork"}:
            raise ValueError(f"artifact {index} has unsupported kind")
        if not isinstance(artifact["target"], str):
            raise ValueError(f"artifact {index} target must be a string")
        parts = validate_relative_posix_path(artifact["target"])
        if parts[0] != manifest["library"]:
            raise ValueError(f"artifact {index} target is outside its Library")
        if artifact["target"] in targets:
            raise ValueError(f"artifact {index} duplicates a target")
        targets.add(artifact["target"])
        if "source" in artifact:
            if not isinstance(artifact["source"], str) or not artifact["source"]:
                raise ValueError(f"artifact {index} source must be a non-empty string")
        elif not isinstance(artifact["content"], str):
            raise ValueError(f"artifact {index} content must be a string")


def create_artifact(mountpoint, artifact):
    components = validate_relative_posix_path(artifact["target"])
    parent_fd = open_directory_beneath(mountpoint, components[:-1])
    temporary_name = (
        f".scrapefs-agent-{os.getpid()}-{secrets.token_hex(6)}.tmp"
    )
    file_fd = None
    try:
        file_fd = os.open(
            temporary_name,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o666,
            dir_fd=parent_fd,
        )
        if "source" in artifact:
            copy_regular_file(artifact["source"], file_fd)
        else:
            write_all(file_fd, artifact["content"].encode("utf-8"))
        os.fsync(file_fd)
        os.close(file_fd)
        file_fd = None
        rename_noreplace(parent_fd, temporary_name, components[-1])
    finally:
        if file_fd is not None:
            os.close(file_fd)
        try:
            os.unlink(temporary_name, dir_fd=parent_fd)
        except FileNotFoundError:
            pass
        os.close(parent_fd)


def open_directory_beneath(root, components):
    root = normalized_absolute_path(root)
    fd = os.open(
        root.anchor,
        os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
    )
    try:
        for component in (*root.parts[1:], *components):
            next_fd = os.open(
                component,
                os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
                dir_fd=fd,
            )
            os.close(fd)
            fd = next_fd
        return fd
    except Exception:
        os.close(fd)
        raise


def copy_regular_file(source, destination_fd):
    source_path = normalized_absolute_path(source)
    source_fd = os.open(
        source_path,
        os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW,
    )
    try:
        status = os.fstat(source_fd)
        if not stat.S_ISREG(status.st_mode):
            raise ValueError(f"artifact source is not a regular file: {source_path}")
        while True:
            chunk = os.read(source_fd, 1024 * 1024)
            if not chunk:
                break
            write_all(destination_fd, chunk)
    finally:
        os.close(source_fd)


def write_all(fd, data):
    view = memoryview(data)
    while view:
        written = os.write(fd, view)
        if written <= 0:
            raise OSError(errno.EIO, "artifact write made no progress")
        view = view[written:]


def rename_noreplace(directory_fd, source, destination):
    libc = ctypes.CDLL(None, use_errno=True)
    function = getattr(libc, "renameat2", None)
    if function is None:
        raise OSError(errno.ENOSYS, "renameat2 is required for no-replace artifacts")
    function.argtypes = [
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_uint,
    ]
    function.restype = ctypes.c_int
    result = function(
        directory_fd,
        os.fsencode(source),
        directory_fd,
        os.fsencode(destination),
        1,
    )
    if result != 0:
        error_number = ctypes.get_errno()
        raise OSError(error_number, os.strerror(error_number), destination)


def classify_artifact_error(error):
    if isinstance(error, FileExistsError):
        return "artifact_exists"
    if isinstance(error, FileNotFoundError):
        return "artifact_path_missing"
    if isinstance(error, PermissionError):
        return "artifact_permission_denied"
    return "artifact_failed"


def validate_library_name(value):
    if (
        not isinstance(value, str)
        or not value
        or value in {".", ".."}
        or "/" in value
        or "\x00" in value
    ):
        raise ValueError("Library name must be one non-empty path component")


def validate_relative_posix_path(value):
    if not isinstance(value, str) or not value:
        raise ValueError("target must be a non-empty string")
    path = PurePosixPath(value)
    if path.is_absolute() or path.as_posix() != value:
        raise ValueError(f"target must be a normalized relative path: {value!r}")
    parts = path.parts
    if not parts or any(part in {"", ".", ".."} or "\x00" in part for part in parts):
        raise ValueError(f"target contains an unsafe path component: {value!r}")
    if any(len(part.encode("utf-8")) > 255 for part in parts):
        raise ValueError(f"target component exceeds 255 UTF-8 bytes: {value!r}")
    if len(value.encode("utf-8")) > 4095:
        raise ValueError(f"target exceeds 4095 UTF-8 bytes: {value!r}")
    return parts


def normalized_absolute_path(value):
    if not isinstance(value, (str, os.PathLike)):
        raise ValueError("path must be a string")
    expanded = os.path.expanduser(os.fspath(value))
    absolute = os.path.abspath(expanded)
    if os.path.normpath(absolute) != absolute:
        raise ValueError(f"path must be lexically normalized: {value!r}")
    return Path(absolute)


def normalized_existing_file(value):
    path = normalized_absolute_path(value)
    status = path.lstat()
    if stat.S_ISLNK(status.st_mode) or not stat.S_ISREG(status.st_mode):
        raise ValueError(f"path must identify a local regular file: {path}")
    return os.fspath(path)


def plan_output_paths(batch, artifacts):
    paths = (normalized_absolute_path(batch), normalized_absolute_path(artifacts))
    if paths[0] == paths[1]:
        invalidate_plan_outputs(paths[:1])
        raise ValueError("batch and artifact outputs must use different paths")
    return paths


def invalidate_plan_outputs(paths):
    errors: list[Exception] = []
    for path in paths:
        try:
            status = path.lstat()
        except FileNotFoundError:
            continue
        except OSError as error:
            errors.append(error)
            continue
        if stat.S_ISDIR(status.st_mode):
            errors.append(ValueError(f"plan output path is a directory: {path}"))
            continue
        try:
            path.unlink()
        except OSError as error:
            errors.append(error)
    if errors:
        raise OSError(
            "cannot invalidate prior plan outputs: "
            + "; ".join(str(error) for error in errors)
        )


def reject_duplicate_object_keys(pairs):
    document = {}
    for key, value in pairs:
        if key in document:
            raise ValueError(f"duplicate JSON object field {key!r}")
        document[key] = value
    return document


def read_json_file(path):
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle, object_pairs_hook=reject_duplicate_object_keys)


def write_json_file(path_value, value):
    path = normalized_absolute_path(path_value)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(
        prefix=f".{path.name}.",
        suffix=".tmp",
        dir=path.parent,
        text=True,
    )
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(value, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except Exception:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def write_json(stream, value):
    json.dump(value, stream, ensure_ascii=False, indent=2)
    stream.write("\n")
    stream.flush()


def main():
    parser = build_parser()
    arguments = parser.parse_args()
    try:
        return arguments.handler(arguments)
    except (OSError, ValueError, RuntimeError) as error:
        write_json(
            sys.stdout,
            {
                "schema": "scrapefs.agent-error/v1",
                "code": "operation_failed",
                "message": str(error),
            },
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
