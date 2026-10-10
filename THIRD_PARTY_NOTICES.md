# Third-party notices

The combined bridge is distributed under [GNU AGPL v3](LICENSE). The following upstream notices and component licenses remain applicable.

This modified version was prepared for `weixin-dot-bridge` on 2026-10-10. The upstream adaptations are described below and in the linked provenance documents.

## Beeper chatgpt-dots

Copyright (c) 2026 Beeper.

- Source: [beeper/chatgpt-dots](https://github.com/beeper/chatgpt-dots/tree/ff12c9acb70507175a8fa2b6bb65b91c3d9bbfba).
- Frozen commit: `ff12c9acb70507175a8fa2b6bb65b91c3d9bbfba`.
- License: GNU Affero General Public License, version 3; complete text in [LICENSE](LICENSE).
- Adapted component: `pkg/chatgpt` → `internal/chatgpt`.
- Material changes: independent native client integration, streamed media with removal of local fixed size checks, multipart filename handling, cancellation/cleanup, safe upload rejection classification, unknown outcome preservation, and trusted same-user WebSocket shard validation. The WeChat adapter and durable delivery journal are separate bridge code. See [dot-source.md](docs/dot-source.md).

Copyright attribution comes from the [frozen upstream README](https://github.com/beeper/chatgpt-dots/blob/ff12c9acb70507175a8fa2b6bb65b91c3d9bbfba/README.md).

## Tencent openclaw-weixin

Copyright (C) 2026 Tencent. All rights reserved.

- Source: [Tencent/openclaw-weixin](https://github.com/Tencent/openclaw-weixin/tree/24de5c9eb0dd5e595d7e2d090ed8a3f82870d42c).
- Frozen version: 2.4.9, commit `24de5c9eb0dd5e595d7e2d090ed8a3f82870d42c`.
- License: MIT; the complete original notice is preserved in [licenses/Tencent-MIT.txt](licenses/Tencent-MIT.txt).
- Protocol implementation: `internal/weixin`. Necessary QR, message, CDN, and media behavior is re-expressed in Go with streaming and durable caller-managed result handling. See [weixin-source.md](docs/weixin-source.md).

## coder/websocket

Source: [github.com/coder/websocket](https://github.com/coder/websocket/tree/v1.8.15), version `v1.8.15`. License: ISC. The following notice is reproduced from that version's `LICENSE.txt`:

```text
Copyright (c) 2025 Coder

Permission to use, copy, modify, and distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
```

## skip2/go-qrcode

Source: [github.com/skip2/go-qrcode](https://github.com/skip2/go-qrcode/tree/da1b6568686e), version `v0.0.0-20200617195104-da1b6568686e`. License: MIT. The following notice is reproduced from that version's `LICENSE`:

```text
Copyright (c) 2014 Tom Harwood

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```
