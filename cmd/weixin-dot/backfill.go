// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
	"github.com/mjmkk/weixin-dot-bridge/internal/weixin"
)

type backfillManifest struct {
	Status               string `json:"status"`
	Room                 string `json:"room"`
	Dot                  string `json:"dot"`
	MessageCount         int    `json:"message_count"`
	AttachmentCount      int    `json:"attachment_count"`
	ElicitationCount     int    `json:"elicitation_count"`
	IdentityVerification struct {
		ProfileRoom bool `json:"primary_profile_room_match"`
		DotMember   bool `json:"unique_existing_dot_member"`
		HumanMember bool `json:"unique_authenticated_human_member"`
	} `json:"identity_verification"`
	Files []struct {
		Name   string `json:"file"`
		Size   int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

type backfillMessage struct {
	ID      string     `json:"id"`
	Sender  string     `json:"account_user_id"`
	Created string     `json:"created_at"`
	Deleted *time.Time `json:"deleted_at"`
	Content struct {
		Text        string            `json:"text"`
		Attachments []json.RawMessage `json:"attachments"`
		Elicitation json.RawMessage   `json:"elicitation"`
	} `json:"content"`
}

type backfillItem struct {
	ID, Text string
	Created  time.Time
}

type backfillSource struct {
	Manifest                                        backfillManifest
	Profile                                         chatgpt.Profile
	Human, DotSender, SHA256                        string
	Items                                           []backfillItem
	HumanCount, DotCount, Attachments, Elicitations int
}

func backfillPrivateFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("history source must be an existing private regular file")
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return nil, errors.New("history source directory must be private")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open private history source")
	}
	return f, nil
}

func backfillReadJSON(path string, dst any) error {
	f, err := backfillPrivateFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	if d.Decode(dst) != nil {
		return errors.New("invalid history source metadata")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("history metadata has trailing data")
	}
	return nil
}

