# Media Contract

## Supported Inputs

The v1 planner accepts local regular files and directories containing:

- one movie file;
- one movie directory with one non-sample movie file;
- one or more separately stored episodes using one `SxxEyy` token per file;
- ordinary external subtitles next to their video;
- Kodi-compatible `movie.nfo`, `tvshow.nfo`, and episode-stem NFO files;
- one local `poster`, `folder`, `cover`, or `movie` artwork candidate.

Video extensions:

```text
avi m2ts m4v mkv mov mp4 mpeg mpg ts webm
```

Subtitle extensions:

```text
ass smi srt ssa sub vtt
```

The planner runs `ffprobe` as the invoking user for every selected video and
requires at least one video stream. It does not read through daemon APIs or use
another identity.

## Sample And Sidecar Rules

Ignore a file when `sample` is a complete case-insensitive filename token or a
path component is `sample` or `samples`.

Associate a subtitle only when it is in the same directory and its name equals:

```text
<video-stem><subtitle-extension>
<video-stem>.<language-or-flags><subtitle-extension>
```

Preserve the complete language/flag suffix in the generated target. Report an
unmatched subtitle as a warning instead of guessing.

Do not place source NFO or artwork in the Projection Batch. Add them to the
post-apply artifact manifest so they are created as OverlayFiles through FUSE.
When no NFO exists, generate a minimal NFO from resolved metadata.

## Metadata Resolution

Use this priority:

1. explicit Agent-resolved metadata JSON;
2. existing local NFO;
3. deterministic filename inference.

The optional metadata file is a strict JSON object:

```json
{
  "kind": "movie",
  "title": "Dune: Part One",
  "year": 2021,
  "provider_ids": {
    "tmdb": "438631"
  }
}
```

For episodes:

```json
{
  "kind": "episodes",
  "show_title": "The Expanse",
  "show_year": 2015,
  "provider_ids": {
    "tvdb": "280619"
  },
  "episodes": {
    "S01E01": {
      "title": "Dulcinea"
    }
  }
}
```

Accepted provider IDs are numeric TMDB/TVDB IDs and IMDb IDs in `tt<digits>`
form. The planner does not contact those providers. The Agent may use an
external provider only with the user's normal permissions and must put the
resolved values in this file.

For existing NFO files, valid supported IDs remain strict. Unknown,
lower-priority provider IDs such as Wikidata are reported as warnings and
ignored for deterministic naming instead of invalidating a higher-priority
TMDB, TVDB, or IMDb identity. Movie target hints prefer TMDB and then IMDb;
show target hints prefer TVDB, TMDB, and then IMDb.

## Deterministic Naming

Use one movie folder for both Profiles.

Jellyfin movie:

```text
Movies/Title (Year) [tmdbid-ID]/Title (Year) [tmdbid-ID].mkv
```

Plex movie:

```text
Movies/Title (Year) {tmdb-ID}/Title (Year) {tmdb-ID}.mkv
```

Jellyfin episode:

```text
Shows/Show (Year) [tvdbid-ID]/Season 01/Show (Year) S01E01 Episode Title.mkv
```

Plex episode:

```text
TV Shows/Show (Year) {tvdb-ID}/Season 01/Show (Year) - S01E01 - Episode Title.mkv
```

Provider hints and years are omitted when unavailable. For movies prefer TMDB,
then IMDb. For shows prefer TVDB, then TMDB, then IMDb. Preserve the source
extension in lowercase. Use the corresponding media basename for external
subtitles.

Replace path separators and control characters in titles. For Jellyfin also
replace `<`, `>`, `:`, `"`, `|`, `?`, and `*`. Return manual handling instead
of truncating an overlong generated component.

## Manual Handling

Return `status: manual` and write no batch when the source contains:

- BDMV or VIDEO_TS;
- anime or another TV layout without standard `SxxEyy`;
- one file containing multiple episodes;
- multiple files claiming the same `SxxEyy`;
- multiple non-episode video candidates;
- automatic edition markers;
- movie or TV extras;
- music or image-only content;
- remote URLs;
- symbolic links or non-regular filesystem objects;
- inaccessible directories or files;
- invalid NFO or metadata;
- unavailable, failed, or non-video `ffprobe` output.

Do not silently drop unsupported non-sample videos.
