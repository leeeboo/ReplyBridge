package feishu

import (
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestDiscoveredIdentityOnlyAcceptsHumanGroupMessages(t *testing.T) {
	event := receiveEvent("oc_group", "user", "ou_user", "text", `{"text":"private body"}`, "", "", nil)
	identity, ok := discoveredIdentity(event)
	if !ok || identity.ChatID != "oc_group" || identity.OpenID != "ou_user" {
		t.Fatalf("identity=%#v ok=%v", identity, ok)
	}
	for name, mutate := range map[string]func(*larkim.P2MessageReceiveV1){
		"bot": func(event *larkim.P2MessageReceiveV1) { event.Event.Sender.SenderType = ptr("bot") },
		"p2p": func(event *larkim.P2MessageReceiveV1) { event.Event.Message.ChatType = ptr("p2p") },
		"missing open id": func(event *larkim.P2MessageReceiveV1) {
			event.Event.Sender.SenderId.OpenId = ptr("")
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := receiveEvent("oc_group", "user", "ou_user", "text", `{"text":"private body"}`, "", "", nil)
			mutate(candidate)
			if identity, ok := discoveredIdentity(candidate); ok {
				t.Fatalf("accepted identity %#v", identity)
			}
		})
	}
}
