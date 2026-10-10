# WeChat iLink protocol provenance

`internal/weixin` re-expresses the necessary protocol from Tencent's [`openclaw-weixin`](https://github.com/Tencent/openclaw-weixin/tree/24de5c9eb0dd5e595d7e2d090ed8a3f82870d42c), version **2.4.9**, frozen at commit `24de5c9eb0dd5e595d7e2d090ed8a3f82870d42c`.

Copyright (C) 2026 Tencent. All rights reserved. The upstream project is MIT-licensed; its complete notice is retained in [licenses/Tencent-MIT.txt](../licenses/Tencent-MIT.txt).

The reference files are `src/api/api.ts`, `src/api/types.ts`, `src/auth/login-qr.ts`, the messaging and monitor modules, the CDN upload/URL/AES modules, media download code, and [`docs/protocol.md`](https://github.com/Tencent/openclaw-weixin/blob/24de5c9eb0dd5e595d7e2d090ed8a3f82870d42c/docs/protocol.md). This bridge does not require the OpenClaw runtime.

## Connection and messages

The standalone Go implementation provides QR creation and polling, native verification-code handoff, iLink long polling, and text/media sends. API and login redirect origins must be HTTPS hosts under `weixin.qq.com`; account headers do not follow arbitrary redirects. Binding is complete only when the token, bot identity, scanning user, and trusted API base are available.

`GetUpdates` preserves unknown message fields and numeric identifier precision. Sending retains the original reply context token and the caller's stable `client_id`. The bridge saves incoming messages before advancing its receive cursor and records every POST attempt before sending. The core does not blindly retry uncertain sends. An API ACK is recorded as an acknowledgement, not a claim of human receipt.

The frozen protocol and field schemas do not establish fixed server quotas, context-token lifetimes, or permission to impersonate a personal WeChat account. Mirrored original-interface Human messages are sent by the bound bot and labelled accordingly.

## Media adaptations

The implementation retains the upstream AES-128-ECB/PKCS#7 wire format and message fields while replacing whole-file buffers with bounded copy buffers and private temporary files. It imposes no fixed total file-size limit.

Before upload, it computes original MD5/SHA256, encrypts an immutable snapshot, and checks that snapshot before a single CDN POST. Upload URLs, AES keys, and download capabilities belong only in the private journal. CDN requests do not carry bot account headers and do not follow redirects. Missing or uncertain upload receipts retain the source and plan instead of automatically creating another upload.

Downloads acquire the full referenced original rather than a thumbnail, then decrypt and verify padding, transfer lengths, and available original size/MD5. Interrupted GETs can resume only with exact range validation and sufficient identity evidence. Partial output is removed on failure; cancellation and no-progress paths terminate cleanly. Range windows are transfer chunks, not total file-size limits.

Incoming voice uses `voice_item.media` and retains codec/sample-rate/duration metadata. Recognizable byte headers determine the actual format; a declared codec is not treated as proof of the container. Audio is passed to the existing dot as a file without replacing it with supplied transcription text or calling a transcription model. Outbound native voice and automatic transcoding are not implemented.

The caller manages 0700 media directories and 0600 files. Completed originals and encrypted snapshots are cleaned up only after the corresponding acknowledged handoff; material needed for unknown outcomes is retained. Simulated-transport tests cover byte integrity, interrupted reads, cleanup, URL validation, and result classification, without authenticating real accounts or sending messages.
