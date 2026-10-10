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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
	"github.com/mjmkk/weixin-dot-bridge/internal/weixin"
)

// This journal contains message content and reply tokens. It is private runtime
// state, never an evidence log. Every attempted POST is recorded before sending.
type bridgeState struct {
	Version      int               `json:"version"`
	Account      string            `json:"account"`
	User         string            `json:"user"`
	Dot          string            `json:"dot"`
	Room         string            `json:"room"`
	DotSender    string            `json:"dot_sender"`
	Bot          string            `json:"weixin_bot"`
	Peer         string            `json:"weixin_peer"`
	Started      time.Time         `json:"started_at"`
	Cursor       string            `json:"weixin_cursor"`
	ContextToken string            `json:"context_token,omitempty"`
	DotBaseline  string            `json:"dot_baseline"`
	DotAfter     string            `json:"dot_after"`
	Inbound      []*bridgeInbound  `json:"inbound"`
	Outbound     []*bridgeOutbound `json:"outbound"`
	NativeSeen   map[string]string `json:"native_seen"`
}

type bridgeInbound struct {
	Key         string                 `json:"key"`
	Message     weixin.Message         `json:"message"`
	Raw         json.RawMessage        `json:"raw"`
	Status      string                 `json:"status"`
	RequestID   string                 `json:"request_id,omitempty"`
	Baseline    string                 `json:"dot_baseline,omitempty"`
	NativeID    string                 `json:"native_id,omitempty"`
	Attempts    int                    `json:"attempts,omitempty"`
	RetryAt     time.Time              `json:"retry_at,omitempty"`
	Media       []*bridgeIncomingMedia `json:"media,omitempty"`
	LastFailure *bridgeSendFailure     `json:"last_failure,omitempty"`
}

type bridgeOutbound struct {
	Key                  string                      `json:"key"`
	Native               *chatgpt.Message            `json:"native,omitempty"`
	Text                 string                      `json:"text"`
	Status               string                      `json:"status"`
	ClientID             string                      `json:"client_id"`
	ContextToken         string                      `json:"context_token,omitempty"`
	WeixinID             weixin.ID                   `json:"weixin_id,omitempty"`
	Attempts             int                         `json:"attempts,omitempty"`
	RetryAt              time.Time                   `json:"retry_at,omitempty"`
	Media                *bridgeOutgoingMedia        `json:"media,omitempty"`
	DeliveryKind         string                      `json:"delivery_kind,omitempty"`
	LastFailure          *bridgeSendFailure          `json:"last_failure,omitempty"`
	MetadataSupersession *bridgeMetadataSupersession `json:"metadata_supersession,omitempty"`
}

// Only numeric protocol facts, a fixed local operation name, and a fixed reason
// enum are retained.
// Raw errors can include secrets and must never be written to the journal.
type bridgeSendFailure struct {
	Op         string `json:"op"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Ret        int    `json:"ret,omitempty"`
	ErrCode    int    `json:"errcode,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type bridgeMetadataSupersession struct {
	PreviousStatus  string `json:"previous_status"`
	AcknowledgedKey string `json:"acknowledged_key"`
}

type bridgeNative interface {
	Messages(context.Context, string, string) (*chatgpt.Page, error)
	Send(context.Context, string, string, string, []chatgpt.FileReference) (*chatgpt.Message, error)
}

type bridgeWeixin interface {
	GetUpdates(context.Context, string) (*weixin.Updates, error)
	SendText(context.Context, weixin.SendTextRequest) (*weixin.SendResult, error)
}

type bridgeEngine struct {
	state          *bridgeState
	native         bridgeNative
	weixin         bridgeWeixin
	persist        func(*bridgeState) error
	lookup         func(context.Context, receipt) (*chatgpt.Message, error)
	log            func(string)
	allowHistory   bool
	readFailures   int
	readAfter      time.Time
	lastRead       time.Time
	reconcileAfter time.Time
	mediaDir       string
	mediaBusy      *bridgeMediaJob
	mediaResults   chan bridgeMediaResult
	prepareMedia   func(context.Context, weixin.MediaUploadRequest, weixin.MediaOptions) (*weixin.MediaUploadPlan, error)
	mediaWorkers   sync.WaitGroup
}

func bridgeID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("cannot allocate a delivery identifier")
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func bridgeHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Native file download capabilities refresh without changing the message.
// Ignore only the observed volatile field, retaining all attachment identities,
// permissions, statuses and other payload fields for edit detection.
func bridgeNativeFingerprint(m chatgpt.Message) string {
	if len(m.Content.Attachments) == 0 {
		return bridgeHash(m)
	}
	attachments := make([]json.RawMessage, len(m.Content.Attachments))
	copy(attachments, m.Content.Attachments)
	for i, raw := range attachments {
		var attachment map[string]json.RawMessage
		var kind string
		if json.Unmarshal(raw, &attachment) != nil || json.Unmarshal(attachment["type"], &kind) != nil || kind != "file" {
			continue
		}
		var file map[string]json.RawMessage
		if json.Unmarshal(attachment["file"], &file) != nil || file == nil {
			continue
		}
		delete(file, "download_url")
		attachment["file"], _ = json.Marshal(file)
		attachments[i], _ = json.Marshal(attachment)
	}
	m.Content.Attachments = attachments
	return bridgeHash(m)
}

