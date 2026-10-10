package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type ReadReceipt struct {
	Member  string    `json:"account_user_id"`
	ReadAt  time.Time `json:"last_read_at"`
	Updated time.Time `json:"read_cursor_updated_at"`
}

type Signal struct {
	Type    string `json:"type"`
	Payload struct {
		Room      string  `json:"room_id"`
		Typing    *bool   `json:"typing"`
		Timestamp float64 `json:"timestamp"`
		Source    struct {
			Role   string `json:"role"`
			Member string `json:"account_user_id"`
		} `json:"source"`
		Message struct {
			Member string `json:"account_user_id"`
		} `json:"message"`
	} `json:"payload"`
}

func (c *Client) RoomSignals(ctx context.Context, p Profile) (*Room, error) {
	c.mu.RLock()
	verified := c.rooms[p.Room]
	c.mu.RUnlock()
	if !verified {
		return nil, errors.New("refusing unverified Dot room")
	}
	var room Room
	if err := c.do(ctx, "GET", "/messaging/rooms/"+url.PathEscape(p.Room), nil, &room); err != nil {
		return nil, err
	}
	if room.ID != p.Room || room.Aeon != p.ID || room.Type != "DM" || room.Source != "chatgpt:messaging" {
		return nil, errors.New("Dot room signal identity changed")
	}
	return &room, nil
}

func keepAlive(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := conn.Ping(pingCtx)
		cancel()
		if err != nil {
			conn.CloseNow()
			return
		}
	}
}

func (c *Client) StreamSignals(ctx context.Context, handle func(Signal) error) error {
	var endpoint struct {
		URL string `json:"websocket_url"`
	}
	if err := c.do(ctx, "GET", "/celsius/ws/user", nil, &endpoint); err != nil {
		return err
	}
	if !validSocketURL(endpoint.URL, c.Identity.User) {
		return errors.New("unexpected ChatGPT messaging socket origin")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(dialCtx, endpoint.URL, &websocket.DialOptions{HTTPClient: &http.Client{Transport: c.http.Transport, CheckRedirect: c.http.CheckRedirect}})
	cancel()
	if err != nil {
		return errors.New("ChatGPT messaging socket connection failed")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)
	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go keepAlive(pingCtx, conn)
	commands := `[{"id":1,"command":{"type":"connect","presence":{"type":"presence","state":"background"}}},{"id":2,"command":{"type":"subscribe","topic_id":"calpico-chatgpt-messaging"}}]`
	writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = conn.Write(writeCtx, websocket.MessageText, []byte(commands))
	cancel()
	if err != nil {
		return errors.New("ChatGPT messaging subscription failed")
	}
	subscribed := false
	for ctx.Err() == nil {
		readCtx := ctx
		cancel := func() {}
		if !subscribed {
			readCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
		}
		_, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return errors.New("ChatGPT messaging stream disconnected")
		}
		var frames []struct {
			ID      int             `json:"id"`
			Type    string          `json:"type"`
			Topic   string          `json:"topic_id"`
			Payload json.RawMessage `json:"payload"`
			Reply   struct {
				Type  string `json:"type"`
				Topic string `json:"topic_id"`
			} `json:"reply"`
		}
		if json.Unmarshal(data, &frames) != nil {
			continue
		}
		for _, frame := range frames {
			if frame.ID == 2 {
				if frame.Reply.Type != "subscribe" || frame.Reply.Topic != "calpico-chatgpt-messaging" {
					return errors.New("ChatGPT messaging subscription rejected")
				}
				subscribed = true
				if err := handle(Signal{Type: "resync"}); err != nil {
					return err
				}
				continue
			}
			if !subscribed || frame.Type != "message" || frame.Topic != "calpico-chatgpt-messaging" {
				continue
			}
			var signal Signal
			if json.Unmarshal(frame.Payload, &signal) != nil {
				continue
			}
			c.mu.RLock()
			verified := c.rooms[signal.Payload.Room]
			c.mu.RUnlock()
			if verified {
				if err := handle(signal); err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
}

// The authenticated discovery endpoint currently selects a numbered shard
// (observed /p16/). Keep the exact origin and account user boundary while
// accepting its numeric shard instead of freezing the upstream /p0/ value.
func validSocketURL(raw, user string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" || u.Host != "ws.chatgpt.com" || u.User != nil || u.Fragment != "" || user == "" {
		return false
	}
	path, ok := strings.CutPrefix(u.Path, "/p")
	if !ok {
		return false
	}
	shard, suffix, ok := strings.Cut(path, "/ws/user/")
	if !ok || suffix != user || shard == "" {
		return false
	}
	for _, r := range shard {
		if r < '0' || r > '9' {
			return false
		}
	}
	_, err = strconv.ParseUint(shard, 10, 64)
	return err == nil
}
