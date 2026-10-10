// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
	"github.com/mjmkk/weixin-dot-bridge/internal/weixin"
)

func nativeHumanFixture() chatgpt.Message {
	m := chatgpt.Message{ID: "human-ui-message", Sender: "human-member", RequestID: "another-client-request", Created: time.Date(2026, 10, 10, 1, 2, 3, 456000000, time.UTC)}
	m.Content.Text = "  original UI text\n"
	return m
}

func TestNativeHumanMirrorRepairsOldSeenAndNeverReplaysUnknown(t *testing.T) {
	e, n, _ := bridgeTestEngine()
	e.state.User = "human-member"
	m := nativeHumanFixture()
	fingerprint := bridgeNativeFingerprint(m)
	e.state.NativeSeen[m.ID] = fingerprint // The released version skipped this message.
	n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
	if err := e.syncNative(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 1 {
		t.Fatal("previously filtered Human message not recovered")
	}
	out := e.state.Outbound[0]
	if !strings.Contains(out.Text, "你在 dot｜2026-10-10 09:02:03.456 CST") || !strings.HasSuffix(out.Text, m.Content.Text) || strings.Contains(out.Text, "更新") {
		t.Fatal("first recovered Human message lost original text/time or was mislabeled")
	}
	out.Status = "unknown"
	out.ContextToken = "context"
	out.Attempts = 1
	before := bridgeClone(t, e.state)
	e.state = bridgeClone(t, e.state)
	n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
	if err := e.syncNative(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 1 || e.state.Outbound[0].ClientID != before.Outbound[0].ClientID || e.state.Outbound[0].Status != "unknown" {
		t.Fatal("review replayed or changed unknown Human delivery")
	}
}

func TestNativeHumanMirrorOwnWeixinEchoAndOriginalEdits(t *testing.T) {
	for _, status := range []string{"attempting", "unknown", "acknowledged"} {
		t.Run(status, func(t *testing.T) {
			e, n, _ := bridgeTestEngine()
			e.state.User = "human-member"
			inMsg := bridgeTestText("incoming", "peer", "WX original", "context")
			raw, _ := json.Marshal(inMsg)
			in := &bridgeInbound{Key: bridgeInboundKey(inMsg), Message: inMsg, Raw: raw, Status: status, RequestID: "bridge-request", Baseline: "baseline"}
			if status == "acknowledged" {
				in.NativeID = "echo-id"
			}
			e.state.Inbound = []*bridgeInbound{in}
			m := nativeHumanFixture()
			m.ID = "echo-id"
			m.RequestID = in.RequestID
			m.Content.Text = "WX original"
			n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
			if err := e.syncNative(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if len(e.state.Outbound) != 0 {
				t.Fatal("WX-origin Human message echoed back before/after acknowledgment")
			}
			m.Content.Text = "edited in original UI"
			m.Updated = m.Created.Add(time.Minute)
			n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
			if err := e.syncNative(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if len(e.state.Outbound) != 1 || !strings.Contains(e.state.Outbound[0].Text, "（更新）") {
				t.Fatal("original UI edit of WX message suppressed")
			}
			deleted := m.Updated.Add(time.Minute)
			m.Deleted = &deleted
			n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
			if err := e.syncNative(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if len(e.state.Outbound) != 2 || !strings.Contains(e.state.Outbound[1].Text, "你已在 dot 删除") {
				t.Fatal("Human deletion not mirrored")
			}
		})
	}
}

func TestNativeHumanMirrorFileAndVerifiedSender(t *testing.T) {
	e, n, _ := bridgeTestEngine()
	e.state.User = "human-member"
	e.state.Version = 2
	m := nativeHumanFixture()
	m.Content.Text = ""
	m.Content.Attachments = []json.RawMessage{json.RawMessage(`{"type":"file","file_id":"native-original"}`)}
	foreign := m
	foreign.ID = "foreign-message"
	foreign.Sender = "stranger"
	n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m, foreign}}}
	if err := e.syncNative(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 2 || e.state.Outbound[0].Native.ID != m.ID || e.state.Outbound[1].Media == nil || e.state.Outbound[1].Media.NativeFileID != "native-original" {
		t.Fatal("Human file was not queued as context plus exact native file")
	}
	for _, out := range e.state.Outbound {
		if !bridgeOutboundValid(out, e.state) {
			t.Fatal("Human native output invalidated journal")
		}
	}
	bad := *e.state.Outbound[0]
	copyNative := *bad.Native
	copyNative.Sender = "stranger"
	bad.Native = &copyNative
	if bridgeOutboundValid(&bad, e.state) {
		t.Fatal("foreign member permitted by output validation")
	}
	n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
	if err := e.syncNative(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 2 {
		t.Fatal("file review duplicated text or original upload")
	}
}

func TestNativeHumanMirrorHistoryIsNeverBypassed(t *testing.T) {
	for _, seen := range []bool{false, true} {
		t.Run(map[bool]string{false: "unseen", true: "seen"}[seen], func(t *testing.T) {
			e, n, _ := bridgeTestEngine()
			e.state.User = "human-member"
			m := nativeHumanFixture()
			text := "original history entry"
			old := &bridgeOutbound{Key: "history:" + e.state.Room + ":" + m.ID + ":" + bridgeTextSHA(text), ClientID: "history-client", Status: "unknown", ContextToken: "history-context", Text: text, DeliveryKind: "history"}
			e.state.Outbound = []*bridgeOutbound{old}
			if seen {
				e.state.NativeSeen[m.ID] = bridgeNativeFingerprint(m)
			}
			n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
			if err := e.syncNative(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if len(e.state.Outbound) != 1 || old.Status != "unknown" || old.ClientID != "history-client" {
				t.Fatal("Human repair bypassed historical delivery identity")
			}
		})
	}
}

func TestNativeHumanMirrorChangedHistoryWaitsForOutcome(t *testing.T) {
	for _, status := range []string{"unknown", "attempting"} {
		t.Run(status, func(t *testing.T) {
			e, n, _ := bridgeTestEngine()
			e.state.User = "human-member"
			m := nativeHumanFixture()
			prior := bridgeNativeFingerprint(m)
			e.state.NativeSeen[m.ID] = prior
			text := "original history entry"
			old := &bridgeOutbound{Key: "history:" + e.state.Room + ":" + m.ID + ":" + bridgeTextSHA(text), ClientID: "history-client", Status: status, ContextToken: "history-context", Text: text, DeliveryKind: "history"}
			e.state.Outbound = []*bridgeOutbound{old}
			m.Content.Text = "edited while history outcome unresolved"
			m.Updated = m.Created.Add(time.Minute)
			n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
			if err := e.syncNative(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if len(e.state.Outbound) != 1 || e.state.NativeSeen[m.ID] != prior || old.Status != status || old.ClientID != "history-client" {
				t.Fatal("edited Human message bypassed unknown historical outcome or lost prior revision")
			}
			// A later confirmed history receipt permits the independently observed edit.
			old.Status = "acknowledged"
			old.WeixinID = "confirmed-history"
			n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
			if err := e.syncNative(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if len(e.state.Outbound) != 2 || !strings.Contains(e.state.Outbound[1].Text, "更新") {
				t.Fatal("confirmed history permanently suppressed the later Human edit")
			}
		})
	}
}

func TestNativeHumanMirrorMediaEchoMatchesExactFiles(t *testing.T) {
	e, n, _ := bridgeTestEngine()
	e.state.User = "human-member"
	msg := bridgeTestText("incoming-file", "peer", "same text", "context")
	msg.ItemList = append(msg.ItemList, weixin.MessageItem{Type: weixin.FileItemType, FileItem: json.RawMessage(`{"name":"original.txt"}`)})
	raw, _ := json.Marshal(msg)
	e.state.Inbound = []*bridgeInbound{{Key: bridgeInboundKey(msg), Message: msg, Raw: raw, Status: "acknowledged", NativeID: "file-echo", RequestID: "file-request", Media: []*bridgeIncomingMedia{{ID: "local-media", Stage: "native_uploaded", NativeFileID: "file-original"}}}}
	m := nativeHumanFixture()
	m.ID = "file-echo"
	m.RequestID = "file-request"
	m.Content.Text = "same text"
	m.Content.Attachments = []json.RawMessage{json.RawMessage(`{"type":"file","file_id":"file-original"}`)}
	n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
	if err := e.syncNative(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 0 {
		t.Fatal("unchanged WX file echoed")
	}
	m.Content.Attachments = []json.RawMessage{json.RawMessage(`{"type":"file","file_id":"file-replaced"}`)}
	m.Updated = m.Created.Add(time.Minute)
	n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
	if err := e.syncNative(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Outbound) != 2 || e.state.Outbound[1].Media.NativeFileID != "file-replaced" {
		t.Fatal("replaced Human file suppressed as original WX echo")
	}
}

func TestNativeHumanMirrorReturnToWeixinOriginalIsVisible(t *testing.T) {
	for _, withFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "file"}[withFile], func(t *testing.T) {
			e, n, _ := bridgeTestEngine()
			e.state.User = "human-member"
			msg := bridgeTestText("incoming-original", "peer", "WX original A", "context")
			raw, _ := json.Marshal(msg)
			in := &bridgeInbound{Key: bridgeInboundKey(msg), Message: msg, Raw: raw, Status: "acknowledged", NativeID: "echo-original", RequestID: "bridge-original-request"}
			m := nativeHumanFixture()
			m.ID, m.RequestID, m.Content.Text = in.NativeID, in.RequestID, "WX original A"
			if withFile {
				in.Media = []*bridgeIncomingMedia{{ID: "local-original", Stage: "native_uploaded", NativeFileID: "file-A"}}
				m.Content.Attachments = []json.RawMessage{json.RawMessage(`{"type":"file","file_id":"file-A"}`)}
			}
			e.state.Inbound = []*bridgeInbound{in}
			review := func() {
				t.Helper()
				n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{m}}}
				if err := e.syncNative(context.Background(), true); err != nil {
					t.Fatal(err)
				}
			}
			review()
			if len(e.state.Outbound) != 0 {
				t.Fatal("initial WX original echoed")
			}
			m.Updated = m.Created.Add(time.Minute)
			if withFile {
				m.Content.Attachments = []json.RawMessage{json.RawMessage(`{"type":"file","file_id":"file-B"}`)}
			} else {
				m.Content.Text = "UI edit B"
			}
			review()
			firstRevisionEntries := len(e.state.Outbound)
			wantRevisionEntries := 1
			if withFile {
				wantRevisionEntries = 2
			}
			if firstRevisionEntries != wantRevisionEntries {
				t.Fatal("first original-UI revision not mirrored")
			}
			m.Updated = m.Created.Add(2 * time.Minute)
			if withFile {
				m.Content.Attachments = []json.RawMessage{json.RawMessage(`{"type":"file","file_id":"file-A"}`)}
			} else {
				m.Content.Text = "WX original A"
			}
			review()
			if len(e.state.Outbound) != 2*wantRevisionEntries {
				t.Fatal("return to WX original mistaken for its initial echo")
			}
			returned := e.state.Outbound[firstRevisionEntries]
			if !strings.Contains(returned.Text, "（更新）") || !strings.HasSuffix(returned.Text, m.Content.Text) {
				t.Fatal("return to original lost its update label or original text")
			}
			if withFile && e.state.Outbound[firstRevisionEntries+1].Media.NativeFileID != "file-A" {
				t.Fatal("returned original attachment identity lost")
			}
			returned.Status, returned.ContextToken, returned.Attempts = "unknown", "private-context", 1
			before := bridgeHash(e.state)
			review()
			if bridgeHash(e.state) != before {
				t.Fatal("repeated review duplicated or changed the unknown returned revision")
			}
		})
	}
}

func TestBridgeVerifyHumanMember(t *testing.T) {
	fixture := func() *chatgpt.Room {
		room := &chatgpt.Room{}
		if err := json.Unmarshal([]byte(`{"members":[{"account_user_id":"dot-member","aeon_id":"dot"},{"account_user_id":"human-member"}]}`), room); err != nil {
			t.Fatal(err)
		}
		return room
	}
	cases := []struct {
		name   string
		change func(*chatgpt.Room)
		valid  bool
	}{
		{name: "exact personal DM", valid: true},
		{name: "reversed members", valid: true, change: func(r *chatgpt.Room) { r.Members[0], r.Members[1] = r.Members[1], r.Members[0] }},
		{name: "duplicate Human", change: func(r *chatgpt.Room) { r.Members[0] = r.Members[1] }},
		{name: "duplicate Dot", change: func(r *chatgpt.Room) { r.Members[1] = r.Members[0] }},
		{name: "foreign Human", change: func(r *chatgpt.Room) { r.Members[1].ID = "foreign-member" }},
		{name: "missing Human", change: func(r *chatgpt.Room) { r.Members = r.Members[:1] }},
		{name: "extra member", change: func(r *chatgpt.Room) { r.Members = append(r.Members, r.Members[1]) }},
		{name: "empty member identity", change: func(r *chatgpt.Room) { r.Members[1].ID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			room := fixture()
			if tc.change != nil {
				tc.change(room)
			}
			err := bridgeVerifyHumanMember(room, "human-member", "dot-member")
			if (err == nil) != tc.valid {
				t.Fatal("room accepted/rejected the wrong authenticated member configuration")
			}
			if err != nil && (strings.Contains(err.Error(), "human-member") || strings.Contains(err.Error(), "foreign-member")) {
				t.Fatal("identity failure disclosed a member identifier")
			}
		})
	}
	for _, args := range [][2]string{{"", "dot-member"}, {"human-member", ""}, {"human-member", "human-member"}} {
		if err := bridgeVerifyHumanMember(fixture(), args[0], args[1]); err == nil {
			t.Fatal("missing or ambiguous authenticated identity accepted")
		}
	}
	if err := bridgeVerifyHumanMember(nil, "human-member", "dot-member"); err == nil {
		t.Fatal("missing verified room accepted")
	}
}