// Older journals stored the raw message fingerprint. Upgrade a known revision
// using its matching saved snapshot, rather than treating normalization as an
// edit or matching an arbitrary historical revision that could hide a revert.
func (e *bridgeEngine) priorNativeFingerprint(id, prior string) string {
	for i := len(e.state.Outbound) - 1; i >= 0; i-- {
		native := e.state.Outbound[i].Native
		if native != nil && native.ID == id && bridgeHash(*native) == prior {
			return bridgeNativeFingerprint(*native)
		}
	}
	return prior
}

func bridgeSendFailureRecord(err error, op string) *bridgeSendFailure {
	record := &bridgeSendFailure{Op: op}
	var wx *weixin.APIError
	if op == "sendmessage" && errors.As(err, &wx) {
		record.HTTPStatus, record.Ret, record.ErrCode = wx.HTTPStatus, wx.Ret, wx.ErrCode
		switch wx.Reason {
		case "", weixin.APIReasonRateLimited, weixin.APIReasonPrepareFailed, weixin.APIReasonEmpty, weixin.APIReasonOther:
			record.Reason = wx.Reason
		default:
			record.Reason = weixin.APIReasonOther
		}
	}
	var native *chatgpt.HTTPError
	if op == "native_sendmessage" && errors.As(err, &native) {
		record.HTTPStatus = native.Status
	}
	return record
}

func bridgeSendFailureValid(record *bridgeSendFailure, op string) bool {
	if record == nil {
		return true
	}
	switch record.Reason {
	case "", weixin.APIReasonRateLimited, weixin.APIReasonPrepareFailed, weixin.APIReasonEmpty, weixin.APIReasonOther:
		return record.Op == op && (record.HTTPStatus == 0 || (record.HTTPStatus >= 100 && record.HTTPStatus <= 599))
	default:
		return false
	}
}

func bridgeRefreshedNativeCapability(before, after chatgpt.Message) bool {
	if len(before.Content.Attachments) != len(after.Content.Attachments) {
		return false
	}
	capability := func(raw json.RawMessage) string {
		var ref struct {
			Type string `json:"type"`
			File struct {
				URL string `json:"download_url"`
			} `json:"file"`
		}
		if json.Unmarshal(raw, &ref) != nil || ref.Type != "file" {
			return ""
		}
		return ref.File.URL
	}
	for i, raw := range before.Content.Attachments {
		previous, current := capability(raw), capability(after.Content.Attachments[i])
		if previous != "" && current != "" && previous != current {
			return true
		}
	}
	return false
}

func bridgeMetadataOnlyUpdate(out, acknowledged *bridgeOutbound, s *bridgeState) bool {
	if out == nil || acknowledged == nil || out.Media != nil || out.Native == nil || acknowledged.Native == nil ||
		acknowledged.Status != "acknowledged" || acknowledged.ContextToken == "" || acknowledged.WeixinID == "" ||
		out.Native.ID != acknowledged.Native.ID || out.Native.Sender != s.DotSender || acknowledged.Native.Sender != s.DotSender ||
		out.Native.Content.Text == "" || out.Text != "dot 更新：\n"+out.Native.Content.Text ||
		(out.Key != "native:"+out.Native.ID+":"+bridgeHash(*out.Native) && out.Key != "native:"+out.Native.ID+":"+bridgeNativeFingerprint(*out.Native)) {
		return false
	}
	return bridgeRefreshedNativeCapability(*acknowledged.Native, *out.Native) && bridgeNativeFingerprint(*out.Native) == bridgeNativeFingerprint(*acknowledged.Native)
}

func bridgeMetadataSupersessionValid(out *bridgeOutbound, s *bridgeState) bool {
	info := out.MetadataSupersession
	if info == nil || !((info.PreviousStatus == "ready" && out.Attempts == 0) || (info.PreviousStatus == "rejected" && out.Attempts > 0)) {
		return false
	}
	for _, acknowledged := range s.Outbound {
		if acknowledged.Key == out.Key {
			break // A later receipt cannot retire an earlier revision.
		}
		if acknowledged.Key == info.AcknowledgedKey && bridgeMetadataOnlyUpdate(out, acknowledged, s) {
			return true
		}
	}
	return false
}

func (e *bridgeEngine) retireMetadataOnlyUpdates() error {
	for _, out := range e.state.Outbound {
		if !bridgeOutboundValid(out, e.state) {
			return errors.New("outbound journal validation failed before metadata recovery")
		}
	}
	dirty := false
	for i, out := range e.state.Outbound {
		if !((out.Status == "ready" && out.Attempts == 0) || (out.Status == "rejected" && out.Attempts > 0)) {
			continue
		}
		for _, acknowledged := range e.state.Outbound[:i] {
			if bridgeMetadataOnlyUpdate(out, acknowledged, e.state) {
				out.MetadataSupersession = &bridgeMetadataSupersession{PreviousStatus: out.Status, AcknowledgedKey: acknowledged.Key}
				out.Status = "superseded_metadata_only"
				dirty = true
				break
			}
		}
	}
	if dirty {
		e.event("metadata_only_updates_superseded")
		return e.store()
	}
	return nil
}

