---
name: organize-scrapefs-media
description: Inspect local movie and standard SxxEyy episode sources, select missing episodes from ordered fallback sources, exclude samples, read NFO metadata, probe media with ffprobe, generate deterministic Plex or Jellyfin ScrapeFS Projection Batches for shows, anime, or variety roots, obtain explicit approval of reviewed batches, apply or undo batches with digest consistency checks, and create NFO or artwork OverlayFiles through FUSE. Use when organizing local media into an existing ScrapeFS Library or diagnosing a media source that the v1 workflow may require manual handling.
---

# Organize ScrapeFS Media

Use the bundled tool for deterministic filesystem work. Keep metadata-provider
selection and any model interaction in the current Agent session.

## Prerequisites

Verify all of the following before planning:

- Linux with Python 3.9 or newer;
- `ffprobe` on `PATH`;
- `scrapefs` on `PATH`;
- a ready `scrapefsd`;
- an ACTIVE `jellyfin@1` or `plex@1` Library;
- invoking-user read permission on every source;
- invoking-user control permission and FUSE write permission.

Never use `sudo` or another identity to enumerate or probe sources. Do not store
provider credentials, prompts, sessions, or pending plans in the daemon.

Set `SKILL_DIR` to the directory containing this file:

```bash
TOOL="$SKILL_DIR/scripts/scrapefs_media.py"
```

Read [media-contract.md](references/media-contract.md) when deciding whether a
layout is supported or when supplying Agent-resolved metadata. Read
[failure-handling.md](references/failure-handling.md) before recovering from a
manual result, CLI error, source change, conflict, or partial artifact failure.

## Preflight

Before planning, verify the exact source roots and FUSE mount visible to the
invoking user:

```bash
python3 "$TOOL" preflight \
  --source /srv/downloads/Dune.2021 \
  --source-root /srv/downloads \
  --library media \
  --mountpoint /mnt/scrapefs
```

Repeat `--source-root` for every explicitly allowed local media root and repeat
`--fallback-source` for each source that will be passed to `plan`. In a
container, every source path must be mounted at the same absolute path used by
the host daemon.

Preflight verifies daemon readiness, the ACTIVE Library and Profile, source
containment and invoking-user readability, the daemon mountpoint path, and
ordinary FUSE Library write access. It returns
`scrapefs.agent-preflight/v1` with `status: ready` or `status: manual`. It never
changes permissions, mounts filesystems, or creates a probe artifact.

If preflight returns `manual`, stop and present every item before planning.

## Plan

First inspect the Library and use its actual Profile:

```bash
scrapefs library list
```

Generate a local batch and artifact manifest:

```bash
python3 "$TOOL" plan \
  --source /srv/downloads/Dune.2021 \
  --library media \
  --profile jellyfin \
  --batch /tmp/dune.batch.json \
  --artifacts /tmp/dune.artifacts.json
```

For an episode source that belongs in a separate anime media library, select
the anime series root explicitly:

```bash
python3 "$TOOL" plan \
  --source /srv/downloads/anime/Oshi.no.Ko.2023 \
  --library jellyfin \
  --profile jellyfin \
  --series-root anime \
  --batch /tmp/oshi-no-ko.batch.json \
  --artifacts /tmp/oshi-no-ko.artifacts.json
```

For complementary episode sources, inspect their actual resolution first, pass
the preferred source first, and add lower-priority sources in fallback order.
For example, a 4K variety source with 1080p gap filling:

```bash
python3 "$TOOL" plan \
  --source /srv/downloads/variety/show.2160p \
  --fallback-source /srv/downloads/variety/show.1080p \
  --library jellyfin \
  --profile jellyfin \
  --series-root variety \
  --batch /tmp/variety.batch.json \
  --artifacts /tmp/variety.artifacts.json
```

The first source claiming an `SxxEyy` wins. A fallback contributes only episode
keys missing from all earlier sources.

The command only reads local sources and writes local JSON files. It never calls
the control CLI, mounts filesystems, queries a metadata provider, or applies a
batch.

If the result status is `manual`, stop. Present every `manual` item and obtain
corrected metadata or a user decision. Do not silently omit unsupported video
content. For resolved ambiguity, write a strict metadata JSON file and rerun:

```bash
python3 "$TOOL" plan \
  --source /srv/downloads/unidentified.mkv \
  --library media \
  --profile plex \
  --metadata /tmp/resolved-metadata.json \
  --batch /tmp/media.batch.json \
  --artifacts /tmp/media.artifacts.json
```

Review the generated batch and artifact manifest before validation.

## Validate

Run stateless validation:

```bash
python3 "$TOOL" validate --batch /tmp/media.batch.json
```

Present the complete validation JSON, including every entry, warning,
`namespace_version`, and `digest`.

Stop and ask the user to explicitly approve or reject the reviewed batch in
natural language. Require an unambiguous apply instruction after presenting the
validation; do not infer approval from the initial request, planning decisions,
silence, or unrelated replies. The user does not need to transcribe the digest.
Retain the exact digest returned by validation as an internal consistency token.

`plan` and `validate` never apply, including in non-TTY use. The tool contains no
combined plan/validate/apply command and never prompts.

## Apply

Only after the user explicitly approves the reviewed validation, pass the
retained digest unchanged:

```bash
VALIDATED_DIGEST=sha256:<digest-from-approved-validation>
python3 "$TOOL" apply \
  --batch /tmp/media.batch.json \
  --confirm-digest "$VALIDATED_DIGEST"
```

The apply command:

1. runs a fresh `projection batch validate`;
2. compares its digest byte-for-byte with the retained approved digest;
3. stops before apply if they differ;
4. calls `projection batch apply` with that exact expected digest;
5. returns both fresh validation and apply result JSON.

Never retry a mismatch or CLI failure automatically. Present the new validation
or stable daemon error and return to the validation step. Replace the retained
digest only after the user reviews and explicitly approves the new validation.

## Create Artifacts

Batch approval covers only the Projection Batch. Present the artifact manifest
separately before writing it.

After a successful apply, create NFO and selected poster files through the
mounted filesystem:

```bash
python3 "$TOOL" artifacts \
  --manifest /tmp/media.artifacts.json \
  --mountpoint /mnt/scrapefs
```

The tool opens the mounted path with the invoking user's permissions, writes a
temporary OverlayFile, fsyncs it, and renames with no-replace semantics. It does
not overwrite an existing artifact. Treat each artifact result independently;
do not undo a successful Projection Batch because one optional artifact failed.

## Replacement Cleanup

When an approved replacement or migration explicitly identifies the superseded
batch and artifact manifest, finish cleanup without a second confirmation after:

1. the replacement batch applies successfully;
2. every required replacement artifact is created;
3. the downstream consumer smoke test passes.

Delete only the old manifest's exact artifact targets, undo the identified old
batch, refresh the downstream consumer, and run `scrapefs gc`. Do not use a
recursive filesystem deletion. If GC retains objects inside the configured
orphan grace period, report deferred reclamation and leave the grace period
unchanged.

## Inspect Or Undo

Inspect provenance after apply:

```bash
python3 "$TOOL" show batch-42
```

Standalone undo still requires an explicit user request:

```bash
python3 "$TOOL" undo batch-42
```

Explain that undo removes remaining batch-owned Projections but retains
OverlayFiles, including NFO and artwork created after apply.
