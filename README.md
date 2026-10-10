# Weixin–dot bridge

[中文说明](README.zh-CN.md)

Connect a personal WeChat account to an **existing OpenAI dot's native message room**. Messages continue in that same room when you switch between WeChat and the original dot interface.

```text
WeChat ↔ Tencent iLink ↔ weixin-dot ↔ existing dot message room
```

The bridge is an ordinary Go process that moves messages and original media bytes. It uses no additional model to route, rewrite, summarize, or transcribe messages. The dot continues to use its own tools and capabilities.

This is an experimental community project. The dot connection uses an unpublished ChatGPT protocol adapted from Beeper; it is not an official OpenAI integration or a supported public API. Access depends on your account and the two services' current behavior.

## What it handles

- WeChat text enters the existing dot room; replies, progress messages, and proactive messages are read from that room and queued for WeChat.
- User messages written in the original dot interface are mirrored with their speaker and original time. Unchanged messages that originated in WeChat are suppressed; original-interface edits, deletions, and returns to an earlier payload remain visible.
- Files, images, and videos use original-byte transfer with bounded memory and private temporary files. The bridge imposes no fixed total file-size limit; platform limits, available disk space, codecs, and network timeouts still apply.
- Incoming WeChat voice is passed to dot as an **audio file**. Dot can use its own tools to read it. This is not a native voice conversation interface, and outbound voice bubbles or automatic audio conversion are not implemented.
- A private journal preserves cursors, payloads, stable delivery identifiers, and outcomes across restarts. Unknown POST outcomes are retained rather than blindly sent again.

Native task cards and approvals require the original dot interface. Updates and deletions are displayed as notices; the bridge cannot retract an existing WeChat bubble. Original-interface user messages appear as labelled bot-forwarded messages, not personal-account outgoing bubbles. WeChat API acknowledgements do not prove that a person saw a message, and sending depends on the available reply context.

## Build

Use Go 1.26 or newer on a Unix host. The journal uses `flock`; the optional service template is for Linux with systemd.

```sh
git clone https://github.com/mjmkk/weixin-dot-bridge.git
cd weixin-dot-bridge
mkdir -p bin
go build -trimpath -o bin/weixin-dot ./cmd/weixin-dot
```

## Connect your accounts

You need an existing dot and your own supported, local ChatGPT/Codex credential file. The default source is `~/.codex/auth.json`; `--auth` can select another private file. The bridge consumes the existing login and does not provide a ChatGPT login flow. Keep credential files private (0600), with private state directories (0700).

1. Discover your dot without sending a message:

   ```sh
   ./bin/weixin-dot discover --auth "$HOME/.codex/auth.json"
   ```

   Keep the returned `room` and `dot` values locally for the next step. The bridge checks the authenticated account, the discovered dot, and the room members before relaying messages.

2. Bind WeChat:

   ```sh
   ./bin/weixin-dot bind
   ```

   Open the local QR image path reported by the command and scan it in WeChat. Successful binding saves credentials to `~/.config/weixin-dot/weixin.auth.json`. An interrupted pending login can be resumed with `bind --resume`; an expired QR requires a fresh login. Existing credentials are not overwritten automatically.

   If WeChat requests a verification code, this implementation accepts it only through a private stdin pipe. In Bash, resume the saved QR without putting the code in shell history:

   ```bash
   read -r -s -p "WeChat verification code: " weixin_verify_code
   printf '\n'
   printf '%s\n' "$weixin_verify_code" | ./bin/weixin-dot bind --resume
   unset weixin_verify_code
   ```

3. Start the bridge, replacing the two placeholders with your discovery results:

   ```sh
   ./bin/weixin-dot bridge \
     --auth "$HOME/.codex/auth.json" \
     --room 'YOUR_VERIFIED_ROOM' \
     --dot 'YOUR_VERIFIED_DOT'
   ```

   Send a new WeChat message to establish the reply context. Defaults are `~/.config/weixin-dot/weixin.auth.json` for WeChat credentials and `~/.local/state/weixin-dot/bridge.json` for the journal. Use `--weixin-auth` or `--state` to choose your own private locations.

Stop with Ctrl+C and restart with the **same journal**. One process owns a journal at a time. If account or room verification fails, reconnect the original account and review the failure before restarting; do not erase the journal to retry an unknown send or upload. Unknown uploads keep the original file because no reliable uploaded-file identity was received.

## History

Normal chat starts at its initial room baseline and reviews messages since that baseline. It does not automatically import the entire pre-connection conversation.

Queued historical messages remain paused by default. Explicitly add `--history-backfill` to the same `bridge` command to enable an existing history queue. History retains its original speaker and time, never writes back to dot, and never re-executes tasks. Live messages take priority; unresolved historical delivery is not bypassed through a new send identifier. Removing the flag pauses the queue without deleting it.

The `backfill inspect` and `backfill enqueue` commands accept a completed private `messages.jsonl` export with its matching manifest, profile, and room metadata. They verify the export before local enqueueing; this repository does not include a general history-export command. Run `./bin/weixin-dot backfill inspect -h` for the input flags.

## Optional Linux user service

Install the binary as `~/.local/bin/weixin-dot`. Copy [deploy/weixin-dot.service](deploy/weixin-dot.service) into `~/.config/systemd/user/`, replace `__VERIFIED_ROOM__` and `__VERIFIED_DOT__`, and adjust credential or state flags if using non-default paths.

```sh
systemctl --user daemon-reload
systemctl --user enable --now weixin-dot.service
```

The template uses private file permissions and retains the default journal. Optional host-specific network settings belong in your own `~/.config/weixin-dot/network.env`.

## Development and sources

```sh
go test -race ./...
go vet ./...
```

Tests use local fixtures and simulated transports. They do not log in to real accounts or send real messages. Keep credentials, QR images, message archives, journals, original media, and signed URLs outside the repository and out of bug reports.

See [dot client provenance](docs/dot-source.md), [WeChat protocol provenance](docs/weixin-source.md), and [third-party notices](THIRD_PARTY_NOTICES.md). This project is licensed under [GNU AGPL v3](LICENSE); third-party components retain their respective notices. The adapted native dot client is Copyright (c) 2026 Beeper.