func bridgeTextSHA(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

func (e *bridgeEngine) store() error {
	if err := e.persist(e.state); err != nil {
		return errors.New("private bridge journal could not be saved; delivery stopped")
	}
	return nil
}

func (e *bridgeEngine) event(stage string) {
	if e.log != nil {
		e.log(stage)
	}
}

func bridgeInboundIdentity(m weixin.Message) string {
	if m.MessageID != "" {
		return "message:" + string(m.MessageID)
	}
	if m.Seq != "" {
		return "sequence:" + m.Seq.String()
	}
	return ""
}

func bridgeInboundKey(m weixin.Message) string {
	// A renewed context token is not a new user message. Preserve edits as
	// separate revisions, while exact replayed messages are deduplicated.
	id := bridgeInboundIdentity(m)
	if id == "" {
		id = "unidentified"
	}
	return id + ":" + bridgeHash(struct {
		Items   []weixin.MessageItem
		Updated json.Number
		Deleted json.Number
		State   int
	}{m.ItemList, m.UpdateTimeMS, m.DeleteTimeMS, m.MessageState})
}

func bridgeText(m weixin.Message) (string, bool) {
	var text []string
	if len(m.ItemList) == 0 {
		return "", false
	}
	for _, item := range m.ItemList {
		if item.Type != weixin.TextItemType || item.TextItem == nil || item.RefMessage != nil ||
			len(item.ImageItem)+len(item.VoiceItem)+len(item.FileItem)+len(item.VideoItem)+len(item.ToolCallStart)+len(item.ToolCallResult) != 0 {
			return "", false
		}
		text = append(text, item.TextItem.Text)
	}
	joined := strings.Join(text, "\n")
	return joined, strings.TrimSpace(joined) != ""
}

func bridgeRetryDelay(attempts int) time.Duration {
	delay := 3 * time.Second
	for i := 1; i < attempts && delay < time.Minute; i++ {
		delay *= 2
	}
	if delay > time.Minute {
		delay = time.Minute
	}
	return delay
}

func (e *bridgeEngine) queue(key, text string, native *chatgpt.Message) error {
	for _, prior := range e.state.Outbound {
		if prior.Key == key {
			return nil
		}
	}
	id, err := bridgeID()
	if err != nil {
		return err
	}
	e.state.Outbound = append(e.state.Outbound, &bridgeOutbound{
		Key: key, Text: text, Native: native, ClientID: id, Status: "ready",
	})
	return nil
}

const bridgeUnsupportedNotice = "这条消息已保留，但本次未取得可转发的正文或原件，或包含尚未接通的引用、编辑或交互。请在 dot 原界面处理该交互；附件与语音是否送达会分别报告。"

func (e *bridgeEngine) receive(updates *weixin.Updates) error {
	if updates == nil {
		return errors.New("Weixin update validation failed; reconnect required")
	}
	for _, m := range updates.Messages {
		key := bridgeInboundKey(m)
		known := false
		edited := false
		for _, prior := range e.state.Inbound {
			if prior.Key == key && prior.Message.FromUserID == m.FromUserID {
				known = true
			}
			if bridgeInboundIdentity(m) != "" && bridgeInboundIdentity(prior.Message) == bridgeInboundIdentity(m) &&
				prior.Message.FromUserID == m.FromUserID && prior.Status != "incomplete" {
				edited = true
			}
		}
		allowed := m.FromUserID == e.state.Peer && m.GroupID == "" && m.MessageType == weixin.UserMessage &&
			(m.ToUserID == "" || m.ToUserID == e.state.Bot)
		if allowed && m.ContextToken != "" {
			e.state.ContextToken = m.ContextToken
		}
		if known {
			continue
		}
		raw := m.Raw
		if len(raw) == 0 {
			raw, _ = json.Marshal(m)
		}
		in := &bridgeInbound{Key: key, Message: m, Raw: raw, Status: "received"}
		e.state.Inbound = append(e.state.Inbound, in)
		switch {
		case !allowed:
			in.Status = "ignored_peer_or_echo"
		case m.MessageState != weixin.FinishedState:
			in.Status = "incomplete"
		case bridgeInboundIdentity(m) == "" || edited || (m.DeleteTimeMS != "" && m.DeleteTimeMS != "0"):
			in.Status = "unsupported"
			if err := e.queue("unsupported:"+key, bridgeUnsupportedNotice, nil); err != nil {
				return err
			}
		default:
			_, items, ok := bridgePayload(m)
			if !ok {
				in.Status = "unsupported"
				if err := e.queue("unsupported:"+key, bridgeUnsupportedNotice, nil); err != nil {
					return err
				}
			} else if len(items) != 0 {
				var err error
				in.Media, err = bridgeInitIncomingMedia(items)
				if err != nil {
					return err
				}
			}
		}
	}
	// Two atomic commits are intentional: a failed cursor commit leaves durable
	// messages and the old cursor. Replayed updates are safe on recovery.
	if err := e.store(); err != nil {
		return err
	}
	if updates.Cursor != "" {
		e.state.Cursor = updates.Cursor
	}
	if err := e.store(); err != nil {
		return err
	}
	e.event("weixin_updates_persisted")
	return nil
}

func bridgeNativeFatal(err error) bool {
	var h *chatgpt.HTTPError
	if errors.As(err, &h) {
		return h.Status == 401 || h.Status == 403 || h.Status == 404 ||
			(h.Status >= 400 && h.Status < 500 && h.Status != 408 && h.Status != 429)
	}
	// Only the known transport categories are temporary. All other non-HTTP
	// client failures include schema, room, renewal and identity validation.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	s := err.Error()
	return !strings.Contains(s, "transport failed") && !strings.Contains(s, "session renewal failed")
}

func bridgeWeixinFatal(err error) bool {
	if errors.Is(err, weixin.ErrSessionExpired) || errors.Is(err, weixin.ErrCredentialsMissing) ||
		errors.Is(err, weixin.ErrUnsafeURL) || errors.Is(err, weixin.ErrInvalidRequest) || errors.Is(err, weixin.ErrProtocol) {
		return true
	}
	var h *weixin.APIError
	return errors.As(err, &h) && h.HTTPStatus >= 400 && h.HTTPStatus < 500 && h.HTTPStatus != 408 && h.HTTPStatus != 429
}

func (e *bridgeEngine) deliverNative(ctx context.Context) error {
	for _, in := range e.state.Inbound {
		if ctx.Err() != nil {
			return nil
		}
		if !bridgeInboundValid(in, e.state) {
			return errors.New("inbound journal identity or payload validation failed; delivery stopped")
		}
		if in.Status == "attempting" || in.Status == "unknown" {
			if time.Now().Before(e.reconcileAfter) {
				return nil
			}
			e.reconcileAfter = time.Now().Add(3 * time.Second)
			text, attachments := bridgeInboundNativePayload(in)
			m, err := e.lookup(ctx, receipt{Room: e.state.Room, Dot: e.state.Dot, Baseline: in.Baseline,
				RequestID: in.RequestID, TextSHA256: bridgeTextSHA(text)})
			if err != nil {
				if bridgeNativeFatal(err) {
					return errors.New("dot reconciliation authentication or validation failed; reconnect required")
				}
				e.event("dot_reconciliation_pending")
				return nil
			}
			if m == nil {
				if in.Status != "unknown" {
					in.Status = "unknown"
					if err := e.store(); err != nil {
						return err
					}
					e.event("dot_send_unknown_retained")
				}
				return nil
			}
			if m.ID == "" || m.RequestID != in.RequestID || bridgeTextSHA(m.Content.Text) != bridgeTextSHA(text) || !bridgeNativeFilesMatch(m, attachments) {
				return errors.New("dot reconciliation identity mismatch; reconnect required")
			}
			in.Status, in.NativeID = "acknowledged", m.ID
			e.reconcileAfter = time.Time{}
			if err := e.store(); err != nil {
				return err
			}
			if err := e.cleanupAcknowledgedMedia(); err != nil {
				return err
			}
			e.event("dot_send_reconciled")
		}
		if in.Status != "received" && in.Status != "retry" {
			continue
		}
		if time.Now().Before(in.RetryAt) {
			return nil
		}
		if len(in.Media) != 0 {
			ready, err := e.ensureIncomingMedia(ctx, in)
			if err != nil {
				return err
			}
			if !ready {
				continue
			}
		}
		text, attachments := bridgeInboundNativePayload(in)
		_, _, ok := bridgePayload(in.Message)
		if !ok {
			return errors.New("stored Weixin text validation failed; delivery stopped")
		}
		if in.RequestID == "" {
			id, err := bridgeID()
			if err != nil {
				return err
			}
			in.RequestID, in.Baseline = id, e.state.DotAfter
		}
		in.Status = "attempting"
		in.Attempts++
		if err := e.store(); err != nil {
			return err
		}
		m, err := e.native.Send(ctx, e.state.Room, text, in.RequestID, attachments)
		if err != nil {
			in.LastFailure = bridgeSendFailureRecord(err, "native_sendmessage")
			in.Status = "unknown"
			var h *chatgpt.HTTPError
			if errors.As(err, &h) && h.Status >= 400 && h.Status < 500 && h.Status != 408 {
				in.Status = "rejected"
				if h.Status == 429 || h.Status == 401 || h.Status == 403 {
					in.Status = "retry"
					in.RetryAt = time.Now().Add(bridgeRetryDelay(in.Attempts))
				}
			}
			if saveErr := e.store(); saveErr != nil {
				return saveErr
			}
			if bridgeNativeFatal(err) {
				return errors.New("dot sending authentication or validation failed; reconnect required")
			}
			e.event("dot_send_" + in.Status)
			return nil
		}
		if m == nil || m.ID == "" || m.RequestID != in.RequestID || (len(attachments) != 0 && !bridgeNativeFilesMatch(m, attachments)) {
			in.Status = "unknown"
			if err := e.store(); err != nil {
				return err
			}
			e.event("dot_send_unknown_retained")
			return nil
		}
		in.Status, in.NativeID = "acknowledged", m.ID
		in.RetryAt = time.Time{}
		if err := e.store(); err != nil {
			return err
		}
		if err := e.cleanupAcknowledgedMedia(); err != nil {
			return err
		}
		e.event("dot_send_acknowledged")
	}
	return nil
}

func bridgeNativeText(m chatgpt.Message, edited bool) string {
	if m.Deleted != nil {
		return "dot 已删除先前的一条消息。微信中的原消息暂无法撤回，请以 dot 原界面为准。"
	}
	text := m.Content.Text
	if edited {
		text = "dot 更新：\n" + text
	}
	if len(m.Content.Attachments) > 0 {
		if text != "" {
			text += "\n\n"
		}
		text += "此消息含尚未接入的原生文件或交互卡片，请在 dot 原界面查看和操作。任务状态与原生审批不会由微信文字替代。"
	}
	if strings.TrimSpace(text) == "" {
		text = "dot 发来了一条当前微信入口无法展示的消息，记录已保留，请在 dot 原界面查看。"
	}
	return text
}

func (e *bridgeEngine) syncNative(ctx context.Context, review bool) error {
	if ctx.Err() != nil || time.Now().Before(e.readAfter) || (!review && time.Since(e.lastRead) < 3*time.Second) {
		return nil
	}
	e.lastRead = time.Now()
	after := e.state.DotAfter
	if review {
		after = e.state.DotBaseline
	}
	visited := map[string]bool{}
	for !visited[after] {
		visited[after] = true
		page, err := e.native.Messages(ctx, e.state.Room, after)
		if err != nil {
			if bridgeNativeFatal(err) {
				return errors.New("dot reading authentication or validation failed; reconnect required")
			}
			e.readFailures++
			e.readAfter = time.Now().Add(bridgeRetryDelay(e.readFailures))
			e.event("dot_read_pending")
			return nil
		}
		if page == nil {
			return errors.New("dot message page validation failed; reconnect required")
		}
		e.readFailures = 0
		e.readAfter = time.Time{}
		dirty := false
		for _, m := range page.Items {
			if m.ID == "" || m.Sender == "" {
				return errors.New("dot message identity validation failed; reconnect required")
			}
			fingerprint := bridgeNativeFingerprint(m)
			prior, known := e.state.NativeSeen[m.ID]
			human := bridgeHumanNative(m, e.state)
			if human && bridgeNativeHistoryUnresolved(m, e.state) {
				continue // Keep the prior fingerprint until this message's history outcome is resolved.
			}
			// Older versions marked Human messages seen before filtering them.
			// Repair those omissions during review without replaying queued work.
			repairHuman := human && !bridgeWeixinNativeEcho(m, e.state) &&
				!bridgeNativeQueued(m, fingerprint, e.state) &&
				!((!known || prior == fingerprint) && bridgeNativeInHistory(m, e.state))
			if known && prior == fingerprint && !repairHuman {
				continue
			}
			e.state.NativeSeen[m.ID] = fingerprint
			dirty = true
			if known && e.priorNativeFingerprint(m.ID, prior) == fingerprint && !repairHuman {
				continue // Normalize an existing revision without a false update.
			}
			if m.Sender != e.state.DotSender && !human {
				continue // Never mirror another account or an unverified member.
			}
			if human && bridgeWeixinNativeEcho(m, e.state) {
				continue
			}
			if human && (!known || prior == fingerprint) && bridgeNativeInHistory(m, e.state) {
				continue // Preserve the original historical delivery identity and receipt.
			}
			edited := known
			if human {
				edited = bridgeNativeHasPrior(m, e.state) || (known && prior != fingerprint)
			}
			if err := e.queueNative(m, edited, fingerprint); err != nil {
				return err
			}
		}
		if dirty {
			if err := e.store(); err != nil {
				return err
			}
		}
		next := after
		if len(page.Items) > 0 {
			next = page.Items[len(page.Items)-1].ID
		}
		if page.Next != "" {
			next = page.Next
		}
		if !review && next != after {
			e.state.DotAfter = next
			if err := e.store(); err != nil {
				return err
			}
		}
		if len(page.Items) == 0 || (len(page.Items) < 32 && page.Next == "") {
			return nil
		}
		if next == after {
			return errors.New("dot message pagination did not advance; delivery stopped")
		}
		after = next
	}
	return errors.New("dot message pagination repeated; delivery stopped")
}

func (e *bridgeEngine) deliverWeixin(ctx context.Context) error {
	if err := e.retireMetadataOnlyUpdates(); err != nil {
		return err
	}
	blockedNative := map[string]bool{}
	// Live revisions are ordered ahead of history, but must not overtake an
	// uncertain historical delivery of the same native message.
	for _, out := range e.state.Outbound {
		if bridgeIsHistory(out) && (out.Status == "unknown" || out.Status == "attempting") && bridgeOutboundValid(out, e.state) {
			blockedNative[bridgeHistoryMessageID(out, e.state)] = true
		}
	}
	liveBlocked := false
	for _, out := range bridgeOutboundDeliveryOrder(e.state.Outbound) {
		if ctx.Err() != nil {
			return nil
		}
		if !bridgeOutboundValid(out, e.state) {
			return errors.New("outbound journal identity or payload validation failed; delivery stopped")
		}
		// Queued history is retained, but normal chat never automatically spends
		// its context on backfill while a new dot response is still pending.
		if bridgeIsHistory(out) && (!e.allowHistory || liveBlocked) {
			continue
		}
		if out.Native != nil && blockedNative[out.Native.ID] {
			continue // Do not deliver an edit ahead of this message's unknown result.
		}
		if out.Status == "attempting" || out.Status == "unknown" {
			liveBlocked = liveBlocked || !bridgeIsHistory(out)
			if out.Status != "unknown" {
				out.Status = "unknown"
				if err := e.store(); err != nil {
					return err
				}
				e.event("weixin_send_unknown_retained")
			}
			if out.Native != nil {
				blockedNative[out.Native.ID] = true
			}
			if out.Media != nil {
				if err := e.mediaNotice(out.Media.ID+":send", "这条附件消息的发送结果尚未确认，原件与发送记录已保留；当前不会重复发送或报告附件已送达。"); err != nil {
					return err
				}
				if err := e.store(); err != nil {
					return err
				}
				continue
			}
			if out.Native != nil {
				continue // Other native messages can progress without replaying this one.
			}
			return nil // iLink has no verified send reconciliation API here.
		}
		if out.Status == "rejected" && !bridgeResumeContextRejection(out, e.state.ContextToken) {
			if bridgeIsHistory(out) {
				return nil // Explicitly enabled history retains its own ordering barrier.
			}
			if out.Native != nil {
				blockedNative[out.Native.ID] = true
			}
			liveBlocked = true
			continue // Preserve this rejected record without freezing unrelated live messages.
		}
		if out.Status == "acknowledged" || out.Status == "superseded_metadata_only" {
			continue
		}
		if out.Status != "ready" {
			return errors.New("outbound journal validation failed; delivery stopped")
		}
		if out.Media != nil {
			ready, err := e.ensureOutgoingMedia(ctx, out)
			if err != nil {
				return err
			}
			if !ready {
				liveBlocked = true
				continue
			}
		}
		if time.Now().Before(out.RetryAt) {
			return nil
		}
		if e.state.ContextToken == "" {
			return nil // Native messages remain queued until a peer context exists.
		}
		out.ContextToken, out.Status = e.state.ContextToken, "attempting"
		out.Attempts++
		if err := e.store(); err != nil {
			return err
		}
		var result *weixin.SendResult
		var err error
		if out.Media != nil && out.Media.Stage == "uploaded" {
			mediaClient, ok := e.weixin.(bridgeWeixinMedia)
			if !ok {
				return errors.New("Weixin media sender unavailable; delivery stopped")
			}
			result, err = mediaClient.SendMedia(ctx, weixin.SendMediaRequest{ToUserID: e.state.Peer, ContextToken: out.ContextToken, ClientID: out.ClientID, Uploaded: *out.Media.Uploaded})
		} else {
			result, err = e.weixin.SendText(ctx, weixin.SendTextRequest{ToUserID: e.state.Peer, Text: out.Text, ContextToken: out.ContextToken, ClientID: out.ClientID})
		}
		if err != nil {
			out.LastFailure = bridgeSendFailureRecord(err, "sendmessage")
			out.Status = "rejected"
			if errors.Is(err, weixin.ErrOutcomeUnknown) {
				out.Status = "unknown"
			}
			var h *weixin.APIError
			if out.Media != nil && out.Media.Stage == "uploaded" && (bridgeHTTPStatus(err) == 413 || bridgeHTTPStatus(err) == 415) {
				out.Status, out.Media.Stage = "rejected", "rejected"
				out.Media.Failure = bridgeMediaFailureRecord("sendmessage", err)
				if noticeErr := e.mediaNotice(out.Media.ID, bridgeMediaRejectionNotice("out", "sendmessage", err)); noticeErr != nil {
					return noticeErr
				}
				if saveErr := e.store(); saveErr != nil {
					return saveErr
				}
				e.event("weixin_media_message_rejected")
				return nil
			}
			if errors.Is(err, weixin.ErrSessionExpired) ||
				(errors.As(err, &h) && (h.HTTPStatus == 429 || h.HTTPStatus == 401 || h.HTTPStatus == 403)) {
				out.Status = "ready" // Explicit rejection, not unknown POST.
				out.RetryAt = time.Now().Add(bridgeRetryDelay(out.Attempts))
			}
			if saveErr := e.store(); saveErr != nil {
				return saveErr
			}
			if bridgeWeixinFatal(err) {
				return errors.New("Weixin sending authentication or validation failed; reconnect required")
			}
			e.event("weixin_send_" + out.Status)
			if out.Media != nil && out.Status == "unknown" {
				liveBlocked = true
				if out.Native != nil {
					blockedNative[out.Native.ID] = true
				}
				if noticeErr := e.mediaNotice(out.Media.ID+":send", "这条附件消息的发送结果尚未确认，原件与发送记录已保留；当前不会重复发送或报告附件已送达。"); noticeErr != nil {
					return noticeErr
				}
				if saveErr := e.store(); saveErr != nil {
					return saveErr
				}
				continue
			}
			return nil
		}
		if result == nil || result.MessageID == "" || result.ClientID != out.ClientID {
			out.Status = "unknown"
			if err := e.store(); err != nil {
				return err
			}
			e.event("weixin_send_unknown_retained")
			if out.Media != nil {
				liveBlocked = true
				if out.Native != nil {
					blockedNative[out.Native.ID] = true
				}
				if err := e.mediaNotice(out.Media.ID+":send", "这条附件消息的发送结果尚未确认，原件与发送记录已保留；当前不会重复发送或报告附件已送达。"); err != nil {
					return err
				}
				if err := e.store(); err != nil {
					return err
				}
				continue
			}
			return nil
		}
		out.Status, out.WeixinID = "acknowledged", result.MessageID
		out.RetryAt = time.Time{}
		if err := e.store(); err != nil {
			return err
		}
		if err := e.cleanupAcknowledgedMedia(); err != nil {
			return err
		}
		e.event("weixin_send_acknowledged") // ACK is not an actual receipt claim.
		if bridgeIsHistory(out) {
			return nil // Yield to inbound/live events between historical messages.
		}
	}
	return nil
}

func bridgePrivateJSON(path string, v any) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("private bridge input must be a regular file with permission 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, v) != nil {
		return errors.New("private bridge input is unreadable or invalid JSON")
	}
	return nil
}

