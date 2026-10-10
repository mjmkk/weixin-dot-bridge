// Protocol adapted from Tencent/openclaw-weixin (MIT); see weixin-source.md.
package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ErrUnsafeURL          = errors.New("untrusted Weixin URL or redirect")
	ErrInvalidRequest     = errors.New("invalid Weixin request")
	ErrCredentialsMissing = errors.New("Weixin credentials are missing")
	ErrContextRequired    = errors.New("Weixin reply context token is missing")
	ErrSessionExpired     = errors.New("Weixin session has expired")
	ErrOutcomeUnknown     = errors.New("Weixin send outcome is unknown; do not blindly resend")
	ErrAPI                = errors.New("Weixin API rejected the request")
	ErrProtocol           = errors.New("unexpected Weixin API response")
)

// APIError deliberately excludes URLs, request bodies, credentials, and server
// error text because any of those may echo secret values. Use errors.Is for Kind.
type APIError struct {
	Op         string
	Kind       error
	HTTPStatus int
	Ret        int
	ErrCode    int
	cause      error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Weixin %s: %s (http=%d ret=%d errcode=%d)", e.Op, e.Kind, e.HTTPStatus, e.Ret, e.ErrCode)
}

func (e *APIError) Is(target error) bool { return target == e.Kind }
func (e *APIError) Unwrap() error        { return e.cause }

type Client struct {
	config Config
	creds  Credentials
	base   *url.URL
	http   *http.Client
}

// Tencent's documented iLink/CDN hosts are below weixin.qq.com. The default
// trust boundary deliberately excludes the broader qq.com namespace, IPs,
// userinfo, non-HTTPS, custom ports, and base URL paths or queries.
func trustedBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return nil, ErrUnsafeURL
	}
	host := strings.ToLower(u.Hostname())
	if host != "weixin.qq.com" && !strings.HasSuffix(host, ".weixin.qq.com") {
		return nil, ErrUnsafeURL
	}
	if host == "" || strings.ContainsAny(host, "\r\n\t ") || (u.Port() != "" && u.Port() != "443") ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, ErrUnsafeURL
	}
	u.Scheme = "https"
	u.Host = host
	u.Path = "/"
	return u, nil
}

var agentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}/[A-Za-z0-9_.+-]{1,32}$`)

func NewClient(config Config, creds Credentials) (*Client, error) {
	baseRaw := config.BaseURL
	if baseRaw == "" {
		baseRaw = creds.BaseURL
	}
	if baseRaw == "" {
		baseRaw = DefaultBaseURL
	}
	base, err := trustedBaseURL(baseRaw)
	if err != nil {
		return nil, err
	}
	if config.BotAgent == "" {
		config.BotAgent = "SpiritWeixinDot/0.1.0"
	}
	if !agentPattern.MatchString(config.BotAgent) {
		return nil, ErrInvalidRequest
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 15 * time.Second
	}
	if config.LongPollTimeout == 0 {
		config.LongPollTimeout = 35 * time.Second
	}
	if config.RequestTimeout < 0 || config.LongPollTimeout < 0 {
		return nil, ErrInvalidRequest
	}
	if strings.ContainsAny(creds.BotToken, "\r\n") {
		return nil, ErrInvalidRequest
	}
	httpClient := &http.Client{}
	if config.HTTPClient != nil {
		copy := *config.HTTPClient
		httpClient = &copy
	}
	// Even trusted-host HTTP redirects are not automatically followed. Protocol
	// QR JSON redirects are separately validated; no Bearer header is forwarded.
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return ErrUnsafeURL }
	return &Client{config: config, creds: creds, base: base, http: httpClient}, nil
}

func (c *Client) baseInfo() map[string]string {
	return map[string]string{"channel_version": SourceVersion, "bot_agent": c.config.BotAgent}
}

func businessError(op string, ret, code int) error {
	if ret == 0 && code == 0 {
		return nil
	}
	kind := ErrAPI
	if ret == -14 || code == -14 {
		kind = ErrSessionExpired
	}
	return &APIError{Op: op, Kind: kind, Ret: ret, ErrCode: code}
}

func (c *Client) do(ctx context.Context, base *url.URL, method, endpoint string, body any, token string, timeout time.Duration, send bool, output any) error {
	op := strings.Split(endpoint, "?")[0]
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return &APIError{Op: op, Kind: ErrInvalidRequest}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpointURL, err := base.Parse(endpoint)
	if err != nil {
		return &APIError{Op: op, Kind: ErrInvalidRequest}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpointURL.String(), bytes.NewReader(payload))
	if err != nil {
		return &APIError{Op: op, Kind: ErrInvalidRequest}
	}
	req.Header.Set("iLink-App-Id", "bot")
	req.Header.Set("iLink-App-ClientVersion", "132105") // 2<<16 | 4<<8 | 9
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("AuthorizationType", "ilink_bot_token")
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return &APIError{Op: op, Kind: ErrInvalidRequest}
		}
		value := strconv.FormatUint(uint64(binary.BigEndian.Uint32(random[:])), 10)
		req.Header.Set("X-WECHAT-UIN", base64.StdEncoding.EncodeToString([]byte(value)))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	response, err := c.http.Do(req)
	if err != nil {
		kind := ErrAPI
		if send {
			kind = ErrOutcomeUnknown
		}
		if errors.Is(err, ErrUnsafeURL) && !send {
			kind = ErrUnsafeURL
		}
		// Keep only context cancellation as a wrapped cause. net/url errors can
		// contain QR codes or credentials and must not enter ordinary logs.
		var cause error
		if ctx.Err() != nil {
			cause = ctx.Err()
		}
		return &APIError{Op: op, Kind: kind, cause: cause}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		kind := ErrAPI
		if send && (response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout || (response.StatusCode >= 300 && response.StatusCode < 400)) {
			kind = ErrOutcomeUnknown
		}
		return &APIError{Op: op, Kind: kind, HTTPStatus: response.StatusCode}
	}
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		kind := ErrProtocol
		if send {
			kind = ErrOutcomeUnknown
		}
		return &APIError{Op: op, Kind: kind, HTTPStatus: response.StatusCode}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		kind := ErrProtocol
		if send {
			kind = ErrOutcomeUnknown
		}
		return &APIError{Op: op, Kind: kind, HTTPStatus: response.StatusCode}
	}
	valueDecoder := json.NewDecoder(bytes.NewReader(raw))
	valueDecoder.UseNumber()
	if err := valueDecoder.Decode(output); err != nil {
		kind := ErrProtocol
		if send {
			kind = ErrOutcomeUnknown
		}
		return &APIError{Op: op, Kind: kind, HTTPStatus: response.StatusCode}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		kind := ErrProtocol
		if send {
			kind = ErrOutcomeUnknown
		}
		return &APIError{Op: op, Kind: kind, HTTPStatus: response.StatusCode}
	}
	return nil
}

func (c *Client) GetUpdates(ctx context.Context, cursor string) (*Updates, error) {
	if c.creds.BotToken == "" {
		return nil, ErrCredentialsMissing
	}
	var response Updates
	err := c.do(ctx, c.base, http.MethodPost, "ilink/bot/getupdates", map[string]any{
		"get_updates_buf": cursor,
		"base_info":       c.baseInfo(),
	}, c.creds.BotToken, c.config.LongPollTimeout, false, &response)
	if err != nil {
		return nil, err
	}
	if err := businessError("getupdates", response.Ret, response.ErrCode); err != nil {
		return nil, err
	}
	return &response, nil
}

func (c *Client) SendText(ctx context.Context, input SendTextRequest) (*SendResult, error) {
	if c.creds.BotToken == "" {
		return nil, ErrCredentialsMissing
	}
	if input.ContextToken == "" {
		return nil, ErrContextRequired
	}
	if input.ToUserID == "" || input.ClientID == "" || input.Text == "" {
		return nil, ErrInvalidRequest
	}
	message := map[string]any{
		"from_user_id": "", "to_user_id": input.ToUserID, "client_id": input.ClientID,
		"message_type": BotMessage, "message_state": FinishedState,
		"context_token": input.ContextToken,
		"item_list":     []MessageItem{{Type: TextItemType, TextItem: &TextItem{Text: input.Text}}},
	}
	if input.RunID != "" {
		message["run_id"] = input.RunID
	}
	var response struct {
		MessageID ID  `json:"message_id"`
		Ret       int `json:"ret"`
		ErrCode   int `json:"errcode"`
	}
	err := c.do(ctx, c.base, http.MethodPost, "ilink/bot/sendmessage", map[string]any{
		"msg": message, "base_info": c.baseInfo(),
	}, c.creds.BotToken, c.config.RequestTimeout, true, &response)
	if err != nil {
		return nil, err
	}
	if err := businessError("sendmessage", response.Ret, response.ErrCode); err != nil {
		return nil, err
	}
	if response.MessageID == "" {
		return nil, &APIError{Op: "sendmessage", Kind: ErrOutcomeUnknown}
	}
	return &SendResult{MessageID: response.MessageID, ClientID: input.ClientID}, nil
}
