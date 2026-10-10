// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/weixin"
)

func backfillFixture(t *testing.T, mutate func([]map[string]any)) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeJSON := func(name string, v any) {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	messages := []map[string]any{
		{"id": "dot-history", "account_user_id": "dot-member", "created_at": "2026-10-09T02:00:00.123456Z", "content": map[string]any{"text": strings.Repeat("完整正文", 18000), "attachments": []any{}}},
		{"id": "human-history", "account_user_id": "human", "created_at": "2026-10-09T01:00:00Z", "content": map[string]any{
			"text": "用户原文\n第二行",
			"attachments": []any{
				map[string]any{"type": "file", "file_id": "original-file", "file": map[string]any{"name": "原件.txt", "mime_type": "text/plain", "status": "ready", "download_url": "secret-storage-capability"}},
				map[string]any{"type": "thread", "thread_id": "original-task"},
				map[string]any{"type": "widget", "product_sku": "original-card", "messages": []any{map[string]any{"author": map[string]any{"role": "assistant"}, "content": map[string]any{"text": "卡片原文"}}}},
				map[string]any{"type": "call", "call_id": "original-call", "outcome": "ended", "duration_ms": 5000},
			},
			"elicitation": map[string]any{"status": "completed", "params": map[string]any{"message": "原来的审批问题", "requestedSchema": json.RawMessage(`{"type":"object","properties":{"amount":{"const":9007199254740993}}}`), "_meta": map[string]any{"tool_params": "secret-approval-param"}}},
		}},
	}
	if mutate != nil {
		mutate(messages)
	}
	var stream []byte
	for _, m := range messages {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		stream = append(stream, raw...)
		stream = append(stream, '\n')
	}
	path := filepath.Join(dir, "messages.jsonl")
	if err := os.WriteFile(path, stream, 0600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(stream)
	writeJSON("manifest.json", map[string]any{
		"status": "complete", "room": "room", "dot": "dot", "message_count": 2, "attachment_count": 4, "elicitation_count": 1,
		"identity_verification": map[string]any{"primary_profile_room_match": true, "unique_existing_dot_member": true, "unique_authenticated_human_member": true},
		"files":                 []any{map[string]any{"file": "messages.jsonl", "bytes": len(stream), "sha256": hex.EncodeToString(h[:])}},
	})
	writeJSON("profile.json", map[string]any{"id": "dot", "messaging_room_id": "room"})
	writeJSON("room.json", map[string]any{"id": "room", "aeon_id": "dot", "type": "DM", "app_source": "chatgpt:messaging", "members": []any{map[string]any{"account_user_id": "human"}, map[string]any{"account_user_id": "dot-member", "aeon_id": "dot"}}})
	return path
}

func backfillTestEngine(t *testing.T) (*bridgeEngine, *bridgeNativeFake, *bridgeWeixinFake) {
	t.Helper()
	e, n, w := bridgeTestEngine()
	e.state.Version = 2
	e.state.Account = "account"
	e.state.User = "human"
	e.state.Started = time.Now().UTC()
	e.state.ContextToken = "context"
	e.allowHistory = true // These tests explicitly exercise the opt-in backfill engine.
	e.mediaResults = make(chan bridgeMediaResult, 1)
	return e, n, w
}

func backfillTestSource(t *testing.T) *backfillSource {
	t.Helper()
	source, err := backfillLoad(backfillFixture(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestBackfillCompleteSourceKeepsSpeakersTimesTextAndReferences(t *testing.T) {
	source := backfillTestSource(t)
	if len(source.Items) != 2 || source.HumanCount != 1 || source.DotCount != 1 || source.Attachments != 4 || source.Elicitations != 1 || source.Items[0].ID != "human-history" {
		t.Fatal("whole history identities/counts/chronology were not retained")
	}
	text := source.Items[0].Text
	for _, expected := range []string{"Human（你）", "原时间：2026-10-09 09:00:00 CST（UTC+08:00）", "用户原文\n第二行", "原件.txt", "original-file", "原件未重新上传", "original-task", "卡片原文", "original-call", "原来的审批问题", "原状态：completed", "9007199254740993", "不会重新执行或代替原生审批"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("history display omitted %q", expected)
		}
	}
	if strings.Contains(text, "secret-storage-capability") || strings.Contains(text, "secret-approval-param") {
		t.Fatal("capability or native tool parameter entered display")
	}
	if !strings.Contains(source.Items[1].Text, strings.Repeat("完整正文", 18000)) || !strings.Contains(source.Items[1].Text, "Dot｜原时间：2026-10-09 10:00:00.123456 CST（UTC+08:00）") {
		t.Fatal("long message was limited/truncated or its original identity/time changed")
	}
}

func TestBackfillOriginalTimeKeepsPrecisionAcrossCSTDateBoundary(t *testing.T) {
	got, err := backfillOriginalTime("2026-10-09T20:00:00.120000Z")
	if err != nil || got != "2026-10-10 04:00:00.120000 CST（UTC+08:00）" {
		t.Fatal("original instant or fractional precision changed during CST display")
	}
}

func TestBackfillSourceIntegrityAndSpeakerFailuresQueueNothing(t *testing.T) {
	for _, test := range []string{"bytes", "speaker", "unhandled-visible-content"} {
		t.Run(test, func(t *testing.T) {
			var mutate func([]map[string]any)
			if test == "speaker" {
				mutate = func(m []map[string]any) { m[0]["account_user_id"] = "someone-else" }
			}
			if test == "unhandled-visible-content" {
				mutate = func(m []map[string]any) { m[0]["content"].(map[string]any)["new_visible_field"] = "not understood" }
			}
			path := backfillFixture(t, mutate)
			if test == "bytes" {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				f.WriteString("\n")
				f.Close()
			}
			if source, err := backfillLoad(path); err == nil || source != nil {
				t.Fatal("unverified/unsupported export was accepted")
			}
		})
	}
}

func TestBackfillEnqueuePreservesIdentityCursorsAndDeduplicatesEveryState(t *testing.T) {
	e, _, _ := backfillTestEngine(t)
	source := backfillTestSource(t)
	before := bridgeClone(t, e.state)
	added, known, err := backfillEnqueue(e.state, source, "room", "dot", "peer")
	if err != nil || added != 2 || known != 0 {
		t.Fatalf("enqueue failed: %d %d %v", added, known, err)
	}
	for _, out := range e.state.Outbound {
		if !bridgeOutboundValid(out, e.state) || out.Native != nil || out.Media != nil {
			t.Fatal("history entry is not a valid local text delivery")
		}
	}
	unchanged := bridgeClone(t, e.state)
	unchanged.Outbound = before.Outbound
	if bridgeHash(unchanged) != bridgeHash(before) {
		t.Fatal("history enqueue changed identity/cursors/native seen/other state")
	}
	e.state.Outbound[0].Status = "acknowledged"
	e.state.Outbound[0].ContextToken = "context"
	e.state.Outbound[0].WeixinID = "history-ack"
	e.state.Outbound[1].Status = "unknown"
	e.state.Outbound[1].ContextToken = "context"
	e.state.Outbound[1].Attempts = 1
	prior := bridgeClone(t, e.state)
	added, known, err = backfillEnqueue(e.state, source, "room", "dot", "peer")
	if err != nil || added != 0 || known != 2 || bridgeHash(e.state) != bridgeHash(prior) {
		t.Fatal("ACK/unknown history entries were duplicated or reset")
	}
	changed := *source
	changed.Items = append([]backfillItem{{ID: "new-message", Text: "【历史补发｜Dot｜原时间：2026-10-09T00:00:00Z】\nnew historical text"}}, source.Items...)
	changed.Items[1].Text += "\nchanged original payload"
	if added, _, err := backfillEnqueue(e.state, &changed, "room", "dot", "peer"); err == nil || added != 0 || bridgeHash(e.state) != bridgeHash(prior) {
		t.Fatal("same native ID with changed history payload created another delivery or partially enqueued")
	}
	if added, _, err := backfillEnqueue(e.state, source, "room", "dot", "different-peer"); err == nil || added != 0 || bridgeHash(e.state) != bridgeHash(prior) {
		t.Fatal("target mismatch changed the journal")
	}
	e.state.Outbound[0].Text = "tampered payload"
	if bridgeOutboundValid(e.state.Outbound[0], e.state) {
		t.Fatal("history key did not bind to its original text")
	}
}

func TestBackfillDeliveryPrefersLiveAndYieldsWithoutSendingToDot(t *testing.T) {
	e, n, w := backfillTestEngine(t)
	if _, _, err := backfillEnqueue(e.state, backfillTestSource(t), "room", "dot", "peer"); err != nil {
		t.Fatal(err)
	}
	if err := e.queue("live-response", "current live reply", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 2 || w.sends[0].Text != "current live reply" || !strings.Contains(w.sends[1].Text, "Human（你）") || e.state.Outbound[1].Status != "ready" || len(n.sends) != 0 {
		t.Fatal("history starved live traffic, failed to yield, or looped into native dot")
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 3 || !strings.Contains(w.sends[2].Text, "Dot｜") || len(n.sends) != 0 {
		t.Fatal("next historical message did not resume in original order")
	}
}

func TestBackfillBlockedHistoryFreezesFollowingHistoryButAllowsLive(t *testing.T) {
	for _, status := range []string{"unknown", "attempting", "rejected", "backoff"} {
		t.Run(status, func(t *testing.T) {
			e, _, w := backfillTestEngine(t)
			if _, _, err := backfillEnqueue(e.state, backfillTestSource(t), "room", "dot", "peer"); err != nil {
				t.Fatal(err)
			}
			out := e.state.Outbound[0]
			out.Status = status
			out.ContextToken = "context"
			out.Attempts = 1
			if status == "backoff" {
				out.Status = "ready"
				out.RetryAt = time.Now().Add(time.Minute)
			}
			if err := e.queue("live", "new live message", nil); err != nil {
				t.Fatal(err)
			}
			if err := e.deliverWeixin(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(w.sends) != 1 || w.sends[0].Text != "new live message" || e.state.Outbound[1].Status != "ready" || out.Attempts != 1 {
				t.Fatal("historical barrier was retried, overtaken by history, or blocked live")
			}
			if status == "attempting" && out.Status != "unknown" {
				t.Fatal("crashed historical POST was not retained as unknown")
			}
		})
	}
}

func TestBackfillActual429IsJournalledAndNotImmediatelyRetried(t *testing.T) {
	e, _, w := backfillTestEngine(t)
	if _, _, err := backfillEnqueue(e.state, backfillTestSource(t), "room", "dot", "peer"); err != nil {
		t.Fatal(err)
	}
	w.err = &weixin.APIError{Op: "sendmessage", Kind: weixin.ErrAPI, HTTPStatus: 429, Ret: 12, ErrCode: 34}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := e.state.Outbound[0]
	if out.Status != "ready" || out.Attempts != 1 || !out.RetryAt.After(time.Now()) || out.LastFailure == nil || out.LastFailure.HTTPStatus != 429 || out.LastFailure.Ret != 12 || out.LastFailure.ErrCode != 34 {
		t.Fatal("actual quota rejection was not safely retained/backed off")
	}
	w.err = nil
	if err := e.queue("live", "current live message", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 2 || w.sends[1].Text != "current live message" || out.Attempts != 1 || e.state.Outbound[1].Status != "ready" {
		t.Fatal("backfill retry was immediate or bypassed history order")
	}
}

func TestBackfillKnownRejectionResumesOncePerRenewedContext(t *testing.T) {
	e, n, w := backfillTestEngine(t)
	if _, _, err := backfillEnqueue(e.state, backfillTestSource(t), "room", "dot", "peer"); err != nil {
		t.Fatal(err)
	}
	w.err = &weixin.APIError{Op: "sendmessage", Kind: weixin.ErrAPI, Ret: -2}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := e.state.Outbound[0]
	key, text, clientID := out.Key, out.Text, out.ClientID
	failure := *out.LastFailure
	if len(w.sends) != 1 || out.Status != "rejected" || out.Attempts != 1 {
		t.Fatal("initial known rejection was not retained")
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 1 {
		t.Fatal("same context retried a rejected historical POST")
	}
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestText("renewal-1", "peer", "new inbound", "renewed-context")}}); err != nil {
		t.Fatal(err)
	}
	var committed *bridgeState
	e.persist = func(s *bridgeState) error { committed = bridgeClone(t, s); return nil }
	w.beforeSend = func(input weixin.SendTextRequest) {
		if committed == nil || committed.Outbound[0].Status != "attempting" || committed.Outbound[0].Attempts != 2 ||
			committed.Outbound[0].ContextToken != "renewed-context" || *committed.Outbound[0].LastFailure != failure ||
			input.ClientID != clientID || input.Text != text || input.ContextToken != "renewed-context" {
			t.Fatal("renewed attempt was not durably bound to the same payload/client ID before POST")
		}
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 2 || out.Status != "rejected" || out.Attempts != 2 || out.Key != key || out.Text != text || out.ClientID != clientID ||
		out.ContextToken != "renewed-context" || *out.LastFailure != failure || e.state.Outbound[1].Status != "ready" || len(n.sends) != 0 {
		t.Fatal("renewed rejection changed identity, lost evidence, overtook history, or looped into dot")
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 2 || out.Attempts != 2 {
		t.Fatal("another rejection was retried with its same context")
	}
	w.beforeSend = nil
	w.err = nil
	if err := e.receive(&weixin.Updates{Messages: []weixin.Message{bridgeTestText("renewal-2", "peer", "another inbound", "second-renewed-context")}}); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 3 || out.Status != "acknowledged" || out.Attempts != 3 || out.Key != key || out.Text != text || out.ClientID != clientID ||
		*out.LastFailure != failure || e.state.Outbound[1].Status != "ready" || w.sends[2].ClientID != clientID {
		t.Fatal("second renewal did not complete the same historical delivery while preserving its failures")
	}
}

func TestBackfillRenewedContextDoesNotResumeOtherBlockedEntries(t *testing.T) {
	for _, test := range []string{"unknown", "attempting", "other-ret", "http-status", "errcode", "no-failure", "no-old-context", "no-current-context"} {
		t.Run(test, func(t *testing.T) {
			e, _, w := backfillTestEngine(t)
			if _, _, err := backfillEnqueue(e.state, backfillTestSource(t), "room", "dot", "peer"); err != nil {
				t.Fatal(err)
			}
			out := e.state.Outbound[0]
			out.Status, out.ContextToken, out.Attempts = "rejected", "old-context", 1
			out.LastFailure = &bridgeSendFailure{Op: "sendmessage", Ret: -2}
			e.state.ContextToken = "new-context"
			switch test {
			case "unknown", "attempting":
				out.Status = test
			case "other-ret":
				out.LastFailure.Ret = -3
			case "http-status":
				out.LastFailure.HTTPStatus = 400
			case "errcode":
				out.LastFailure.ErrCode = 4
			case "no-failure":
				out.LastFailure = nil
			case "no-old-context":
				out.ContextToken = ""
			case "no-current-context":
				e.state.ContextToken = ""
			}
			if err := e.deliverWeixin(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(w.sends) != 0 || out.Attempts != 1 || e.state.Outbound[1].Status != "ready" {
				t.Fatal("renewed context retried another blocked entry or bypassed the history barrier")
			}
			if test == "unknown" && out.Status != "unknown" {
				t.Fatal("unknown historical result was changed")
			}
			if test == "attempting" && out.Status != "unknown" {
				t.Fatal("crashed historical POST was not retained as unknown")
			}
		})
	}
}

func TestBackfillRenewedMediaNoticeDoesNotBlockHistory(t *testing.T) {
	e, _, w := backfillTestEngine(t)
	if _, _, err := backfillEnqueue(e.state, backfillTestSource(t), "room", "dot", "peer"); err != nil {
		t.Fatal(err)
	}
	if err := e.queue("media-notice:attachment:native_upload_attempting", "retained native upload rejection", nil); err != nil {
		t.Fatal(err)
	}
	notice := e.state.Outbound[len(e.state.Outbound)-1]
	notice.Status, notice.ContextToken, notice.Attempts = "rejected", e.state.ContextToken, 1
	notice.LastFailure = &bridgeSendFailure{Op: "sendmessage", Ret: -2}
	clientID := notice.ClientID
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 0 {
		t.Fatal("old context retried a rejected local notice")
	}
	e.state.ContextToken = "renewed-context"
	if err := e.deliverWeixin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.sends) != 2 || w.sends[0].ClientID != clientID || notice.Status != "acknowledged" || notice.Attempts != 2 || notice.LastFailure.Ret != -2 || e.state.Outbound[0].Status != "acknowledged" {
		t.Fatal("a known local notice rejection blocked history or changed its identity")
	}
}

func TestBackfillEnqueueCannotRaceOwningBridgeOrAlterItsJournal(t *testing.T) {
	e, _, _ := backfillTestEngine(t)
	sourcePath := backfillFixture(t, nil)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "bridge.json")
	if err := save(statePath, e.state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := bridgeLock(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := runBackfill([]string{"enqueue", "--source", sourcePath, "--state", statePath, "--room", "room", "--dot", "dot", "--peer", "peer"}); err == nil {
		t.Fatal("enqueue mutated a journal owned by a running bridge")
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("failed lifecycle lock changed the journal")
	}
}