func bridgeDotSender(room *chatgpt.Room, dot string) (string, error) {
	var sender string
	for _, member := range room.Members {
		if member.Aeon == dot && member.ID != "" {
			if sender != "" && sender != member.ID {
				return "", errors.New("dot room has ambiguous member identity")
			}
			sender = member.ID
		}
	}
	if sender == "" {
		return "", errors.New("dot room member identity is missing")
	}
	return sender, nil
}

func bridgeStateValid(s *bridgeState, c *chatgpt.Client, p chatgpt.Profile, sender string, credentials weixin.Credentials) bool {
	if (s.Version != 1 && s.Version != 2) || s.Account != c.Identity.Account || s.User != c.Identity.User || s.Dot != p.ID ||
		s.Room != p.Room || s.DotSender != sender || s.Bot != credentials.BotID || s.Peer != credentials.UserID ||
		s.NativeSeen == nil || s.Started.IsZero() {
		return false
	}
	for _, in := range s.Inbound {
		if !bridgeInboundValid(in, s) {
			return false
		}
	}
	for _, out := range s.Outbound {
		if !bridgeOutboundValid(out, s) {
			return false
		}
	}
	for id, fingerprint := range s.NativeSeen {
		decoded, err := hex.DecodeString(fingerprint)
		if id == "" || err != nil || len(decoded) != sha256.Size {
			return false
		}
	}
	return true
}

