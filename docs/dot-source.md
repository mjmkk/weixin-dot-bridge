# Native dot client provenance

`internal/chatgpt` is adapted from Beeper's [`chatgpt-dots/pkg/chatgpt`](https://github.com/beeper/chatgpt-dots/tree/ff12c9acb70507175a8fa2b6bb65b91c3d9bbfba/pkg/chatgpt), frozen at commit `ff12c9acb70507175a8fa2b6bb65b91c3d9bbfba`.

Copyright (c) 2026 Beeper. The upstream project is licensed under GNU AGPL v3. The original license text is retained in [LICENSE](../LICENSE), and attribution is retained in [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md).

This client was modified for `weixin-dot-bridge`; this version's adaptations are dated 2026-10-10 and described below.

## Connection scope

The client discovers an existing primary dot and verifies its profile, native DM room, and dot member. Bridge startup and periodic verification additionally require the authenticated user to be the sole Human member. Dot identity, associated thread, and messaging room are distinct objects.

The protocol is unpublished and experimental. These paths describe the implementation, not an official compatibility or quota contract:

| Purpose | Native request |
| --- | --- |
| Discover primary dot | `GET /backend-api/tbo/primary` |
| Verify dot profile | `GET /backend-api/tbo/by-thread/{thread_id}` |
| Verify room | `GET /backend-api/messaging/rooms/{room_id}` |
| Read/send messages | `GET / POST /backend-api/messaging/rooms/{room_id}/messages` |
| Upload an attachment | `POST /backend-api/messaging/rooms/{room_id}/files` |
| Get attachment metadata | `GET /backend-api/messaging/rooms/{room_id}/files/{file_id}` |
| Obtain activity WebSocket | `GET /backend-api/celsius/ws/user` |

The bridge uses REST polling and periodic review for normal delivery. The client also provides native activity subscriptions; its WebSocket validation accepts numeric server-selected shards only on the trusted host and for the same authenticated user.

## Material adaptations

- The native client is isolated from Beeper's Matrix connector and used by a direct WeChat adapter.
- Upstream's local `20 << 20` upload and download checks are removed. This changes a client-side check, not a service quota.
- `UploadFile` streams a regular file from one opened descriptor, retaining only the multipart envelope in memory. The UTF-8 filename uses the quoted `filename` form with header-safe escaping.
- `DownloadAttachmentTo` copies original bytes to a caller-owned writer with bounded buffers. The bridge uses private temporary files and checks the completed local result. Legacy byte-slice APIs remain available; the legacy download buffers the whole file.
- Media uses a separate HTTP client without an overall transfer deadline inherited from ordinary API calls. Context cancellation and transport protections remain. Authentication locks are released during long byte transfers.
- Unknown uploads return a typed outcome and are not retried automatically. Known HTTP rejections retain safe status/reason metadata; arbitrary response bodies are not logged or journaled.
- The bridge journals sends before POST, reconciles unknown native message sends by their recorded request identity, and mirrors original-interface Human revisions while suppressing unchanged WeChat-origin inputs.

## Media and retry boundary

`UploadFile(ctx, room, filename, contentType, localPath)` returns a reliable `fileID` only after a valid successful response. Keep the source bytes unchanged until it returns. Transport failure, in-flight cancellation, HTTP 5xx, and malformed successful responses can mean that the file was stored without a usable receipt; preserve the original and unknown record. No verified attachment-upload idempotency or reconciliation API is assumed. An explicit HTTP 401 may trigger one credential refresh and replay from the same file descriptor.

`DownloadAttachmentTo(ctx, room, fileID, dst)` returns file metadata and the actual copied size. The caller owns the destination and must discard partial output on error. Downloads require a verified room and matching file identity, use HTTPS storage below `oaiusercontent.com`, refuse redirects, and do not send account credentials to storage.

The bridge forwards incoming voice as an ordinary audio file without transcription or conversion. Reading its contents is work performed by the existing dot and its own tools. Supported codecs, service acceptance, native interactions, and eventual message display remain separate concerns.

Local fake-transport tests verify streaming, cleanup, cancellation, identity checks, MIME/header handling, and unknown outcomes. They do not establish a server file-size limit, account access, or universal format support.
