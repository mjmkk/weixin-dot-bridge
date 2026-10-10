package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (c *Client) SelfAvatar(ctx context.Context, profile Profile) ([]byte, error) {
	c.mu.RLock()
	verified := c.rooms[profile.Room]
	c.mu.RUnlock()
	if !verified {
		return nil, errors.New("refusing own avatar lookup in an unverified Dot room")
	}
	var room struct {
		Room
		Members []struct {
			ID     string          `json:"account_user_id"`
			Aeon   string          `json:"aeon_id"`
			Avatar json.RawMessage `json:"avatar_url"`
		} `json:"members"`
	}
	if err := c.do(ctx, "GET", "/messaging/rooms/"+url.PathEscape(profile.Room), nil, &room); err != nil {
		return nil, err
	}
	if room.Type != "DM" || room.Source != "chatgpt:messaging" || room.ID != profile.Room || room.Aeon != profile.ID {
		return nil, errors.New("Dot room identity changed during own avatar lookup")
	}
	var avatarURL string
	selfCount := 0
	dotFound := false
	for _, member := range room.Members {
		if member.Aeon == profile.ID && member.ID != "" {
			dotFound = true
		}
		if member.ID == c.Identity.User && member.Aeon == "" {
			selfCount++
			if len(member.Avatar) == 0 || json.Unmarshal(member.Avatar, &avatarURL) != nil {
				return nil, errors.New("own avatar field missing or invalid in Dot room")
			}
		}
	}
	if selfCount != 1 || !dotFound {
		return nil, errors.New("Dot room does not identify the original account and Dot")
	}
	if avatarURL == "" {
		return nil, nil
	}
	u, err := url.Parse(avatarURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !(strings.HasSuffix(u.Hostname(), ".oaiusercontent.com") || u.Hostname() == "cdn.openai.com" || strings.HasSuffix(u.Hostname(), ".oaistatic.com") || u.Hostname() == "cdn.auth0.com") {
		return nil, errors.New("own avatar uses an unsupported storage origin")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid own avatar request")
	}
	req.Header.Set("User-Agent", "BeeperDotsBridge/0.1")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("own avatar download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("own avatar storage denied the download")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("own avatar exceeds the supported download size")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return nil, errors.New("own avatar must be a supported raster image no larger than 4096 pixels")
	}
	return data, nil
}
