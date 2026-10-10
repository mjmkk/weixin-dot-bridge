package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

type ThreadStatus struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	TurnID    string `json:"turn_id,omitempty"`
	StartedAt int64  `json:"started_at,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

func (t *ThreadStatus) Terminal() bool {
	return t != nil && (t.Status == "completed" || t.Status == "failed" || t.Status == "interrupted")
}

func (c *Client) Thread(ctx context.Context, room string, message Message, threadID string) (*ThreadStatus, error) {
	c.mu.RLock()
	verified := c.rooms[room]
	c.mu.RUnlock()
	found := false
	for _, raw := range message.Content.Attachments {
		var ref struct {
			Type string `json:"type"`
			ID   string `json:"thread_id"`
		}
		if json.Unmarshal(raw, &ref) == nil && ref.Type == "thread" && ref.ID == threadID {
			found = true
		}
	}
	if !verified || message.Deleted != nil || message.ID == "" || threadID == "" || !found {
		return nil, errors.New("refusing task without a verified Dot room attachment")
	}
	var metadata struct {
		Thread struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			UpdatedAt int64  `json:"updatedAt"`
			Status    struct {
				Type        string            `json:"type"`
				ActiveFlags []json.RawMessage `json:"activeFlags"`
			} `json:"status"`
		} `json:"thread"`
	}
	const origin = "https://codex-cloud-backend.chatgpt.com"
	if err := c.doAt(ctx, origin, "GET", "/v1/threads/"+url.PathEscape(threadID), nil, &metadata); err != nil {
		return nil, err
	}
	if metadata.Thread.ID != threadID || metadata.Thread.Status.Type == "" {
		return nil, errors.New("ChatGPT task response has no matching identity or status")
	}
	var turns struct {
		Data []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			StartedAt int64  `json:"startedAt"`
		} `json:"data"`
	}
	if err := c.doAt(ctx, origin, "GET", "/v2/threads/"+url.PathEscape(threadID)+"/turns?limit=1&sortDirection=desc&itemsView=notLoaded", nil, &turns); err != nil {
		return nil, err
	}
	result := &ThreadStatus{ID: threadID, Title: strings.TrimSpace(metadata.Thread.Name), UpdatedAt: metadata.Thread.UpdatedAt, Status: "thinking"}
	if len(turns.Data) > 0 {
		result.TurnID = turns.Data[0].ID
		result.StartedAt = turns.Data[0].StartedAt
		result.Status = turns.Data[0].Status
	}
	switch metadata.Thread.Status.Type {
	case "systemError":
		result.Status = "failed"
	case "active":
		result.Status = "inProgress"
		if len(metadata.Thread.Status.ActiveFlags) > 0 {
			result.Status = "needsInput"
		}
	}
	switch result.Status {
	case "thinking", "inProgress", "needsInput", "completed", "failed", "interrupted":
		return result, nil
	default:
		return nil, errors.New("ChatGPT returned an unknown task turn state")
	}
}