// The export's raw pages and manifest are a private point-in-time source, not a
// second conversation state. Do not send signed download capabilities or native
// approval tool parameters while rendering their user-visible history.
func backfillLoad(path string) (*backfillSource, error) {
	dir := filepath.Dir(path)
	source := &backfillSource{}
	if err := backfillReadJSON(filepath.Join(dir, "manifest.json"), &source.Manifest); err != nil {
		return nil, err
	}
	if err := backfillReadJSON(filepath.Join(dir, "profile.json"), &source.Profile); err != nil {
		return nil, err
	}
	var room chatgpt.Room
	if err := backfillReadJSON(filepath.Join(dir, "room.json"), &room); err != nil {
		return nil, err
	}
	m := source.Manifest
	if m.Status != "complete" || m.Room == "" || m.Dot == "" || source.Profile.ID != m.Dot || source.Profile.Room != m.Room ||
		room.ID != m.Room || room.Aeon != m.Dot || room.Type != "DM" || room.Source != "chatgpt:messaging" ||
		!m.IdentityVerification.ProfileRoom || !m.IdentityVerification.DotMember || !m.IdentityVerification.HumanMember {
		return nil, errors.New("history source does not verify the existing native dot DM identity")
	}
	var err error
	source.DotSender, err = bridgeDotSender(&room, m.Dot)
	if err != nil {
		return nil, errors.New("history dot member identity is invalid")
	}
	for _, member := range room.Members {
		if member.ID == "" || member.ID == source.DotSender {
			continue
		}
		if source.Human != "" {
			return nil, errors.New("history source has multiple human members")
		}
		source.Human = member.ID
	}
	if source.Human == "" {
		return nil, errors.New("history source has no human member")
	}
	var expectedSHA string
	var expectedSize int64
	for _, file := range m.Files {
		if file.Name == filepath.Base(path) {
			if expectedSHA != "" {
				return nil, errors.New("history manifest repeats the source file")
			}
			expectedSHA, expectedSize = file.SHA256, file.Size
		}
	}
	digest, err := hex.DecodeString(expectedSHA)
	if err != nil || len(digest) != sha256.Size || expectedSize < 0 {
		return nil, errors.New("history source checksum metadata is missing")
	}
	f, err := backfillPrivateFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	counter := &backfillCountingReader{Reader: io.TeeReader(f, h)}
	d := json.NewDecoder(counter)
	seen := map[string]bool{}
	for {
		var raw json.RawMessage
		if err := d.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return nil, errors.New("invalid history message stream; nothing queued")
		}
		if !utf8.Valid(raw) {
			return nil, errors.New("history message is not valid UTF-8")
		}
		var message backfillMessage
		if json.Unmarshal(raw, &message) != nil || message.ID == "" || seen[message.ID] {
			return nil, errors.New("history message identity is invalid or duplicated")
		}
		var envelope struct {
			Content map[string]json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			return nil, errors.New("history message content is invalid")
		}
		for field := range envelope.Content {
			if field != "text" && field != "attachments" && field != "elicitation" {
				return nil, errors.New("history contains unsupported visible content; source retained, nothing queued")
			}
		}
		created, err := time.Parse(time.RFC3339Nano, message.Created)
		if err != nil || created.IsZero() {
			return nil, errors.New("history message lacks a valid original time")
		}
		speaker := ""
		switch message.Sender {
		case source.Human:
			speaker = "Human（你）"
			source.HumanCount++
		case source.DotSender:
			speaker = "Dot"
			source.DotCount++
		default:
			return nil, errors.New("history contains an unverified third speaker")
		}
		text, err := backfillRender(message, speaker)
		if err != nil {
			return nil, err
		}
		source.Attachments += len(message.Content.Attachments)
		if backfillPresent(message.Content.Elicitation) {
			source.Elicitations++
		}
		source.Items = append(source.Items, backfillItem{ID: message.ID, Text: text, Created: created})
		seen[message.ID] = true
	}
	source.SHA256 = hex.EncodeToString(h.Sum(nil))
	if source.SHA256 != strings.ToLower(expectedSHA) || counter.Bytes != expectedSize || len(source.Items) != m.MessageCount ||
		source.Attachments != m.AttachmentCount || source.Elicitations != m.ElicitationCount {
		return nil, errors.New("history source checksum, length or message counts do not match the completed export")
	}
	sort.SliceStable(source.Items, func(i, j int) bool { return source.Items[i].Created.Before(source.Items[j].Created) })
	return source, nil
}

type backfillCountingReader struct {
	io.Reader
	Bytes int64
}

func (r *backfillCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.Bytes += int64(n)
	return n, err
}
func backfillPresent(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

func backfillRender(m backfillMessage, speaker string) (string, error) {
	originalTime, err := backfillOriginalTime(m.Created)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "【历史补发｜%s｜原时间：%s】\n", speaker, originalTime)
	if m.Deleted != nil {
		b.WriteString("【原消息已删除；以下为当前历史记录保留的内容】\n")
	}
	b.WriteString(m.Content.Text)
	for _, raw := range m.Content.Attachments {
		text, err := backfillAttachment(raw)
		if err != nil {
			return "", err
		}
		b.WriteString("\n\n")
		b.WriteString(text)
	}
	if backfillPresent(m.Content.Elicitation) {
		var e struct {
			Status string `json:"status"`
			Params struct {
				Message string          `json:"message"`
				Schema  json.RawMessage `json:"requestedSchema"`
			} `json:"params"`
		}
		if json.Unmarshal(m.Content.Elicitation, &e) != nil {
			return "", errors.New("history native interaction display is invalid")
		}
		b.WriteString("\n\n【原生交互历史记录】\n")
		b.WriteString(e.Params.Message)
		if e.Status != "" {
			fmt.Fprintf(&b, "\n原状态：%s", e.Status)
		}
		if backfillPresent(e.Params.Schema) {
			var pretty bytes.Buffer
			if json.Indent(&pretty, e.Params.Schema, "", "  ") != nil {
				return "", errors.New("history native interaction schema is invalid")
			}
			b.WriteString("\n原交互表单：\n")
			b.WriteString(pretty.String())
		}
		b.WriteString("\n这是历史显示，不会重新执行或代替原生审批；需要继续时请在原 dot 查看：https://chatgpt.com/")
	}
	if m.Content.Text == "" && len(m.Content.Attachments) == 0 && !backfillPresent(m.Content.Elicitation) {
		b.WriteString("【该原消息无可显示正文】")
	}
	return b.String(), nil
}

