package weixin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}
}

func testClient(t *testing.T, fn transportFunc) *Client {
	t.Helper()
	c, err := NewClient(Config{HTTPClient: &http.Client{Transport: fn}}, Credentials{
		BotToken: "test-secret", BotID: "bot", UserID: "owner", BaseURL: DefaultBaseURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTrustedOriginRejectsCredentialsToArbitraryHost(t *testing.T) {
	for _, raw := range []string{
		"http://ilinkai.weixin.qq.com", "https://ilinkai.weixin.qq.com.evil.example",
		"https://evil.weixin.qq.com@evil.example", "https://evil.example@ilinkai.weixin.qq.com",
		"https://ilinkai.weixin.qq.com:444", "https://127.0.0.1", "https://qq.com",
		"https://ilinkai.weixin.qq.com/path", "https://ilinkai.weixin.qq.com?token=secret",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := NewClient(Config{BaseURL: raw}, Credentials{BotToken: "secret"}); !errors.Is(err, ErrUnsafeURL) {
				t.Fatalf("unsafe origin was accepted: %v", err)
			}
		})
	}
}

func TestUpdatesPreserveLargeIdentifiersCursorAndContext(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/ilink/bot/getupdates" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatalf("unexpected poll request")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["get_updates_buf"] != "previous" {
			t.Fatalf("poll did not preserve cursor")
		}
		return reply(r, 200, `{"ret":0,"get_updates_buf":"next","longpolling_timeout_ms":35000,"msgs":[{"message_id":18446744073709551615,"seq":18446744073709551614,"from_user_id":"owner","context_token":" raw context ","item_list":[{"type":1,"msg_id":9007199254740993,"text_item":{"text":"message_id: 99999999999999999999"},"ref_msg":{"svr_id":18446744073709551613}}],"unknown":{"preserved":true}}]}`), nil
	})
	updates, err := c.GetUpdates(context.Background(), "previous")
	if err != nil {
		t.Fatal(err)
	}
	m := updates.Messages[0]
	if updates.Cursor != "next" || m.MessageID != "18446744073709551615" || m.Seq.String() != "18446744073709551614" || m.ItemList[0].MessageID != "9007199254740993" || m.ItemList[0].RefMessage.ServerID != "18446744073709551613" {
		t.Fatalf("identifier/cursor lost: %#v", updates)
	}
	if m.ContextToken != " raw context " || !strings.Contains(string(m.Raw), `"unknown":{"preserved":true}`) {
		t.Fatal("context or unknown protocol fields were discarded")
	}
}

