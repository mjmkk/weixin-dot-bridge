// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/chatgpt"
)

func bridgeHumanLinkFixture(thumbnail string, withFile bool) chatgpt.Message {
	m := nativeHumanFixture()
	raw, _ := json.Marshal(map[string]any{
		"attachment_id": "link-attachment", "type": "link", "url": "https://example.invalid/original",
		"title": "Original title", "description": "Original description", "thumbnail_url": thumbnail,
	})
	m.Content.Attachments = []json.RawMessage{raw}
	if withFile {
		file := bridgeNativeFileWithCapability("https://example.invalid/file-capability", "ready")
		m.Content.Attachments = append(m.Content.Attachments, file.Content.Attachments[0])
	}
	return m
}

func TestBridgeHumanLinkThumbnailRefreshAndLegacyRevisionMigration(t *testing.T) {
	for _, hashKind := range []string{"raw", "file-normalized", "current"} {
		for _, withFile := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/file-%v", hashKind, withFile), func(t *testing.T) {
				e, n, _ := bridgeTestEngine()
				e.state.User = "human-member"
				original := bridgeHumanLinkFixture("https://example.invalid/thumbnail-first", withFile)
				fingerprint := bridgeNativeFingerprint(original)
				switch hashKind {
				case "raw":
					fingerprint = bridgeHash(original)
				case "file-normalized":
					fingerprint = bridgeLegacyNativeFingerprint(original)
				}
				if err := e.queueNative(original, false, fingerprint); err != nil {
					t.Fatal(err)
				}
				e.state.NativeSeen[original.ID] = fingerprint
				ack := e.state.Outbound[0]
				ack.Status, ack.ContextToken, ack.WeixinID, ack.Attempts = "acknowledged", "context", "original-ack", 1
				before := bridgeClone(t, e.state)
				e.state = bridgeClone(t, e.state) // Simulate the existing journal on restart.
				for _, thumbnail := range []string{"https://example.invalid/thumbnail-second", "https://example.invalid/thumbnail-third"} {
					refreshed := bridgeHumanLinkFixture(thumbnail, withFile)
					n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{refreshed}}}
					if err := e.syncNative(context.Background(), true); err != nil {
						t.Fatal(err)
					}
					if len(e.state.Outbound) != len(before.Outbound) || e.state.NativeSeen[original.ID] != bridgeNativeFingerprint(original) {
						t.Fatal("link refresh queued a false edit or did not migrate the exact legacy revision")
					}
					for i, out := range before.Outbound {
						if bridgeHash(out) != bridgeHash(e.state.Outbound[i]) {
							t.Fatal("normalization rewrote the original snapshot, receipt or file delivery")
						}
					}
				}
				// A true text change and a later return to A remain separate
				// source revisions, with their original update timestamps retained.
				edited := bridgeHumanLinkFixture("https://example.invalid/thumbnail-fourth", withFile)
				edited.Content.Text = "  Genuine text B\n"
				edited.Updated = edited.Created.Add(time.Minute)
				reverted := original
				reverted.Updated = original.Created.Add(2 * time.Minute)
				for _, revision := range []chatgpt.Message{edited, reverted} {
					n.pages = []*chatgpt.Page{{Items: []chatgpt.Message{revision}}}
					if err := e.syncNative(context.Background(), true); err != nil {
						t.Fatal(err)
					}
					out := e.state.Outbound[len(e.state.Outbound)-1]
					if !strings.Contains(out.Text, "（更新）") || !strings.Contains(out.Text, revision.Content.Text) || out.Media != nil {
						t.Fatal("real edit/revert lost its source text or repeated the original attachment")
					}
				}
				if len(e.state.Outbound) != len(before.Outbound)+2 {
					t.Fatal("real text edit or revert was suppressed")
				}
			})
		}
	}
}

