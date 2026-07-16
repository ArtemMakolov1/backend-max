# Browser API contract

`openapi.yaml` is the canonical contract shared by the Go API and the React/Astro
frontend. It describes the public `/api/v1` routes used by the browser; internal
compatibility aliases are intentionally excluded.

The backend test verifies that every canonical browser route and method remains
present. The frontend generates TypeScript declarations from this file with:

```bash
npm run generate:api
```

`npm run check:api` is a CI gate: it regenerates the declarations and fails when
the committed TypeScript contract is stale. When both repositories change in one
release, publish the backend contract first and then the frontend consumer.

Do not edit `frontend/src/lib/generated/api-contract.d.ts` by hand.

## Ordered post attachments

The browser API treats media as an ordered `attachments` array embedded in each
post. The legacy `image_url` field remains a compatibility projection of the
first image and must not be used to implement galleries.

Supported attachment types are `image` and `video`:

| Type | Accepted files | Maximum size |
| --- | --- | ---: |
| `image` | PNG, JPEG, GIF | 50 MiB |
| `video` | MP4, MOV, MKV, WebM | 250 MiB |

An image is decoded before storage and may be at most 7680 pixels on either
edge. Video container signatures are checked against the filename extension.
The server rejects empty, oversized, malformed and mismatched files before they
become post attachments.

A post may contain at most 12 attachments, or 11 when it also has link buttons.
The limit applies to images, videos and mixed media together.

### Routes

All routes require the normal session cookie and are tenant-scoped:

| Method | Route | Purpose | Success response |
| --- | --- | --- | --- |
| `POST` | `/api/v1/posts/{id}/attachments` | Upload and append/insert one file | `201` + `{attachment, post}` |
| `PUT` | `/api/v1/posts/{id}/attachments/{attachment_id}` | Replace the file, preserving attachment ID and position | `200` + `{attachment, post}` |
| `PATCH` | `/api/v1/posts/{id}/attachments/order` | Replace the complete order | `200` + updated post |
| `DELETE` | `/api/v1/posts/{id}/attachments/{attachment_id}` | Remove one item and compact positions | `200` + updated post |

Upload and replacement use `multipart/form-data` with a required `file` field.
Optional `type=image|video` overrides filename inference. Create also accepts a
zero-based `position`; omitting it appends the item. Reorder accepts the complete
set of IDs:

```json
{
  "attachment_ids": [31, 29, 30]
}
```

The order request must contain every current attachment exactly once. Partial,
duplicate, foreign or stale ID sets are rejected rather than silently losing
media.

Upload and replacement return both the affected `attachment` and the latest
complete `post`. The client must merge this post revision instead of assuming
that only the gallery changed.

`PostAttachment` objects expose `id`, `type`, `position`, authenticated `url`,
`processing_status`, byte size, MIME type and available media metadata.
Private `storage_key`, MAX provider upload tokens and provider metadata are
never serialized to the browser. The object in S3 remains the source of truth;
the backend may cache an opaque MAX upload token for subsequent edits.

Documents and audio are intentionally outside this contract. Add them only
after product demand is confirmed and the size, processing and MAX-editing
semantics have their own explicit limits.
