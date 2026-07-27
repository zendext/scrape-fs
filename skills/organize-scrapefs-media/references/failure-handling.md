# Failure Handling

## Planning

- `status: manual`: stop and present each code, message, and path.
- `source_access_failed`: rerun only after the invoking user can read the path.
- `symlink_source`: choose the real local regular file explicitly; do not follow
  the link automatically.
- `probe_failed`: inspect the source and `ffprobe` output; do not generate a
  batch from filename evidence alone.
- `ambiguous_metadata`: obtain title, year, provider ID, or episode title from
  the user or an external provider and rerun with `--metadata`.
- Unsupported layout codes require a manual source-to-target batch or another
  media preparation tool outside this skill.

## Validation And Apply

Validation and apply errors are daemon authority:

- source missing, denied, unsafe, non-regular, or changed: repair or select the
  source, regenerate the batch if its path changes, and validate again;
- target conflict: inspect the namespace, choose a different deterministic
  target or undo/remove the conflicting item, then validate again;
- inactive Library or unavailable Profile: repair Library configuration before
  planning again;
- digest mismatch: present the fresh validation and request renewed explicit
  approval of that reviewed batch; retain its digest internally only after
  approval;
- permission denied: use the invoking user's configured control permissions;
  never escalate inside the skill.

Never apply after any failed validation. Never retry apply automatically.

## Artifacts

Artifact creation is intentionally separate from Projection Batch apply:

- `artifact_exists`: preserve the existing OverlayFile and ask whether it should
  be handled manually;
- `artifact_path_missing`: verify that apply succeeded and that the expected
  managed directory exists;
- `artifact_permission_denied`: correct ordinary FUSE permissions for the
  invoking user;
- another artifact failure: keep successful artifacts and retry only failed
  entries after resolving the cause.

A partial artifact failure does not roll back or invalidate the applied batch.
Batch undo retains all OverlayFiles, so report retained paths from the undo
result and let the user remove them through ordinary FUSE operations if needed.

## Replacement Cleanup

For an approved replacement or migration, remove only artifact paths listed in
the reviewed superseded manifest, then undo the explicitly identified old batch
after the replacement artifacts and downstream smoke test succeed. This cleanup
does not need another prompt.

Run `scrapefs gc` after unlinking old OverlayFiles. Unreferenced objects retained
inside the configured orphan grace period are expected deferred reclamation, not
a cleanup failure. Do not shorten or bypass the grace period. Any unexpected
path, missing reviewed artifact, undo failure, GC error, or still-referenced old
namespace entry stops automatic cleanup and must be reported.
