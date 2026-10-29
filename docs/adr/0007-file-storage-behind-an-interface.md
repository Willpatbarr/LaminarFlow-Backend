# 7. File storage: bytes behind an interface, on a local folder first

Status: Accepted — 2026-10-29 (LAM-90)

## Context

Tickets and documents get file attachments and images (LAM-116). The app is one container on
a home server (ADR 2) and may move to a cloud VM later, where object storage is the natural
home for user files. Postgres could hold the bytes, but that makes every backup and every
`pg_dump` carry them and puts large objects through the connection pool.

## Decision

**Metadata in Postgres, bytes behind `storage.Storage`.** The `attachment` table holds
everything a query needs. The bytes are reached through three methods — `Put`, `Open`,
`Delete` — keyed by an opaque server-generated uuid. The only implementation today is a
folder (`ATTACHMENT_DIR`); an S3-style backend is a second file implementing the same
interface and a config switch in `main.go`, with no change to the API or the table.

**No user-controlled string is a path.** The key is a uuid; the client's filename is
sanitised and stored as display text only. The local backend additionally refuses any key
that is not a plain token.

**Rows first, bytes second, and the row wins.** A create writes the blob, then the row, and
removes the blob if the row does not land. A delete removes the row, then the blob, and
succeeds if the blob was already gone.

**The type is the server's call.** Decided from the file's first bytes against a closed
allowlist; HTML, SVG and XML are not storable, so nothing uploaded can be served as a page
from the app's origin. Raster images are served inline so Markdown can embed them; everything
else downloads.

## Consequences

* The files folder is state and must be on a volume and in the backup, alongside the
  database. A restored database without its files leaves rows whose download is a 404.
* Deleting a ticket or document outright cascades the rows and leaves the blobs. The API
  only archives, so this takes hand-run SQL; a sweep for unreferenced keys is the fix if it
  ever matters.
* The upload is streamed: the size limit stops the read, and memory use does not scale with
  file size. This is why the upload operation does not declare its body to the HTTP framework
  — see `internal/api/attachment.go`.
