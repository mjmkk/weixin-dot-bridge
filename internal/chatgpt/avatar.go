package chatgpt

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (c *Client) Avatar(ctx context.Context, profile Profile) ([]byte, error) {
	var current struct {
		Profile
		URL string `json:"avatar_url"`
	}
	if err := c.do(ctx, "GET", "/tbo/by-thread/"+url.PathEscape(profile.Thread), nil, &current); err != nil {
		return nil, err
	}
	if current.ID != profile.ID || current.Room != profile.Room {
		return nil, errors.New("Dot identity changed while fetching avatar")
	}
	if current.URL == "" {
		return nil, nil
	}
	u, err := url.Parse(current.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !(strings.HasSuffix(u.Hostname(), ".oaiusercontent.com") || u.Hostname() == "cdn.openai.com" || strings.HasSuffix(u.Hostname(), ".oaistatic.com")) {
		return nil, errors.New("Dot avatar uses an unsupported storage origin")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid Dot avatar request")
	}
	req.Header.Set("User-Agent", "BeeperDotsBridge/0.1")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("Dot avatar download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("Dot avatar storage denied the download")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("Dot avatar exceeds the supported download size")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return nil, errors.New("Dot avatar must be a supported raster image no larger than 4096 pixels")
	}
	return data, nil
}