func TestBridgeNativeFingerprintCanonicalKeysAndRealLinkChanges(t *testing.T) {
	original := bridgeHumanLinkFixture("https://example.invalid/thumbnail-first", false)
	original.Content.Attachments[0] = json.RawMessage(`{"type":"link","attachment_id":"link-attachment","url":"https://example.invalid/original","title":"Original title","metadata":{"counter":18446744073709551615,"status":"ready"},"thumbnail_url":"https://example.invalid/first"}`)
	reordered := original
	reordered.Content.Attachments = []json.RawMessage{json.RawMessage(`{"thumbnail_url":"https://example.invalid/second","metadata":{"status":"ready","counter":18446744073709551615},"title":"Original title","url":"https://example.invalid/original","attachment_id":"link-attachment","type":"link"}`)}
	before := bridgeHash(original)
	if bridgeNativeFingerprint(original) != bridgeNativeFingerprint(reordered) || bridgeHash(original) != before {
		t.Fatal("object key order or thumbnail refresh changed the fingerprint, or modified the original")
	}
	for name, mutate := range map[string]func(*chatgpt.Message){
		"text":        func(m *chatgpt.Message) { m.Content.Text += "real edit" },
		"timestamp":   func(m *chatgpt.Message) { m.Updated = time.Now().UTC() },
		"reaction":    func(m *chatgpt.Message) { m.Reactions = map[string]string{"human": "reaction"} },
		"delete":      func(m *chatgpt.Message) { at := time.Now().UTC(); m.Deleted = &at },
		"remove-link": func(m *chatgpt.Message) { m.Content.Attachments = nil },
		"add-link": func(m *chatgpt.Message) {
			m.Content.Attachments = append(m.Content.Attachments, json.RawMessage(`{"type":"link","attachment_id":"another-link"}`))
		},
		"href": func(m *chatgpt.Message) {
			m.Content.Attachments[0] = json.RawMessage(strings.Replace(string(m.Content.Attachments[0]), "/original", "/actual-new-link", 1))
		},
		"title": func(m *chatgpt.Message) {
			m.Content.Attachments[0] = json.RawMessage(strings.Replace(string(m.Content.Attachments[0]), "Original title", "New title", 1))
		},
		"identity": func(m *chatgpt.Message) {
			m.Content.Attachments[0] = json.RawMessage(strings.Replace(string(m.Content.Attachments[0]), "link-attachment", "new-attachment", 1))
		},
		"type": func(m *chatgpt.Message) {
			m.Content.Attachments[0] = json.RawMessage(strings.Replace(string(m.Content.Attachments[0]), `"type":"link"`, `"type":"thread"`, 1))
		},
		"status": func(m *chatgpt.Message) {
			m.Content.Attachments[0] = json.RawMessage(strings.Replace(string(m.Content.Attachments[0]), `"ready"`, `"failed"`, 1))
		},
		"large-number": func(m *chatgpt.Message) {
			m.Content.Attachments[0] = json.RawMessage(strings.Replace(string(m.Content.Attachments[0]), "18446744073709551615", "18446744073709551614", 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := original
			changed.Content.Attachments = append([]json.RawMessage(nil), original.Content.Attachments...)
			mutate(&changed)
			if bridgeNativeFingerprint(original) == bridgeNativeFingerprint(changed) {
				t.Fatal("a real source revision was hidden by thumbnail normalization")
			}
		})
	}
}

func TestBridgeHumanRetiresOnlyConsecutiveConfirmedThumbnailUpdates(t *testing.T) {
	e, _, _ := bridgeTestEngine()
	e.state.User = "human-member"
	queue := func(m chatgpt.Message, edited bool) *bridgeOutbound {
		t.Helper()
		if err := e.queueNative(m, edited, bridgeLegacyNativeFingerprint(m)); err != nil {
			t.Fatal(err)
		}
		return e.state.Outbound[len(e.state.Outbound)-1]
	}
	original := bridgeHumanLinkFixture("https://example.invalid/thumbnail-original", false)
	ack := queue(original, false)
	ack.Status, ack.ContextToken, ack.WeixinID, ack.Attempts = "acknowledged", "context", "original-ack", 1
	rejected := queue(bridgeHumanLinkFixture("https://example.invalid/thumbnail-rejected", false), true)
	rejected.Status, rejected.ContextToken, rejected.Attempts = "rejected", "context", 1
	rejected.LastFailure = &bridgeSendFailure{Op: "sendmessage", Ret: -2, Reason: "prepare_failed"}
	ready := queue(bridgeHumanLinkFixture("https://example.invalid/thumbnail-ready", false), true)
	unknown := queue(bridgeHumanLinkFixture("https://example.invalid/thumbnail-unknown", false), true)
	unknown.Status, unknown.ContextToken, unknown.Attempts = "unknown", "context", 1
	attempting := queue(bridgeHumanLinkFixture("https://example.invalid/thumbnail-attempting", false), true)
	attempting.Status, attempting.ContextToken, attempting.Attempts = "attempting", "context", 1
	mismatch := queue(bridgeHumanLinkFixture("https://example.invalid/thumbnail-payload", false), true)
	mismatch.Text += "different actual output"
	trueEdit := original
	trueEdit.Content.Text = "real B"
	queue(trueEdit, true)
	// Returning to A with a new thumbnail cannot use an arbitrary older A receipt.
	revert := queue(bridgeHumanLinkFixture("https://example.invalid/thumbnail-revert", false), true)
	historyText := "original history payload"
	history := &bridgeOutbound{Key: "history:" + e.state.Room + ":history-id:" + bridgeTextSHA(historyText), Text: historyText, Status: "ready", ClientID: "history-client", DeliveryKind: "history"}
	e.state.Outbound = append(e.state.Outbound, history)
	before := bridgeClone(t, e.state)
	if err := e.retireMetadataOnlyUpdates(); err != nil {
		t.Fatal(err)
	}
	for _, out := range []*bridgeOutbound{rejected, ready} {
		if out.Status != "superseded_metadata_only" || !bridgeOutboundValid(out, e.state) || out.MetadataSupersession.AcknowledgedKey != ack.Key {
			t.Fatal("confirmed Human thumbnail-only update was not durably retired")
		}
	}
	if unknown.Status != "unknown" || attempting.Status != "attempting" || mismatch.Status != "ready" || revert.Status != "ready" || history.Status != "ready" {
		t.Fatal("retirement changed an unknown, attempting, mismatched, reverted or historical delivery")
	}
	expected := bridgeClone(t, before)
	for _, index := range []int{1, 2} {
		expected.Outbound[index].Status = "superseded_metadata_only"
		expected.Outbound[index].MetadataSupersession = e.state.Outbound[index].MetadataSupersession
	}
	if bridgeHash(expected) != bridgeHash(e.state) {
		t.Fatal("retirement changed attempts, payload, failure, receipts, context or other journal facts")
	}
	e.state = bridgeClone(t, e.state)
	restored := bridgeHash(e.state)
	if err := e.retireMetadataOnlyUpdates(); err != nil || bridgeHash(e.state) != restored {
		t.Fatal("restart did not preserve supersession validation and idempotent recovery")
	}
}