func TestSendEchoesContextAndReturnsAckWithoutRetry(t *testing.T) {
	requests := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Header.Get("iLink-App-ClientVersion") != "132105" || r.Header.Get("AuthorizationType") != "ilink_bot_token" {
			t.Fatal("missing protocol headers")
		}
		if _, err := base64.StdEncoding.DecodeString(r.Header.Get("X-WECHAT-UIN")); err != nil {
			t.Fatalf("invalid UIN header: %v", err)
		}
		var body struct {
			Message  Message           `json:"msg"`
			BaseInfo map[string]string `json:"base_info"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Message.ContextToken != " exact token " || body.Message.ClientID != "persisted-id" || body.Message.ToUserID != "owner" || body.Message.MessageType != BotMessage || body.Message.MessageState != FinishedState || body.Message.ItemList[0].TextItem.Text != "hello" || body.BaseInfo["channel_version"] != SourceVersion {
			t.Fatal("send request changed the stable identity, context or text")
		}
		return reply(r, 200, `{"ret":0,"message_id":18446744073709551615}`), nil
	})
	result, err := c.SendText(context.Background(), SendTextRequest{ToUserID: "owner", ClientID: "persisted-id", ContextToken: " exact token ", Text: "hello"})
	if err != nil || result.MessageID != "18446744073709551615" || requests != 1 {
		t.Fatalf("bad send result: %#v, %v, requests=%d", result, err, requests)
	}
}

func TestMissingContextDoesNotSend(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		t.Fatal("request was sent with no context token")
		return nil, nil
	})
	_, err := c.SendText(context.Background(), SendTextRequest{ToUserID: "owner", ClientID: "id", Text: "hello"})
	if !errors.Is(err, ErrContextRequired) {
		t.Fatalf("wrong local error: %v", err)
	}
}

func TestExpiredAndUnknownOutcomesRemainDistinct(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"expired", 200, `{"ret":-14,"errmsg":"secret"}`, ErrSessionExpired},
		{"expired-errcode", 200, `{"ret":0,"errcode":-14}`, ErrSessionExpired},
		{"missing-ack", 200, `{"ret":0}`, ErrOutcomeUnknown},
		{"broken-response", 200, `{"secret":`, ErrOutcomeUnknown},
		{"null-response", 200, `null`, ErrOutcomeUnknown},
		{"server-failure", 503, `secret`, ErrOutcomeUnknown},
		{"rejected", 403, `secret`, ErrAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				requests++
				return reply(r, tc.status, tc.body), nil
			})
			_, err := c.SendText(context.Background(), SendTextRequest{ToUserID: "owner", ClientID: "id", ContextToken: "context", Text: "hello"})
			if !errors.Is(err, tc.want) || requests != 1 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe error/retry classification: %v, requests=%d", err, requests)
			}
		})
	}
}

func TestSendBusinessReasonIsExactSafeEnumForTextAndMedia(t *testing.T) {
	for _, sendKind := range []string{"text", "media"} {
		for _, tc := range []struct {
			name, body, reason string
			want               error
		}{
			{"rate", `{"ret":-2,"errmsg":"rate limited"}`, APIReasonRateLimited, ErrAPI},
			{"prepare", `{"ret":-2,"errmsg":"prepare failed"}`, APIReasonPrepareFailed, ErrAPI},
			{"empty", `{"ret":-2,"errmsg":""}`, APIReasonEmpty, ErrAPI},
			{"missing", `{"ret":-2}`, APIReasonEmpty, ErrAPI},
			{"null", `{"ret":-2,"errmsg":null}`, APIReasonEmpty, ErrAPI},
			{"opaque", `{"ret":-2,"errmsg":"private-error-marker"}`, APIReasonOther, ErrAPI},
			{"prefix", `{"ret":-2,"errmsg":"rate limited private-error-marker"}`, APIReasonOther, ErrAPI},
			{"whitespace", `{"ret":-2,"errmsg":" rate limited "}`, APIReasonOther, ErrAPI},
			{"case", `{"ret":-2,"errmsg":"RATE LIMITED"}`, APIReasonOther, ErrAPI},
			{"object", `{"ret":-2,"errmsg":{"secret":"private-error-marker"}}`, APIReasonOther, ErrAPI},
			{"expired", `{"ret":0,"errcode":-14,"errmsg":"private-error-marker"}`, APIReasonOther, ErrSessionExpired},
		} {
			t.Run(sendKind+"/"+tc.name, func(t *testing.T) {
				requests := 0
				c := testClient(t, func(r *http.Request) (*http.Response, error) {
					requests++
					return reply(r, 200, tc.body), nil
				})
				var err error
				if sendKind == "text" {
					_, err = c.SendText(context.Background(), SendTextRequest{ToUserID: "owner", ClientID: "id", ContextToken: "context", Text: "hello"})
				} else {
					_, err = c.SendMedia(context.Background(), SendMediaRequest{ToUserID: "owner", ClientID: "id", ContextToken: "context", Uploaded: UploadedMedia{
						Type: ImageItemType, ToUserID: "owner", RawSize: 1, CipherSize: 16,
						AESKeyHex: strings.Repeat("00", 16), DownloadParam: "fixture-download-reference",
					}})
				}
				var api *APIError
				if !errors.Is(err, tc.want) || !errors.As(err, &api) || api.Op != "sendmessage" || api.Reason != tc.reason || api.HTTPStatus != 0 || requests != 1 {
					t.Fatal("reason diagnostic changed the business rejection or repeated the send")
				}
				encoded, marshalErr := json.Marshal(api)
				if marshalErr != nil || strings.Contains(string(encoded), "private-error-marker") || strings.Contains(api.Error(), "private-error-marker") || !strings.Contains(api.Error(), "reason="+tc.reason) {
					t.Fatal("server error text escaped the reason whitelist")
				}
			})
		}
	}
}

func TestReasonDiagnosticDoesNotChangeHTTPOrUnconfirmedResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"missing-ack", 200, `{"ret":0,"errmsg":"rate limited"}`, ErrOutcomeUnknown},
		{"trailing-json", 200, `{"ret":-2,"errmsg":"rate limited"}{}`, ErrOutcomeUnknown},
		{"invalid-ret", 200, `{"ret":"-2","errmsg":"rate limited"}`, ErrOutcomeUnknown},
		{"invalid-id", 200, `{"ret":-2,"message_id":{},"errmsg":"rate limited"}`, ErrOutcomeUnknown},
		{"http-server", 503, `{"ret":-2,"errmsg":"rate limited"}`, ErrOutcomeUnknown},
		{"http-reject", 403, `{"ret":-2,"errmsg":"rate limited"}`, ErrAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				requests++
				return reply(r, tc.status, tc.body), nil
			})
			_, err := c.SendText(context.Background(), SendTextRequest{ToUserID: "owner", ClientID: "id", ContextToken: "context", Text: "hello"})
			var api *APIError
			if !errors.Is(err, tc.want) || !errors.As(err, &api) || api.Reason != "" || requests != 1 {
				t.Fatal("reason diagnostic reclassified an HTTP or unknown send result")
			}
		})
	}
	api := &APIError{Op: "sendmessage", Kind: ErrAPI, Reason: "private-error-marker"}
	if strings.Contains(api.Error(), "private-error-marker") || !strings.Contains(api.Error(), "reason=other") {
		t.Fatal("untrusted reason was printed verbatim")
	}
}

func TestSendNetworkFailureIsUnknownAndDoesNotExposeURL(t *testing.T) {
	requests := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("network error with private-token or QR URL")
	})
	_, err := c.SendText(context.Background(), SendTextRequest{ToUserID: "owner", ClientID: "id", ContextToken: "context", Text: "hello"})
	if !errors.Is(err, ErrOutcomeUnknown) || requests != 1 || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("unsafe network outcome: %v, requests=%d", err, requests)
	}
}

func TestHTTPRedirectNeverForwardsAuthorization(t *testing.T) {
	requests := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "ilinkai.weixin.qq.com" {
			t.Fatal("followed redirect with credentials")
		}
		response := reply(r, http.StatusTemporaryRedirect, "")
		response.Header.Set("Location", "https://evil.example/steal")
		return response, nil
	})
	_, err := c.GetUpdates(context.Background(), "cursor")
	if !errors.Is(err, ErrUnsafeURL) || requests != 1 {
		t.Fatalf("redirect was not blocked: %v, requests=%d", err, requests)
	}
}

func TestQRRedirectAndNativeVerification(t *testing.T) {
	responses := []string{
		`{"qrcode":"secret-qr","qrcode_img_content":"https://ilinkai.weixin.qq.com/qr"}`,
		`{"status":"scaned_but_redirect","redirect_host":"region.weixin.qq.com"}`,
		`{"status":"need_verifycode"}`,
		`{"status":"confirmed","bot_token":"private-token","ilink_bot_id":"bot","ilink_user_id":"owner","baseurl":"https://region.weixin.qq.com"}`,
	}
	requests := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("QR request received bot authorization")
		}
		if requests == 0 {
			if r.Method != http.MethodPost || r.URL.Query().Get("bot_type") != "3" {
				t.Fatal("incorrect QR creation request")
			}
		} else {
			if r.Method != http.MethodGet || r.URL.Query().Get("qrcode") != "secret-qr" || r.Header.Get("AuthorizationType") != "" || r.Header.Get("X-WECHAT-UIN") != "" {
				t.Fatal("incorrect QR status request")
			}
			if requests > 1 && r.URL.Host != "region.weixin.qq.com" {
				t.Fatal("validated QR redirect was not followed")
			}
			if requests == 3 && r.URL.Query().Get("verify_code") != "123456" {
				t.Fatal("user verification code was not forwarded")
			}
		}
		response := reply(r, 200, responses[requests])
		requests++
		return response, nil
	})
	session, err := c.StartQR(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	status, err := c.PollQR(context.Background(), session, "")
	if err != nil || status.State != QRRedirect || session.PollBaseURL != "https://region.weixin.qq.com" {
		t.Fatalf("redirect failure: %#v, %v", status, err)
	}
	status, err = c.PollQR(context.Background(), session, "")
	if err != nil || !status.NeedsVerification || status.Credentials != nil {
		t.Fatalf("verification was hidden: %#v, %v", status, err)
	}
	status, err = c.PollQR(context.Background(), session, "123456")
	if err != nil || status.Credentials == nil || status.Credentials.UserID != "owner" || status.Credentials.BaseURL != "https://region.weixin.qq.com" {
		t.Fatalf("confirmation failure: %#v, %v", status, err)
	}
}

func TestUntrustedQRRedirectDoesNotMutateSession(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		return reply(r, 200, `{"status":"scaned_but_redirect","redirect_host":"weixin.qq.com.evil.example"}`), nil
	})
	session := &QRSession{QRCode: "secret", PollBaseURL: DefaultBaseURL}
	_, err := c.PollQR(context.Background(), session, "")
	if !errors.Is(err, ErrUnsafeURL) || session.PollBaseURL != DefaultBaseURL {
		t.Fatalf("unsafe redirect was accepted: %v", err)
	}
}

func TestAlreadyBoundDoesNotInventCredentials(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		return reply(r, 200, `{"status":"binded_redirect"}`), nil
	})
	status, err := c.PollQR(context.Background(), &QRSession{QRCode: "secret"}, "")
	if err != nil || status.State != QRAlreadyBound || status.Credentials != nil {
		t.Fatalf("already-bound was treated as a new login: %#v, %v", status, err)
	}
}
