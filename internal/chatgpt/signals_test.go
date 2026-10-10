package chatgpt

import "testing"

func TestSocketShardKeepsOriginAndUser(t *testing.T) {
	for _, raw := range []string{"wss://ws.chatgpt.com/p0/ws/user/user-original?ticket=fixture", "wss://ws.chatgpt.com/p16/ws/user/user-original?ticket=fixture"} {
		if !validSocketURL(raw, "user-original") {
			t.Fatalf("legitimate numeric shard rejected")
		}
	}
	for _, raw := range []string{"ws://ws.chatgpt.com/p16/ws/user/user-original", "wss://evil.example/p16/ws/user/user-original", "wss://ws.chatgpt.com:443/p16/ws/user/user-original", "wss://x@ws.chatgpt.com/p16/ws/user/user-original", "wss://ws.chatgpt.com/p16/ws/user/user-other", "wss://ws.chatgpt.com/p16/ws/user/user-original/extra", "wss://ws.chatgpt.com/pother/ws/user/user-original", "wss://ws.chatgpt.com/p16/ws/user/user-original#fragment"} {
		if validSocketURL(raw, "user-original") {
			t.Fatalf("unsafe socket URL accepted")
		}
	}
}
