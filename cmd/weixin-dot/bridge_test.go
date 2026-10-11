// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
	"github.com/mjmkk/weixin-dot-bridge/internal/weixin"
)

func TestBridgeSendFailureReasonWhitelistAndLegacyCompatibility(t *testing.T) {
	for _, reason := range []string{"", weixin.APIReasonRateLimited, weixin.APIReasonPrepareFailed, weixin.APIReasonEmpty, weixin.APIReasonOther, "private-error-marker"} {
		record := bridgeSendFailureRecord(&weixin.APIError{Op: "private-operation-marker", Kind: weixin.ErrAPI, Ret: -2, Reason: reason}, "sendmessage")
		want := reason
		if reason == "private-error-marker" {
			want = weixin.APIReasonOther
		}
		if record.Reason != want || record.Op != "sendmessage" || record.Ret != -2 || !bridgeSendFailureValid(record, "sendmessage") {
			t.Fatal("safe reason or numeric protocol facts were lost")
		}
		encoded, err := json.Marshal(record)
		if err != nil || strings.Contains(string(encoded), "private-error-marker") || strings.Contains(string(encoded), "private-operation-marker") {
			t.Fatal("untrusted error data entered the failure journal")
		}
		var restored bridgeSendFailure
		if json.Unmarshal(encoded, &restored) != nil || restored != *record || !bridgeSendFailureValid(&restored, "sendmessage") {
			t.Fatal("reason failure record did not survive journal serialization")
		}
		out := &bridgeOutbound{Status: "rejected", ContextToken: "old-context", LastFailure: &restored}
		if !bridgeResumeContextRejection(out, "new-context") || out.Status != "ready" {
			t.Fatal("diagnostic reason changed the existing known-rejection renewal rule")
		}
		out.Status = "rejected"
		if bridgeResumeContextRejection(out, "old-context") {
			t.Fatal("diagnostic reason enabled a same-context retry")
		}
	}
	var legacy bridgeSendFailure
	if json.Unmarshal([]byte(`{"op":"sendmessage","ret":-2}`), &legacy) != nil || legacy.Reason != "" || !bridgeSendFailureValid(&legacy, "sendmessage") {
		t.Fatal("legacy failure records without a reason were rejected")
	}
	legacy.Reason = "private-error-marker"
	if bridgeSendFailureValid(&legacy, "sendmessage") {
		t.Fatal("journal accepted a non-enum reason")
	}
}

type bridgeNativeFake struct {
	pages      []*chatgpt.Page
	reads      []string
	sends      []string
	sendErr    error
	readErr    error
	beforeSend func(string)
}

func (f *bridgeNativeFake) Messages(_ context.Context, _ string, after string) (*chatgpt.Page, error) {
	f.reads = append(f.reads, after)
	if f.readErr != nil {
		return nil, f.readErr
	}
	if len(f.pages) == 0 {
		return &chatgpt.Page{}, nil
	}
	p := f.pages[0]
	f.pages = f.pages[1:]
	return p, nil
}

func (f *bridgeNativeFake) Send(_ context.Context, _ string, _ string, request string, _ []chatgpt.FileReference) (*chatgpt.Message, error) {
	if f.beforeSend != nil {
		f.beforeSend(request)
	}
	f.sends = append(f.sends, request)
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &chatgpt.Message{ID: "remote-" + request, RequestID: request}, nil
}

type bridgeWeixinFake struct {
	sends      []weixin.SendTextRequest
	err        error
	beforeSend func(weixin.SendTextRequest)
}

func (f *bridgeWeixinFake) GetUpdates(ctx context.Context, _ string) (*weixin.Updates, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *bridgeWeixinFake) SendText(_ context.Context, input weixin.SendTextRequest) (*weixin.SendResult, error) {
	if f.beforeSend != nil {
		f.beforeSend(input)
	}
	f.sends = append(f.sends, input)
	if f.err != nil {
		return nil, f.err
	}
	return &weixin.SendResult{MessageID: "wx-remote", ClientID: input.ClientID}, nil
}

func bridgeTestEngine() (*bridgeEngine, *bridgeNativeFake, *bridgeWeixinFake) {
	n, w := &bridgeNativeFake{}, &bridgeWeixinFake{}
	e := &bridgeEngine{
		state: &bridgeState{Version: 1, Room: "room", Dot: "dot", DotSender: "dot-member", Bot: "bot", Peer: "peer",
			Cursor: "old-cursor", DotBaseline: "native-baseline", DotAfter: "native-baseline", NativeSeen: map[string]string{}},
		native: n, weixin: w, persist: func(*bridgeState) error { return nil },
		lookup: func(context.Context, receipt) (*chatgpt.Message, error) { return nil, nil },
	}
	return e, n, w
}

