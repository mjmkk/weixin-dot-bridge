// Protocol adapted from Tencent/openclaw-weixin (MIT); see weixin-source.md.
package weixin

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

type QRState string

const (
	QRWait         QRState = "wait"
	QRScanned      QRState = "scaned"
	QRConfirmed    QRState = "confirmed"
	QRExpired      QRState = "expired"
	QRRedirect     QRState = "scaned_but_redirect"
	QRNeedsCode    QRState = "need_verifycode"
	QRCodeBlocked  QRState = "verify_code_blocked"
	QRAlreadyBound QRState = "binded_redirect"
)

// QRSession contains login secrets; persist only privately if the caller needs
// to resume login. DisplayURL is the server's value to render as a QR code.
type QRSession struct {
	QRCode      string `json:"qrcode"`
	DisplayURL  string `json:"display_url"`
	PollBaseURL string `json:"poll_base_url"`
}

type QRStatus struct {
	State             QRState
	NeedsVerification bool
	RedirectBaseURL   string
	// Credentials are returned only for a complete, validated confirmation.
	Credentials *Credentials
}

func (c *Client) StartQR(ctx context.Context, localTokens []string) (*QRSession, error) {
	base, _ := trustedBaseURL(DefaultBaseURL)
	if localTokens == nil {
		localTokens = []string{}
	}
	if len(localTokens) > 10 {
		return nil, ErrInvalidRequest
	}
	var response struct {
		QRCode       string `json:"qrcode"`
		ImageContent string `json:"qrcode_img_content"`
		Ret          int    `json:"ret"`
		ErrCode      int    `json:"errcode"`
	}
	err := c.do(ctx, base, http.MethodPost, "ilink/bot/get_bot_qrcode?bot_type=3",
		map[string]any{"local_token_list": localTokens}, "", c.config.RequestTimeout, false, &response)
	if err != nil {
		return nil, err
	}
	if err := businessError("get_bot_qrcode", response.Ret, response.ErrCode); err != nil {
		return nil, err
	}
	if response.QRCode == "" || response.ImageContent == "" {
		return nil, &APIError{Op: "get_bot_qrcode", Kind: ErrProtocol}
	}
	return &QRSession{QRCode: response.QRCode, DisplayURL: response.ImageContent, PollBaseURL: DefaultBaseURL}, nil
}

// PollQR performs exactly one poll. The caller displays native challenges and
// obtains any required verification code directly from the user. It never
// guesses codes, refreshes QR codes, or retries binding automatically.
func (c *Client) PollQR(ctx context.Context, session *QRSession, verifyCode string) (*QRStatus, error) {
	if session == nil || session.QRCode == "" {
		return nil, ErrInvalidRequest
	}
	baseRaw := session.PollBaseURL
	if baseRaw == "" {
		baseRaw = DefaultBaseURL
	}
	base, err := trustedBaseURL(baseRaw)
	if err != nil {
		return nil, err
	}
	query := url.Values{"qrcode": {session.QRCode}}
	if verifyCode != "" {
		query.Set("verify_code", verifyCode)
	}
	var response struct {
		Status       QRState `json:"status"`
		BotToken     string  `json:"bot_token"`
		BotID        string  `json:"ilink_bot_id"`
		BaseURL      string  `json:"baseurl"`
		UserID       string  `json:"ilink_user_id"`
		RedirectHost string  `json:"redirect_host"`
		Ret          int     `json:"ret"`
		ErrCode      int     `json:"errcode"`
	}
	err = c.do(ctx, base, http.MethodGet, "ilink/bot/get_qrcode_status?"+query.Encode(),
		nil, "", c.config.LongPollTimeout, false, &response)
	if err != nil {
		return nil, err
	}
	if err := businessError("get_qrcode_status", response.Ret, response.ErrCode); err != nil {
		return nil, err
	}
	status := &QRStatus{State: response.Status}
	switch response.Status {
	case QRWait, QRScanned, QRExpired, QRCodeBlocked, QRAlreadyBound:
		// Already-bound is not a new confirmation and returns no invented token.
	case QRNeedsCode:
		status.NeedsVerification = true
	case QRRedirect:
		if response.RedirectHost == "" || strings.ContainsAny(response.RedirectHost, "/?#@") {
			return nil, &APIError{Op: "get_qrcode_status", Kind: ErrUnsafeURL}
		}
		redirect, err := trustedBaseURL("https://" + response.RedirectHost)
		if err != nil {
			return nil, &APIError{Op: "get_qrcode_status", Kind: ErrUnsafeURL}
		}
		session.PollBaseURL = strings.TrimSuffix(redirect.String(), "/")
		status.RedirectBaseURL = session.PollBaseURL
	case QRConfirmed:
		if response.BotToken == "" || response.BotID == "" || response.UserID == "" {
			return nil, &APIError{Op: "get_qrcode_status", Kind: ErrProtocol}
		}
		confirmedBase := response.BaseURL
		if confirmedBase == "" {
			confirmedBase = baseRaw
		}
		trusted, err := trustedBaseURL(confirmedBase)
		if err != nil {
			return nil, &APIError{Op: "get_qrcode_status", Kind: ErrUnsafeURL}
		}
		status.Credentials = &Credentials{
			BotToken: response.BotToken, BotID: response.BotID, UserID: response.UserID,
			BaseURL: strings.TrimSuffix(trusted.String(), "/"),
		}
	default:
		return nil, &APIError{Op: "get_qrcode_status", Kind: ErrProtocol}
	}
	return status, nil
}
