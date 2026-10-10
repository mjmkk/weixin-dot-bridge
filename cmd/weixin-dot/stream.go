// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
)

func runStream(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("stream", flag.ContinueOnError)
	source := fs.String("auth", filepath.Join(home, ".codex/auth.json"), "existing private auth source")
	duration := fs.Duration("duration", 15*time.Second, "observation duration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	c, p, _, err := connect(ctx, *source)
	if err != nil {
		return err
	}
	subscribed := false
	err = c.StreamSignals(ctx, func(s chatgpt.Signal) error {
		if s.Type == "resync" {
			subscribed = true
		}
		emit(map[string]any{"event": s.Type, "room": s.Payload.Room, "expected_room": p.Room})
		return nil
	})
	if ctx.Err() != nil && subscribed {
		return nil
	}
	return err
}
