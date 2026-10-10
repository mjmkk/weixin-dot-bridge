// Protocol adapted from Tencent/openclaw-weixin, commit
// 24de5c9eb0dd5e595d7e2d090ed8a3f82870d42c (MIT).
// The Tencent copyright and permission notice are in docs/weixin-source.md.
package weixin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	DefaultBaseURL = "https://ilinkai.weixin.qq.com"
	DefaultCDNURL  = "https://novac2c.cdn.weixin.qq.com/c2c"
	SourceVersion  = "2.4.9"
	SourceCommit   = "24de5c9eb0dd5e595d7e2d090ed8a3f82870d42c"
	TextItemType   = 1
	UserMessage    = 1
	BotMessage     = 2
	FinishedState  = 2
)

// Credentials belong in the caller's private credential store, never in logs.
type Credentials struct {
	BotToken string `json:"bot_token"`
	BotID    string `json:"ilink_bot_id"`
	BaseURL  string `json:"base_url"`
	UserID   string `json:"ilink_user_id"`
}

type Config struct {
	BaseURL         string
	BotAgent        string
	RequestTimeout  time.Duration
	LongPollTimeout time.Duration
	// A supplied client is copied and HTTP redirects are always disabled.
	HTTPClient *http.Client
}

// ID preserves uint64 wire identifiers without conversion through float64.
// It accepts either a JSON string or an unsigned integer and marshals as a string.
type ID string

func (id *ID) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		*id = ""
		return nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		*id = ID(value)
		return nil
	}
	if _, err := strconv.ParseUint(string(raw), 10, 64); err != nil {
		return fmt.Errorf("invalid unsigned message identifier")
	}
	*id = ID(raw)
	return nil
}

type TextItem struct {
	Text string `json:"text"`
}

// VoiceFormat preserves source metadata separately from a recognized byte
// header. DeclaredCodec describes Tencent's encode_type; Codec may be corrected
// by an unambiguous header. Neither field proves destination understanding.
type VoiceFormat struct {
	EncodeType    int    `json:"encode_type"`
	BitsPerSample int    `json:"bits_per_sample"`
	SampleRate    int    `json:"sample_rate"`
	PlaytimeMS    int64  `json:"playtime_ms"`
	DeclaredCodec string `json:"declared_codec"`
	Codec         string `json:"codec"`
	Container     string `json:"container"`
}

type RefMessage struct {
	ServerID    ID              `json:"svr_id,omitempty"`
	Title       string          `json:"title,omitempty"`
	MessageItem *MessageItem    `json:"message_item,omitempty"`
	PartialText json.RawMessage `json:"partial_text,omitempty"`
}

// Unsupported items remain visible and retain their metadata. Receiving a media
// reference does not mean its original bytes have been downloaded or delivered.
type MessageItem struct {
	Type           int             `json:"type"`
	MessageID      ID              `json:"msg_id,omitempty"`
	TextItem       *TextItem       `json:"text_item,omitempty"`
	RefMessage     *RefMessage     `json:"ref_msg,omitempty"`
	ImageItem      json.RawMessage `json:"image_item,omitempty"`
	VoiceItem      json.RawMessage `json:"voice_item,omitempty"`
	FileItem       json.RawMessage `json:"file_item,omitempty"`
	VideoItem      json.RawMessage `json:"video_item,omitempty"`
	ToolCallStart  json.RawMessage `json:"tool_call_start_item,omitempty"`
	ToolCallResult json.RawMessage `json:"tool_call_result_item,omitempty"`
}

type Message struct {
	Seq          json.Number     `json:"seq,omitempty"`
	MessageID    ID              `json:"message_id,omitempty"`
	FromUserID   string          `json:"from_user_id,omitempty"`
	ToUserID     string          `json:"to_user_id,omitempty"`
	ClientID     string          `json:"client_id,omitempty"`
	CreateTimeMS json.Number     `json:"create_time_ms,omitempty"`
	UpdateTimeMS json.Number     `json:"update_time_ms,omitempty"`
	DeleteTimeMS json.Number     `json:"delete_time_ms,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	GroupID      string          `json:"group_id,omitempty"`
	MessageType  int             `json:"message_type,omitempty"`
	MessageState int             `json:"message_state,omitempty"`
	ItemList     []MessageItem   `json:"item_list,omitempty"`
	ContextToken string          `json:"context_token,omitempty"`
	RunID        string          `json:"run_id,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

func (m *Message) UnmarshalJSON(raw []byte) error {
	type messageAlias Message
	var value messageAlias
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*m = Message(value)
	m.Raw = append(json.RawMessage(nil), raw...)
	return nil
}

type Updates struct {
	Ret                  int       `json:"ret"`
	ErrCode              int       `json:"errcode"`
	Messages             []Message `json:"msgs"`
	Cursor               string    `json:"get_updates_buf"`
	LongPollingTimeoutMS int64     `json:"longpolling_timeout_ms"`
}

// ClientID must be allocated and persisted by the caller before sending. The
// client does not create another ID or retry a request with an unknown outcome.
type SendTextRequest struct {
	ToUserID     string
	Text         string
	ContextToken string
	ClientID     string
	RunID        string
}

// SendResult is an API acknowledgement, not proof of receipt in Weixin.
type SendResult struct {
	MessageID ID
	ClientID  string
}