func backfillOriginalTime(raw string) (string, error) {
	created, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || created.IsZero() {
		return "", errors.New("history message lacks a valid original time")
	}
	local := created.In(time.FixedZone("CST", 8*60*60))
	formatted := local.Format("2006-01-02 15:04:05")
	if dot := strings.IndexAny(raw, ".,"); dot >= 0 {
		digits := 0
		for _, c := range raw[dot+1:] {
			if c < '0' || c > '9' {
				break
			}
			digits++
		}
		if digits > 9 {
			return "", errors.New("history time has unsupported precision; nothing truncated")
		}
		if digits > 0 {
			formatted += "." + fmt.Sprintf("%09d", local.Nanosecond())[:digits]
		}
	}
	return formatted + " CST（UTC+08:00）", nil
}

func backfillAttachment(raw json.RawMessage) (string, error) {
	var a struct {
		Type         string          `json:"type"`
		FileID       string          `json:"file_id"`
		ThreadID     string          `json:"thread_id"`
		CallID       string          `json:"call_id"`
		AttachmentID string          `json:"attachment_id"`
		Product      string          `json:"product_sku"`
		Outcome      string          `json:"outcome"`
		Transcript   string          `json:"transcript_status"`
		Connected    json.RawMessage `json:"connected_at"`
		Ended        json.RawMessage `json:"ended_at"`
		Duration     json.Number     `json:"duration_ms"`
		File         struct {
			Name   string `json:"name"`
			MIME   string `json:"mime_type"`
			Status string `json:"status"`
			ID     string `json:"file_id"`
		} `json:"file"`
		Messages []struct {
			Author struct {
				Role string `json:"role"`
			} `json:"author"`
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &a) != nil || a.Type == "" {
		return "", errors.New("history attachment display is invalid")
	}
	var b strings.Builder
	switch a.Type {
	case "file":
		id := a.FileID
		if id == "" {
			id = a.File.ID
		}
		if id == "" {
			return "", errors.New("history file reference lacks its original ID")
		}
		b.WriteString("【原文件引用】\n")
		if a.File.Name != "" {
			b.WriteString(a.File.Name)
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "file_id：%s", id)
		if a.File.MIME != "" {
			fmt.Fprintf(&b, "\n类型：%s", a.File.MIME)
		}
		if a.File.Status != "" {
			fmt.Fprintf(&b, "\n原状态：%s", a.File.Status)
		}
		b.WriteString("\n原件未重新上传；请在原 dot 查看：https://chatgpt.com/")
	case "thread":
		fmt.Fprintf(&b, "【原生任务卡历史记录】\nthread_id：%s\n仅保留引用，不重新启动任务或执行审批；请在原 dot 查看：https://chatgpt.com/", a.ThreadID)
	case "widget":
		b.WriteString("【原卡片历史记录】")
		if a.Product != "" {
			fmt.Fprintf(&b, "\n%s", a.Product)
		}
		for _, message := range a.Messages {
			fmt.Fprintf(&b, "\n卡片消息（%s）：\n%s", message.Author.Role, message.Content.Text)
		}
		b.WriteString("\n原卡片交互请在原 dot 查看：https://chatgpt.com/")
	case "call":
		fmt.Fprintf(&b, "【原通话历史记录】\ncall_id：%s", a.CallID)
		if a.Outcome != "" {
			fmt.Fprintf(&b, "\n结果：%s", a.Outcome)
		}
		if a.Transcript != "" {
			fmt.Fprintf(&b, "\n转录状态：%s", a.Transcript)
		}
		if a.Duration != "" {
			fmt.Fprintf(&b, "\n时长（毫秒）：%s", a.Duration)
		}
		if backfillPresent(a.Connected) {
			fmt.Fprintf(&b, "\n接通时间：%s", a.Connected)
		}
		if backfillPresent(a.Ended) {
			fmt.Fprintf(&b, "\n结束时间：%s", a.Ended)
		}
	default:
		fmt.Fprintf(&b, "【原附件/交互引用】\n类型：%s\nattachment_id：%s\n当前无法显示其交互内容；请在原 dot 查看：https://chatgpt.com/", a.Type, a.AttachmentID)
	}
	return b.String(), nil
}

func backfillEnqueue(s *bridgeState, source *backfillSource, room, dot, peer string) (int, int, error) {
	if source == nil || s == nil || room == "" || dot == "" || peer == "" || s.Room != room || s.Dot != dot || s.Peer != peer ||
		source.Manifest.Room != room || source.Manifest.Dot != dot || s.User != source.Human || s.DotSender != source.DotSender {
		return 0, 0, errors.New("history source or target account/dot/room/peer identity mismatch; nothing queued")
	}
	// This validates an existing local journal without loading credentials or
	// making network calls. Normal bridge startup still verifies the account.
	c := &chatgpt.Client{Identity: chatgpt.Identity{Account: s.Account, User: s.User}}
	if !bridgeStateValid(s, c, source.Profile, source.DotSender, weixin.Credentials{BotID: s.Bot, UserID: peer}) {
		return 0, 0, errors.New("existing bridge journal validation failed; nothing queued")
	}
	existing := map[string]*bridgeOutbound{}
	existingMessages := map[string]*bridgeOutbound{}
	for _, out := range s.Outbound {
		existing[out.Key] = out
		if bridgeIsHistory(out) {
			id := bridgeHistoryMessageID(out, s)
			if existingMessages[id] != nil {
				return 0, 0, errors.New("existing history repeats a native message identity; nothing queued")
			}
			existingMessages[id] = out
		}
	}
	var added []*bridgeOutbound
	known := 0
	for _, item := range source.Items {
		key := "history:" + room + ":" + item.ID + ":" + bridgeTextSHA(item.Text)
		if out := existingMessages[item.ID]; out != nil && out.Key != key {
			return 0, 0, errors.New("historical message payload changed; prior receipt retained, nothing queued")
		}
		if out := existing[key]; out != nil {
			if out.Text != item.Text || !bridgeHistoryEntryValid(out, s) {
				return 0, 0, errors.New("existing history entry payload mismatch; nothing queued")
			}
			known++
			continue // Including ACKs and unknown outcomes: never re-POST.
		}
		id, err := bridgeID()
		if err != nil {
			return 0, 0, err
		}
		out := &bridgeOutbound{Key: key, Text: item.Text, Status: "ready", ClientID: id, DeliveryKind: "history"}
		if !bridgeOutboundValid(out, s) {
			return 0, 0, errors.New("new history entry failed journal validation")
		}
		added = append(added, out)
		existing[key] = out
		existingMessages[item.ID] = out
	}
	s.Outbound = append(s.Outbound, added...)
	return len(added), known, nil
}

func bridgeIsHistory(out *bridgeOutbound) bool { return out != nil && out.DeliveryKind == "history" }

// A renewed inbound context permits one new attempt after this exact known
// rejection for text or an already uploaded native attachment. It does not
// establish what ret=-2 means or a server quota/TTL. The caller validates the
// journal's native identity and bound peer before considering this transition.
// The engine journals the new token and attempting state before the POST, so
// another rejection freezes this entry until the context changes again.
func bridgeResumeContextRejection(out *bridgeOutbound, currentContext string) bool {
	eligible := out != nil && out.Media == nil
	if out != nil && out.Media != nil {
		media := out.Media
		eligible = out.Native != nil && out.Native.ID != "" && out.DeliveryKind == "media" &&
			media.Stage == "uploaded" && !media.Cleaned && media.Uploaded != nil && media.Uploaded.ToUserID != "" &&
			bridgeOutgoingRecordValid(media, media.Uploaded.ToUserID)
	}
	if !eligible || out.Status != "rejected" || out.LastFailure == nil ||
		out.LastFailure.Op != "sendmessage" || out.LastFailure.Ret != -2 ||
		out.LastFailure.HTTPStatus != 0 || out.LastFailure.ErrCode != 0 ||
		out.ContextToken == "" || currentContext == "" || out.ContextToken == currentContext {
		return false
	}
	out.Status = "ready"
	return true
}

func bridgeHistoryEntryValid(out *bridgeOutbound, s *bridgeState) bool {
	if !bridgeIsHistory(out) || out.Native != nil || out.Media != nil {
		return false
	}
	prefix := "history:" + s.Room + ":"
	if !strings.HasPrefix(out.Key, prefix) {
		return false
	}
	identity := strings.TrimPrefix(out.Key, prefix)
	i := strings.LastIndex(identity, ":")
	return i > 0 && identity[i+1:] == bridgeTextSHA(out.Text)
}

func bridgeHistoryMessageID(out *bridgeOutbound, s *bridgeState) string {
	identity := strings.TrimPrefix(out.Key, "history:"+s.Room+":")
	return identity[:strings.LastIndex(identity, ":")]
}

// Preserve each queue's own order. A blocked historical POST freezes the later
// history while new live messages remain ahead of it on the next engine pass.
func bridgeOutboundDeliveryOrder(entries []*bridgeOutbound) []*bridgeOutbound {
	ordered := make([]*bridgeOutbound, 0, len(entries))
	for _, out := range entries {
		if !bridgeIsHistory(out) {
			ordered = append(ordered, out)
		}
	}
	for _, out := range entries {
		if bridgeIsHistory(out) {
			ordered = append(ordered, out)
		}
	}
	return ordered
}

func runBackfill(args []string) error {
	if len(args) == 0 || (args[0] != "enqueue" && args[0] != "inspect") {
		return errors.New("usage: weixin-dot backfill inspect|enqueue --source private/messages.jsonl")
	}
	fs := flag.NewFlagSet("backfill "+args[0], flag.ContinueOnError)
	sourcePath := fs.String("source", "", "completed private native history messages.jsonl")
	statePath := fs.String("state", "", "existing private bridge journal (enqueue only)")
	room := fs.String("room", "", "expected existing native room")
	dot := fs.String("dot", "", "expected existing dot")
	peer := fs.String("peer", "", "expected existing Weixin peer")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *sourcePath == "" {
		return errors.New("backfill requires --source and no positional arguments")
	}
	source, err := backfillLoad(*sourcePath)
	if err != nil {
		return err
	}
	if args[0] == "inspect" {
		emit(map[string]any{"stage": "history_source_verified_no_delivery", "messages": len(source.Items), "human": source.HumanCount, "dot": source.DotCount, "attachment_references": source.Attachments, "native_interactions": source.Elicitations, "sha256": source.SHA256})
		return nil
	}
	if *statePath == "" || *room == "" || *dot == "" || *peer == "" {
		return errors.New("backfill enqueue requires --state --room --dot --peer")
	}
	lock, err := bridgeLock(*statePath)
	if err != nil {
		return err
	}
	defer lock.Close()
	var s bridgeState
	if err := bridgePrivateJSON(*statePath, &s); err != nil {
		return err
	}
	added, known, err := backfillEnqueue(&s, source, *room, *dot, *peer)
	if err != nil {
		return err
	}
	if added > 0 {
		if err := save(*statePath, &s); err != nil {
			return errors.New("cannot persist private history queue; no messages sent")
		}
	}
	emit(map[string]any{"stage": "history_enqueued_not_delivered", "messages": len(source.Items), "new_entries": added, "existing_entries": known})
	return nil
}
