// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
)

type receipt struct {
	Status     string `json:"status"`
	Room       string `json:"room"`
	Dot        string `json:"dot"`
	Baseline   string `json:"baseline"`
	RequestID  string `json:"request_id"`
	MessageID  string `json:"message_id,omitempty"`
	TextSHA256 string `json:"text_sha256,omitempty"`
}

func emit(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }

func loadClient(source string) (*chatgpt.Client, error) {
	info, err := os.Stat(source)
	if err != nil {
		return nil, errors.New("authentication source unavailable")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("authentication source must be private (0600)")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return nil, errors.New("cannot read authentication source")
	}
	var auth struct {
		chatgpt.Credentials
		Tokens struct {
			Access string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil {
		return nil, errors.New("unsupported authentication source format")
	}
	if auth.AccessToken == "" {
		auth.AccessToken = auth.Tokens.Access
	}
	return chatgpt.New(auth.Credentials, nil)
}

func connect(ctx context.Context, source string) (*chatgpt.Client, chatgpt.Profile, *chatgpt.Room, error) {
	c, err := loadClient(source)
	if err != nil {
		return nil, chatgpt.Profile{}, nil, err
	}
	ps, err := c.Discover(ctx)
	if err != nil {
		return nil, chatgpt.Profile{}, nil, err
	}
	if len(ps) != 1 {
		return nil, chatgpt.Profile{}, nil, errors.New("expected one existing primary dot")
	}
	r, err := c.Verify(ctx, ps[0])
	return c, ps[0], r, err
}

func baseline(r *chatgpt.Room) string {
	var id string
	var latest time.Time
	for _, m := range r.Latest {
		if id == "" || m.Created.After(latest) {
			id = m.ID
			latest = m.Created
		}
	}
	return id
}

func save(path string, v any) error {
	d, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return saveBytes(path, d)
}

func saveBytes(path string, d []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if parent.Mode().Perm()&0077 != 0 {
		return errors.New("runtime state directory must be private (0700)")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(d); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func findRequest(ctx context.Context, c *chatgpt.Client, r receipt) (*chatgpt.Message, error) {
	if r.RequestID == "" || r.Room == "" {
		return nil, errors.New("reconciliation receipt lacks request or room identity")
	}
	after := r.Baseline
	seen := map[string]bool{}
	for !seen[after] {
		seen[after] = true
		p, err := c.Messages(ctx, r.Room, after)
		if err != nil {
			return nil, err
		}
		for _, m := range p.Items {
			if m.RequestID == r.RequestID {
				if m.ID == "" || r.TextSHA256 != "" && r.TextSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(m.Content.Text))) {
					return nil, errors.New("reconciled message payload identity mismatch")
				}
				return &m, nil
			}
		}
		if len(p.Items) == 0 || (len(p.Items) < 32 && p.Next == "") {
			return nil, nil
		}
		after = p.Items[len(p.Items)-1].ID
		if p.Next != "" {
			after = p.Next
		}
	}
	return nil, errors.New("message pagination did not advance")
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: weixin-dot discover|read|send|bind|bridge|stream|backfill")
	}
	if os.Args[1] == "bind" {
		return runBind(os.Args[2:])
	}
	if os.Args[1] == "stream" {
		return runStream(os.Args[2:])
	}
	if os.Args[1] == "bridge" {
		return runBridge(os.Args[2:])
	}
	if os.Args[1] == "backfill" {
		return runBackfill(os.Args[2:])
	}
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	source := fs.String("auth", filepath.Join(home, ".codex/auth.json"), "existing private ChatGPT/Codex credential file")
	room := fs.String("room", "", "expected native room (required for send)")
	dot := fs.String("dot", "", "expected dot (required for send)")
	after := fs.String("after", "", "native message cursor")
	text := fs.String("text", "", "message text")
	record := fs.String("receipt", "", "private send receipt path")
	include := fs.Bool("include-content", false, "include content in intentional read output")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c, p, r, err := connect(ctx, *source)
	if err != nil {
		return err
	}
	if *room != "" && p.Room != *room || *dot != "" && p.ID != *dot {
		return errors.New("expected dot/room mismatch; no message sent")
	}
	switch os.Args[1] {
	case "discover":
		emit(map[string]any{"status": "verified", "dot": p.ID, "room": p.Room, "thread": p.Thread, "name": p.Name, "baseline": baseline(r), "members": len(r.Members)})
		return nil
	case "read":
		page, err := c.Messages(ctx, p.Room, *after)
		if err != nil {
			return err
		}
		items := []map[string]any{}
		for _, m := range page.Items {
			v := map[string]any{"id": m.ID, "request_id": m.RequestID, "sender": m.Sender, "created": m.Created, "updated": m.Updated, "deleted": m.Deleted, "attachments": len(m.Content.Attachments)}
			if *include {
				v["text"] = m.Content.Text
				v["attachment_metadata"] = m.Content.Attachments
			}
			items = append(items, v)
		}
		emit(map[string]any{"room": p.Room, "items": items, "next": page.Next})
		return nil
	case "send":
		if *text == "" || *room == "" || *dot == "" || *record == "" {
			return errors.New("send requires --text, --room, --dot and --receipt")
		}
		if err = os.MkdirAll(filepath.Dir(*record), 0700); err != nil {
			return err
		}
		lock, err := os.OpenFile(*record+".lock", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return errors.New("another sender owns this receipt")
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(*text)))
		if data, err := os.ReadFile(*record); err == nil {
			var prior receipt
			if json.Unmarshal(data, &prior) != nil {
				return errors.New("invalid existing receipt")
			}
			if prior.RequestID == "" || prior.Status != "pending" && prior.Status != "acknowledged" || prior.Status == "acknowledged" && prior.MessageID == "" {
				return errors.New("existing receipt lacks a valid delivery identity")
			}
			if prior.Room != p.Room || prior.Dot != p.ID {
				return errors.New("receipt identity mismatch")
			}
			if prior.TextSHA256 != digest {
				return errors.New("receipt does not identify this message text; choose a new receipt for a new send")
			}
			if prior.Status == "acknowledged" {
				emit(prior)
				return nil
			}
			m, err := findRequest(ctx, c, prior)
			if err != nil {
				return err
			}
			if m != nil {
				prior.Status = "acknowledged"
				prior.MessageID = m.ID
				if err = save(*record, prior); err != nil {
					return err
				}
				emit(prior)
				return nil
			}
			return errors.New("prior send outcome unknown; no automatic repeat POST")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var b [16]byte
		if _, err = rand.Read(b[:]); err != nil {
			return err
		}
		b[6] = b[6]&15 | 64
		b[8] = b[8]&63 | 128
		h := hex.EncodeToString(b[:])
		id := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		rec := receipt{Status: "pending", Room: p.Room, Dot: p.ID, Baseline: baseline(r), RequestID: id, TextSHA256: digest}
		if err = save(*record, rec); err != nil {
			return err
		}
		m, err := c.Send(ctx, p.Room, *text, id, nil)
		if err != nil {
			return fmt.Errorf("send outcome unknown; receipt retained: %w", err)
		}
		rec.Status = "acknowledged"
		rec.MessageID = m.ID
		if err = save(*record, rec); err != nil {
			return err
		}
		emit(rec)
		return nil
	default:
		return errors.New("unknown command")
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
