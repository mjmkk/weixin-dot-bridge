package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Credentials struct {
	AccessToken  string `json:"access_token"`
	SessionToken string `json:"session_token,omitempty"`
}
type Identity struct {
	Account, User string
	Email         string
	Expires       time.Time
}

func (c Credentials) Identity() (Identity, error) {
	var out Identity
	parts := strings.Split(c.AccessToken, ".")
	if len(parts) != 3 {
		return out, errors.New("invalid access token; reconnect ChatGPT")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return out, errors.New("invalid access token encoding")
	}
	var claims struct {
		Exp  int64 `json:"exp"`
		Auth struct {
			Account string `json:"chatgpt_account_id"`
			User    string `json:"chatgpt_user_id"`
		} `json:"https://api.openai.com/auth"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
	}
	if json.Unmarshal(data, &claims) != nil || claims.Auth.Account == "" || claims.Auth.User == "" || claims.Exp == 0 {
		return out, errors.New("access token has no supported ChatGPT account identity")
	}
	out = Identity{Account: claims.Auth.Account, User: claims.Auth.User, Email: claims.Profile.Email, Expires: time.Unix(claims.Exp, 0)}
	return out, nil
}

type Profile struct {
	Status string `json:"status"`
	ID     string `json:"id"`
	Name   string `json:"display_name"`
	Room   string `json:"messaging_room_id"`
	Thread string `json:"active_root_thread_id"`
}
type Message struct {
	ID        string                   `json:"id"`
	Created   time.Time                `json:"created_at"`
	Updated   time.Time                `json:"updated_at"`
	Sender    string                   `json:"account_user_id"`
	RequestID string                   `json:"request_id"`
	Deleted   *time.Time               `json:"deleted_at"`
	Threads   map[string]*ThreadStatus `json:"-"`
	Reactions map[string]string        `json:"reactions"`
	Content   struct {
		Text        string            `json:"text"`
		Attachments []json.RawMessage `json:"attachments"`
	} `json:"content"`
}
type Room struct {
	Type         string        `json:"type"`
	Source       string        `json:"app_source"`
	ID           string        `json:"id"`
	Aeon         string        `json:"aeon_id"`
	ReadReceipts []ReadReceipt `json:"read_receipts"`
	Members      []struct {
		ID   string `json:"account_user_id"`
		Aeon string `json:"aeon_id"`
		Name string `json:"name"`
	} `json:"members"`
	Latest []struct {
		ID      string    `json:"id"`
		Created time.Time `json:"created_at"`
	} `json:"latest_messages"`
}
type Page struct {
	Items []Message `json:"items"`
	Next  string    `json:"next_cursor"`
	Prev  string    `json:"prev_cursor"`
}
type HTTPError struct {
	Status             int
	PrimaryUnavailable bool
}

// TransportError deliberately excludes URLs and credential-bearing request
// details. A caller may retry a GET; it never permits replaying an unknown POST.
type TransportError struct{}

func (*TransportError) Error() string { return "ChatGPT transport failed; delivery may be unknown" }

func (e *HTTPError) Error() string {
	switch e.Status {
	case 401:
		return "ChatGPT authentication expired; reconnect in Beeper"
	case 403:
		return "ChatGPT denied access or requires provider verification; open ChatGPT, then reconnect (no challenge bypass)"
	case 404:
		return "ChatGPT returned HTTP 404"
	case 429:
		return "ChatGPT rate limit; delivery will retry after backoff"
	default:
		return fmt.Sprintf("ChatGPT returned HTTP %d", e.Status)
	}
}

type Client struct {
	credentials        Credentials
	Identity           Identity
	http               *http.Client
	mediaHTTP          *http.Client
	rooms              map[string]bool
	mu                 sync.RWMutex
	authMu             sync.Mutex
	PersistCredentials func(Credentials) error
}

func New(c Credentials, transport http.RoundTripper) (*Client, error) {
	identity, err := c.Identity()
	if err != nil {
		return nil, err
	}
	if transport == nil {
		transport = http.DefaultTransport
		if standard, ok := transport.(*http.Transport); ok {
			configured := standard.Clone()
			configured.ResponseHeaderTimeout = 30 * time.Second
			transport = configured
		}
	}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		credentials: c, Identity: identity,
		http:      &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: noRedirect},
		mediaHTTP: &http.Client{Transport: transport, CheckRedirect: noRedirect},
		rooms:     map[string]bool{},
	}, nil
}
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doAt(ctx, "https://chatgpt.com/backend-api", method, path, body, out)
}
func (c *Client) doAt(ctx context.Context, origin, method, path string, body, out any) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if time.Until(c.Identity.Expires) < 5*time.Minute {
		if err := c.refresh(ctx); err != nil {
			return err
		}
	}
	err := c.request(ctx, method, origin+path, body, out)
	var authError *HTTPError
	if errors.As(err, &authError) && authError.Status == 401 {
		if err = c.refresh(ctx); err != nil {
			return err
		}
		err = c.request(ctx, method, origin+path, body, out)
	}
	return err
}
func (c *Client) request(ctx context.Context, method, target string, body, out any) error {
	var reader io.Reader
	contentType := "application/json"
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return errors.New("invalid provider request")
	}
	req.Header.Set("Authorization", "Bearer "+c.credentials.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", c.Identity.Account)
	req.Header.Set("X-OpenAI-Expected-Account-Id", c.Identity.Account)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "BeeperDotsBridge/0.1")
	if req.URL.Host == "codex-cloud-backend.chatgpt.com" {
		req.Header.Set("X-OpenAI-Product-Sku", "unknown")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &TransportError{}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := &HTTPError{Status: resp.StatusCode}
		if resp.StatusCode == http.StatusNotFound && req.URL.Path == "/backend-api/tbo/primary" {
			var body struct {
				Detail string `json:"detail"`
			}
			failure.PrimaryUnavailable = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) == nil && body.Detail == "Not Found"
		}
		return failure
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return errors.New("unexpected ChatGPT response schema")
	}
	return nil
}
func (c *Client) Credentials() Credentials {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	return c.credentials
}
func (c *Client) Refresh(ctx context.Context) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	return c.refresh(ctx)
}
func (c *Client) refresh(ctx context.Context) error {
	if c.credentials.SessionToken == "" {
		return &HTTPError{Status: 401}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://chatgpt.com/api/auth/session", nil)
	if err != nil {
		return errors.New("cannot prepare ChatGPT renewal")
	}
	req.Header.Set("User-Agent", "BeeperDotsBridge/0.1")
	req.AddCookie(&http.Cookie{Name: "__Secure-next-auth.session-token", Value: c.credentials.SessionToken})
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &TransportError{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("renewing the ChatGPT session: %w", &HTTPError{Status: resp.StatusCode})
	}
	var session struct {
		Access  string `json:"accessToken"`
		Session string `json:"sessionToken"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&session) != nil || session.Access == "" || session.Session == "" {
		return &HTTPError{Status: 401}
	}
	updated := Credentials{AccessToken: session.Access, SessionToken: session.Session}
	identity, err := updated.Identity()
	if err != nil || identity.Account != c.Identity.Account || identity.User != c.Identity.User || time.Until(identity.Expires) < time.Minute {
		return errors.New("ChatGPT renewal changed identity or returned expired access; reconnect the original account")
	}
	if c.PersistCredentials != nil {
		if err = c.PersistCredentials(updated); err != nil {
			return errors.New("could not persist renewed ChatGPT credentials; connector remains disconnected")
		}
	}
	c.credentials = updated
	c.Identity.Expires = identity.Expires
	return nil
}
func (c *Client) Discover(ctx context.Context) ([]Profile, error) {
	var primary struct {
		Selection *struct {
			Thread    string `json:"thread_id"`
			Aeon      string `json:"aeon_id"`
			Room      string `json:"messaging_room_id"`
			Available bool   `json:"available"`
		} `json:"selection"`
	}
	if err := c.do(ctx, "GET", "/tbo/primary", nil, &primary); err != nil {
		return nil, fmt.Errorf("finding the primary Dot: %w", err)
	}
	if primary.Selection == nil || !primary.Selection.Available {
		return nil, errors.New("no available primary Dot; select an existing Dot in ChatGPT")
	}
	s := primary.Selection
	var p Profile
	if s.Thread == "" || s.Room == "" || s.Aeon == "" {
		return nil, errors.New("primary Dot has incomplete identity")
	}
	if err := c.do(ctx, "GET", "/tbo/by-thread/"+url.PathEscape(s.Thread), nil, &p); err != nil {
		return nil, fmt.Errorf("loading the Dot profile: %w", err)
	}
	if p.Status != "active" || p.ID != s.Aeon || p.Room != s.Room || (p.Thread != "" && p.Thread != s.Thread) {
		return nil, errors.New("Dot profile identity mismatch")
	}
	return []Profile{p}, nil
}
func (c *Client) Verify(ctx context.Context, p Profile) (*Room, error) {
	var r Room
	if err := c.do(ctx, "GET", "/messaging/rooms/"+url.PathEscape(p.Room), nil, &r); err != nil {
		return nil, fmt.Errorf("verifying the Dot room: %w", err)
	}
	if r.Type != "DM" || r.Source != "chatgpt:messaging" || r.ID != p.Room || r.Aeon != p.ID {
		return nil, errors.New("room is not the discovered Dot room")
	}
	found := false
	for _, m := range r.Members {
		if m.Aeon == p.ID && m.ID != "" {
			found = true
		}
	}
	if !found {
		return nil, errors.New("Dot room has no verified Dot member")
	}
	c.mu.Lock()
	c.rooms[p.Room] = true
	c.mu.Unlock()
	if len(r.Latest) == 0 {
		var baseline struct {
			Items []struct {
				ID      string    `json:"id"`
				Created time.Time `json:"created_at"`
			} `json:"items"`
		}
		if err := c.do(ctx, "GET", "/messaging/rooms/"+url.PathEscape(p.Room)+"/messages?limit=1", nil, &baseline); err != nil {
			return nil, fmt.Errorf("reading the Dot message baseline: %w", err)
		}
		r.Latest = baseline.Items
	}
	return &r, nil
}
func (c *Client) Messages(ctx context.Context, room, after string) (*Page, error) {
	c.mu.RLock()
	verified := c.rooms[room]
	c.mu.RUnlock()
	if !verified {
		return nil, errors.New("refusing unverified Dot room")
	}
	query := url.Values{"limit": {"32"}}
	if after != "" {
		query.Set("after", after)
	}
	var page Page
	err := c.do(ctx, "GET", "/messaging/rooms/"+url.PathEscape(room)+"/messages?"+query.Encode(), nil, &page)
	return &page, err
}
func (c *Client) Send(ctx context.Context, room, text, requestID string, attachments []FileReference) (*Message, error) {
	c.mu.RLock()
	verified := c.rooms[room]
	c.mu.RUnlock()
	if !verified {
		return nil, errors.New("refusing unverified Dot room")
	}
	body := struct {
		Content struct {
			Text        string          `json:"text"`
			Attachments []FileReference `json:"attachments,omitempty"`
		} `json:"content"`
		Request     string `json:"request_id"`
		Idempotency string `json:"idempotency_token"`
	}{Request: requestID, Idempotency: requestID}
	body.Content.Text = text
	body.Content.Attachments = attachments
	var m Message
	err := c.do(ctx, "POST", "/messaging/rooms/"+url.PathEscape(room)+"/messages", body, &m)
	if err == nil && (m.ID == "" || m.RequestID != requestID) {
		err = errors.New("send response missing matching remote message identity; delivery unknown")
	}
	return &m, err
}