func bridgeTestText(id, peer, text, token string) weixin.Message {
	return weixin.Message{MessageID: weixin.ID(id), FromUserID: peer, ToUserID: "bot", MessageType: weixin.UserMessage,
		MessageState: weixin.FinishedState, ContextToken: token,
		ItemList: []weixin.MessageItem{{Type: weixin.TextItemType, TextItem: &weixin.TextItem{Text: text}}}}
}

func bridgeClone(t *testing.T, s *bridgeState) *bridgeState {
	t.Helper()
	d, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out bridgeState
	if err := json.Unmarshal(d, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestBridgeDurableUpdatesBeforeCursorAndPeerIsolation(t *testing.T) {
	e, n, _ := bridgeTestEngine()
	var committed []*bridgeState
	e.persist = func(s *bridgeState) error { committed = append(committed, bridgeClone(t, s)); return nil }
	peer := bridgeTestText("1", "peer", "private message", "private-context")
	foreign := bridgeTestText("2", "stranger", "foreign content", "foreign-context")
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{peer, foreign}, Cursor: "new-cursor"}); err != nil {
		t.Fatal(err)
	}
	if len(committed) != 2 || committed[0].Cursor != "old-cursor" || committed[1].Cursor != "new-cursor" ||
		len(committed[0].Inbound) != 2 || committed[0].ContextToken != "private-context" {
		t.Fatal("messages/context were not committed before cursor, or foreign peer replaced context")
	}
	if len(e.state.Inbound[0].Raw) == 0 || e.state.Inbound[1].Status != "ignored_peer_or_echo" {
		t.Fatal("raw input/foreign-peer disposition was not retained")
	}
	peer.ContextToken = "renewed-context"
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{peer}, Cursor: "new-cursor"}); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Inbound) != 2 || e.state.ContextToken != "renewed-context" {
		t.Fatal("replay duplicated input or failed to preserve a renewed reply token")
	}
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.sends) != 1 {
		t.Fatal("foreign/replayed message entered dot")
	}
}

func TestBridgeFailedMessageCommitDoesNotAdvanceCursorOrSend(t *testing.T) {
	e, n, _ := bridgeTestEngine()
	e.persist = func(*bridgeState) error { return errors.New("disk failure") }
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestText("1", "peer", "hello", "context")}, Cursor: "new"}); err == nil {
		t.Fatal("failed journal write was accepted")
	}
	if e.state.Cursor != "old-cursor" || len(n.sends) != 0 {
		t.Fatal("uncommitted input advanced cursor or caused a POST")
	}
}

func TestBridgeUnknownDotSendReconcilesWithoutRepeat(t *testing.T) {
	e, n, _ := bridgeTestEngine()
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{
		bridgeTestText("1", "peer", "first", "context"), bridgeTestText("2", "peer", "second", "context"),
	}}); err != nil {
		t.Fatal(err)
	}
	var durable *bridgeState
	e.persist = func(s *bridgeState) error { durable = bridgeClone(t, s); return nil }
	n.beforeSend = func(request string) {
		if durable.Inbound[0].Status != "attempting" || durable.Inbound[0].RequestID != request {
			t.Fatal("POST happened without a durable stable request ID")
		}
	}
	n.sendErr = errors.New("ChatGPT transport failed; delivery may be unknown")
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.sends) != 1 || durable.Inbound[0].Status != "unknown" {
		t.Fatal("unknown POST was not preserved")
	}
	// Resume from a fresh deserialized journal, as after a process crash.
	e.state = bridgeClone(t, durable)
	n.beforeSend = nil
	n.sendErr = nil
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.sends) != 1 || e.state.Inbound[1].Status != "received" {
		t.Fatal("absence during reconciliation caused a repeat or reordered later input")
	}
	e.lookup = func(_ context.Context, r receipt) (*chatgpt.Message, error) {
		if r.TextSHA256 != bridgeTextSHA("first") {
			t.Fatal("reconciliation did not bind the original text payload hash")
		}
		m := &chatgpt.Message{ID: "found-native", RequestID: r.RequestID}
		m.Content.Text = "first"
		return m, nil
	}
	e.reconcileAfter = time.Time{}
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.sends) != 2 || e.state.Inbound[0].NativeID != "found-native" || e.state.Inbound[1].Status != "acknowledged" {
		t.Fatal("reconciled original did not unblock the next message exactly once")
	}
}