func bridgeInboundValid(in *bridgeInbound, s *bridgeState) bool {
	if in == nil || in.Key == "" || in.Key != bridgeInboundKey(in.Message) || !json.Valid(in.Raw) || in.Attempts < 0 || !bridgeSendFailureValid(in.LastFailure, "native_sendmessage") {
		return false
	}
	switch in.Status {
	case "ignored_peer_or_echo", "incomplete", "unsupported":
		return true
	case "received", "retry", "attempting", "unknown", "acknowledged", "rejected":
		_, items, payloadOK := bridgePayload(in.Message)
		if !payloadOK || len(items) != len(in.Media) || !bridgeIncomingRecordsValid(in.Media) || bridgeInboundIdentity(in.Message) == "" || in.Message.MessageState != weixin.FinishedState ||
			in.Message.FromUserID != s.Peer || in.Message.GroupID != "" || in.Message.MessageType != weixin.UserMessage ||
			(in.Message.ToUserID != "" && in.Message.ToUserID != s.Bot) {
			return false
		}
		for i, item := range items {
			if bridgeHash(item) != bridgeHash(in.Media[i].Source) {
				return false
			}
		}
		if in.Status != "received" && in.RequestID == "" {
			return false
		}
		return in.Status != "acknowledged" || in.NativeID != ""
	default:
		return false
	}
}

