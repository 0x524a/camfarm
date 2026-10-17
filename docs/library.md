# Bring your own video

```sh
camfarm --media-dir /srv/clips --media-dir ./more-clips
```

A Docker volume or bind mount is just a directory, so inside a container point
`--media-dir` at the mount path.

- Files with a video extension (`.ts .mp4 .m4v .mov .mkv .avi .h264 .h265 .mpg`)
  are listed when they sit up to three levels below the media directory
  (`root/a/b/file.mp4` is listed, `root/a/b/c/file.mp4` is not). Symlinks and dot-files are skipped.
  The container is judged from content, so a `.mkv` is reported as an
  unsupported container rather than ignored. Only MPEG-TS and MP4/MOV carrying
  H.264 or H.265 can be served.
- Uploads through the dashboard (the Library tab's drop area) or the API
  (`POST /api/v1/library/upload`) go to an `uploads/` subdirectory of the first
  writable `--media-dir`, created on first use, capped by `--max-upload` (default 2GiB).
  An upload that fails validation is kept so its report stays visible; delete it with
  `DELETE /api/v1/library/files/{id}`. Only files inside `uploads/` can be deleted
  (anything you place there by hand included); your own files elsewhere in the
  directory are never removed by the API.
- `--allow-ui-paths` lets the dashboard API add directories by typed path
  (`POST /api/v1/library/roots`). It is off
  by default because the dashboard has no authentication and the option lets
  anyone who can reach it point the process at any directory it can read.
- A camera takes a library file with `{"id": "cam", "libraryId": "<id from GET
  /api/v1/library>"}`. The file is re-checked first, since it may have changed
  since the scan. Only this route validates: a camera created with a typed
  `source.path` skips the library entirely.
- If a media directory disappears, a rescan drops its files and the scan response
  lists it under `scanErrors`; the other directories keep working.

What the validator can and cannot see: it reports what camfarm's own parser
loads. B-frames are read from H.264 slice headers; for H.265 they are inferred
from timestamp reordering, because H.265 slice types are not parsed. It does not
repair or transcode anything.