func TestBridgeUnknownWeixinSendStopsAndSurvivesRecovery(t *testing.T) {
	e, _, w := bridgeTestEngine()
	e.state.ContextToken = "context"
	if err := e.queue("1", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.queue("2", "second", nil); err != nil {
		t.Fatal(err)
	}
	var durable *bridgeState
	e.persist = func(s *bridgeState) error { durable = bridgeClone(t, s); return nil }
	w.beforeSend = func(input weixin.SendTextRequest) {
		if durable.Outbound[0].Status != "attempting" || durable.Outbound[0].ClientID != input.ClientID ||
			durable.Outbound[0].ContextToken != input.ContextToken || input.ToUserID != "peer" {
			t.Fatal("Weixin POST had no durable identity/context or changed peer")
		}
	}
	w.err = weixin.ErrOutcomeUnknown
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.state = bridgeClone(t, durable)
	w.err = nil
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 1 || e.state.Outbound[0].Status != "unknown" || e.state.Outbound[1].Status != "ready" {
		t.Fatal("unknown Weixin POST was repeated or later message overtook it")
	}
}

func TestBridgeCrashBeforePostIsConservativelyUnknown(t *testing.T) {
	e, n, w := bridgeTestEngine()
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestText("1", "peer", "hello", "context")}}); err != nil {
		t.Fatal(err)
	}
	in := e.state.Inbound[0]
	in.Status, in.RequestID, in.Baseline = "attempting", "durable-request", "baseline"
	e.state.ContextToken = "context"
	e.state.Outbound = []*bridgeOutbound{{Key: "1", Status: "attempting", ClientID: "durable-client", Text: "hello", ContextToken: "context"}}
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.sends)+len(w.sends) != 0 || e.state.Inbound[0].Status != "unknown" || e.state.Outbound[0].Status != "unknown" {
		t.Fatal("crash boundary was assumed safe for automatic resend")
	}
}

func TestBridgeDamagedPendingEntriesCannotReconcileOrSend(t *testing.T) {
	e, n, w := bridgeTestEngine()
	lookups := 0
	e.lookup = func(context.Context, receipt) (*chatgpt.Message, error) {
		lookups++
		return &chatgpt.Message{ID: "accidental-match"}, nil
	}
	e.state.Inbound = []*bridgeInbound{{Status: "attempting"}}
	if err := e.deliverNative(context.Background()); err == nil || lookups != 0 || len(n.sends) != 0 {
		t.Fatal("damaged pending entry entered reconciliation or delivery")
	}
	e.state.ContextToken = "context"
	e.state.Outbound = []*bridgeOutbound{{Key: "1", Status: "ready", Text: "hello"}}
	if err := e.deliverWeixin(context.Background()); err == nil || len(w.sends) != 0 {
		t.Fatal("empty outgoing client identity caused a POST")
	}
}

func TestBridgeCancellationDoesNotBeginAnotherPost(t *testing.T) {
	e, n, w := bridgeTestEngine()
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestText("1", "peer", "hello", "context")}}); err != nil {
		t.Fatal(err)
	}
	if err := e.queue("reply", "hello", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.deliverNative(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.sends)+len(w.sends) != 0 || e.state.Inbound[0].Status != "received" || e.state.Outbound[0].Status != "ready" {
		t.Fatal("shutdown unnecessarily began a POST or damaged unsent queue state")
	}
}

func TestBridgeEmptyResponseCursorKeepsCommittedCursor(t *testing.T) {
	e, _, _ := bridgeTestEngine()
	if err := e.receive(&weixin.Updates{}); err != nil {
		t.Fatal(err)
	}
	if e.state.Cursor != "old-cursor" {
		t.Fatal("empty response cursor reset the committed receive position")
	}
}