func bridgeOutboundValid(out *bridgeOutbound, s *bridgeState) bool {
	if out == nil || out.Key == "" || out.ClientID == "" || (strings.TrimSpace(out.Text) == "" && out.Media == nil) || out.Attempts < 0 || !bridgeOutgoingRecordValid(out.Media, s.Peer) || !bridgeSendFailureValid(out.LastFailure, "sendmessage") {
		return false
	}
	if bridgeIsHistory(out) && !bridgeHistoryEntryValid(out, s) {
		return false
	}
	if out.MetadataSupersession != nil && out.Status != "superseded_metadata_only" {
		return false
	}
	if out.Native != nil && (out.Native.ID == "" || (out.Native.Sender != s.DotSender && !bridgeHumanNative(*out.Native, s))) {
		return false
	}
	if out.Media != nil {
		if out.Native == nil {
			return false
		}
		found := false
		for _, raw := range out.Native.Content.Attachments {
			var ref chatgpt.FileReference
			if json.Unmarshal(raw, &ref) == nil && ref.Type == "file" && ref.FileID == out.Media.NativeFileID {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	switch out.Status {
	case "superseded_metadata_only":
		return bridgeMetadataSupersessionValid(out, s)
	case "ready", "rejected":
		return true
	case "attempting", "unknown":
		return out.ContextToken != ""
	case "acknowledged":
		return out.ContextToken != "" && out.WeixinID != ""
	default:
		return false
	}
}

// The lock is only lifecycle protection for this application journal. It does
// not arbitrate team resources or create another task registry.
func bridgeLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, errors.New("cannot create private bridge state directory")
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || dir.Mode().Perm()&0077 != 0 {
		return nil, errors.New("bridge state directory must be private (0700)")
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("cannot open bridge lifecycle lock")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another bridge owns this journal; delivery stopped")
	}
	return f, nil
}

func runBridge(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return errors.New("home directory unavailable")
	}
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	auth := fs.String("auth", filepath.Join(home, ".codex/auth.json"), "existing private ChatGPT authentication file")
	weixinAuth := fs.String("weixin-auth", filepath.Join(home, ".config/weixin-dot/weixin.auth.json"), "private QR-bound Weixin credentials")
	statePath := fs.String("state", filepath.Join(home, ".local/state/weixin-dot/bridge.json"), "private durable bridge journal")
	expectedRoom := fs.String("room", "", "expected native room (required)")
	expectedDot := fs.String("dot", "", "expected existing dot (required)")
	historyBackfill := fs.Bool("history-backfill", false, "explicitly enable queued history delivery; disabled for normal chat")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *expectedRoom == "" || *expectedDot == "" {
		return errors.New("bridge requires --room and --dot, with optional --auth, --weixin-auth and --state")
	}
	var credentials weixin.Credentials
	if err := bridgePrivateJSON(*weixinAuth, &credentials); err != nil {
		return err
	}
	if credentials.BotToken == "" || credentials.BotID == "" || credentials.UserID == "" {
		return errors.New("Weixin QR credentials must identify both bot and scanning peer; bind again")
	}
	w, err := weixin.NewClient(weixin.Config{}, credentials)
	if err != nil {
		return errors.New("Weixin credentials or endpoint validation failed; bind again")
	}
	lock, err := bridgeLock(*statePath)
	if err != nil {
		return err
	}
	defer lock.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connectCtx, connectCancel := context.WithTimeout(ctx, 45*time.Second)
	c, p, room, err := connect(connectCtx, *auth)
	connectCancel()
	if err != nil {
		return errors.New("dot connection authentication or room validation failed; reconnect required")
	}
	if p.Room != *expectedRoom || p.ID != *expectedDot {
		return errors.New("expected dot/room mismatch; bridge did not start")
	}
	sender, err := bridgeDotSender(room, p.ID)
	if err != nil {
		return err
	}
	if err := bridgeVerifyHumanMember(room, c.Identity.User, sender); err != nil {
		return err
	}
	var s bridgeState
	if _, err := os.Stat(*statePath); err == nil {
		if err := bridgePrivateJSON(*statePath, &s); err != nil {
			return err
		}
		if !bridgeStateValid(&s, c, p, sender, credentials) {
			return errors.New("stored bridge account/dot/room/peer identity mismatch; journal retained")
		}
		if bridgeMigrateState(&s) {
			if err := save(*statePath, &s); err != nil {
				return errors.New("cannot save migrated private bridge journal")
			}
		}
	} else if errors.Is(err, os.ErrNotExist) {
		s = bridgeState{Version: 2, Account: c.Identity.Account, User: c.Identity.User, Dot: p.ID, Room: p.Room,
			DotSender: sender, Bot: credentials.BotID, Peer: credentials.UserID, Started: time.Now().UTC(),
			DotBaseline: baseline(room), DotAfter: baseline(room), NativeSeen: map[string]string{}}
		if err := save(*statePath, &s); err != nil {
			return errors.New("cannot initialize private bridge journal")
		}
	} else {
		return errors.New("private bridge journal unavailable")
	}
	e := &bridgeEngine{state: &s, native: c, weixin: w,
		allowHistory: *historyBackfill,
		persist:      func(s *bridgeState) error { return save(*statePath, s) },
		lookup:       func(ctx context.Context, r receipt) (*chatgpt.Message, error) { return findRequest(ctx, c, r) },
		log:          func(stage string) { emit(map[string]string{"stage": stage}) },
		mediaDir:     filepath.Join(filepath.Dir(*statePath), "media"),
		mediaResults: make(chan bridgeMediaResult, 1),
	}
	if err := e.cleanupAcknowledgedMedia(); err != nil {
		return err
	}
	e.event("bridge_started_native_media_and_native_approvals_pending")
	return e.loop(ctx, func(ctx context.Context) error {
		r, err := c.Verify(ctx, p)
		if err != nil {
			if bridgeNativeFatal(err) {
				return errors.New("dot room authentication or validation failed; reconnect required")
			}
			e.event("dot_room_revalidation_pending")
			return nil
		}
		current, err := bridgeDotSender(r, p.ID)
		if err != nil || current != s.DotSender {
			return errors.New("dot member identity changed; reconnect required")
		}
		if err := bridgeVerifyHumanMember(r, c.Identity.User, current); err != nil {
			return err
		}
		return nil
	})
}

func (e *bridgeEngine) loop(ctx context.Context, verify func(context.Context) error) error {
	ctx, stopAll := context.WithCancel(ctx)
	defer func() { stopAll(); e.shutdownMedia() }()
	type updateResult struct {
		updates *weixin.Updates
		err     error
	}
	results := make(chan updateResult)
	committed := make(chan string)
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func(cursor string) {
		backoff := time.Second
		for pollCtx.Err() == nil {
			updates, err := e.weixin.GetUpdates(pollCtx, cursor)
			select {
			case results <- updateResult{updates, err}:
			case <-pollCtx.Done():
				return
			}
			select {
			case cursor = <-committed:
			case <-pollCtx.Done():
				return
			}
			if err != nil {
				timer := time.NewTimer(backoff)
				select {
				case <-timer.C:
				case <-pollCtx.Done():
					timer.Stop()
					return
				}
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			} else {
				backoff = time.Second
			}
		}
	}(e.state.Cursor)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	lastReview := time.Now()
	lastVerify := time.Now()
	for {
		select {
		case <-ctx.Done():
			e.event("bridge_stopped_journal_retained")
			return nil
		case mediaResult := <-e.mediaResults:
			if err := e.acceptMedia(mediaResult); err != nil {
				return err
			}
		case result := <-results:
			if result.err != nil {
				if bridgeWeixinFatal(result.err) {
					return errors.New("Weixin polling authentication or validation failed; reconnect required")
				}
				e.event("weixin_poll_pending")
			} else if err := e.receive(result.updates); err != nil {
				return err
			}
			select {
			case committed <- e.state.Cursor:
			case <-ctx.Done():
				return nil
			}
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(lastVerify) >= time.Minute {
			if err := verify(ctx); err != nil {
				return err
			}
			lastVerify = time.Now()
		}
		if err := e.deliverNative(ctx); err != nil {
			return err
		}
		review := time.Since(lastReview) >= 30*time.Second
		if err := e.syncNative(ctx, review); err != nil {
			return err
		}
		if review {
			lastReview = time.Now()
		}
		if err := e.deliverWeixin(ctx); err != nil {
			return err
		}
	}
}