func TestBridgeNativeMultiMessageEditsAndEchoSuppression(t *testing.T) {
	e, n, _ := bridgeTestEngine()
	m1 := chatgpt.Message{ID: "dot-1", Sender: "dot-member"}
	m1.Content.Text = "first"
	m2 := chatgpt.Message{ID: "dot-2", Sender: "dot-member"}
	m2.Content.Text = "second"
	self := chatgpt.Message{ID: "own-1", Sender: "account-human", RequestID: "our-request"}
	self.Content.Text = "input"
	n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{self, m1, m2}}}
	if err := e.syncNative(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 2 || e.state.Outbound[0].Text != "first" || e.state.Outbound[1].Text != "second" ||
		e.state.DotAfter != "dot-2" || n.reads[0] != "native-baseline" {
		t.Fatal("native message sequence/baseline/filtering failed")
	}
	n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{self, m1, m2}}}
	if err := e.syncNative(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 2 {
		t.Fatal("REST review repeated already observed messages")
	}
	m1.Content.Text = "revised"
	m1.Updated = time.Now().UTC()
	n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{self, m1, m2}}}
	if err := e.syncNative(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 3 || e.state.Outbound[2].Text != "dot 更新：\nrevised" || e.state.DotAfter != "dot-2" {
		t.Fatal("edit was lost, unlabeled or moved the forward cursor")
	}
}

func TestBridgeUnsupportedMediaPreservesInputAndNeverCallsDot(t *testing.T) {
	e, n, _ := bridgeTestEngine()
	m := bridgeTestText("media", "peer", "read this file", "context")
	m.ItemList = append(m.ItemList, weixin.MessageItem{Type: weixin.VoiceItemType, VoiceItem: json.RawMessage(`{"transcription":"private voice"}`)})
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{m}}); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.sends) != 0 || e.state.Inbound[0].Status != "unsupported" || len(e.state.Inbound[0].Raw) == 0 ||
		len(e.state.Outbound) != 1 || !strings.Contains(e.state.Outbound[0].Text, "已保留") || !strings.Contains(e.state.Outbound[0].Text, "未取得可转发") || strings.Contains(e.state.Outbound[0].Text, "不能把附件") {
		t.Fatal("unsupported media was dropped or falsely delivered")
	}
	card := chatgpt.Message{ID: "card", Sender: "dot-member"}
	card.Content.Attachments = []json.RawMessage{json.RawMessage(`{"type":"thread","thread_id":"task"}`)}
	if text := bridgeNativeText(card, false); !strings.Contains(text, "原生审批不会由微信文字替代") {
		t.Fatal("native card was presented as a functional text approval")
	}
}

func TestBridgeZeroDeleteTimeIsNotDeletionAndFinalUpdateIsAccepted(t *testing.T) {
	e, n, _ := bridgeTestEngine()
	m := bridgeTestText("1", "peer", "complete", "context")
	m.DeleteTimeMS = "0"
	m.MessageState = 1
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{m}}); err != nil {
		t.Fatal(err)
	}
	m.MessageState = weixin.FinishedState
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{m}}); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.sends) != 1 || e.state.Inbound[1].Status != "acknowledged" {
		t.Fatal("partial update prevented a final text from being forwarded")
	}
}

func TestBridgeAuthenticationRejectStopsWithoutUnknownOrImmediateRetry(t *testing.T) {
	e, n, w := bridgeTestEngine()
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestText("1", "peer", "hello", "context")}}); err != nil {
		t.Fatal(err)
	}
	n.sendErr = &chatgpt.HTTPError{Status: 401}
	if err := e.deliverNative(context.Background()); err == nil || e.state.Inbound[0].Status != "retry" {
		t.Fatal("dot auth rejection was not stopped and retained for reconnect")
	}
	n.sendErr = nil
	if err := e.deliverNative(context.Background()); err != nil || len(n.sends) != 1 {
		t.Fatal("known rejection was retried without backoff")
	}
	if err := e.queue("response", "hello", nil); err != nil {
		t.Fatal(err)
	}
	w.err = &weixin.APIError{Kind: weixin.ErrSessionExpired, ErrCode: -14}
	if err := e.deliverWeixin(context.Background()); err == nil || e.state.Outbound[0].Status != "ready" {
		t.Fatal("Weixin auth rejection was not stopped and retained for rebind")
	}
}

func TestBridgePrivateJournalAndSingleProcessOwnership(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "bridge.json")
	f, err := bridgeLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if second, err := bridgeLock(path); err == nil {
		second.Close()
		t.Fatal("second process can own the same delivery journal")
	}
	e, _, _ := bridgeTestEngine()
	if err := save(path, e.state); err != nil {
		t.Fatal(err)
	}
	var loaded bridgeState
	if err := bridgePrivateJSON(path, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Peer != "peer" {
		t.Fatal("private journal read failed")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := bridgePrivateJSON(path, &loaded); err == nil {
		t.Fatal("publicly readable journal was accepted")
	}
}
