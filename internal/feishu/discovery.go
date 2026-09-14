package feishu

import (
	"context"
	"errors"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
)

// DiscoveredIdentity is the minimum metadata needed to bind the bridge to one
// Feishu group and one authorized operator. Message content is deliberately
// neither exposed nor retained during discovery.
type DiscoveredIdentity struct {
	ChatID string
	OpenID string
}

// DiscoverGroupIdentity runs an official Feishu long connection until ctx is
// cancelled. It reports identifiers only for human messages in group chats.
func DiscoverGroupIdentity(ctx context.Context, appID, appSecret string, sink func(DiscoveredIdentity)) error {
	if strings.TrimSpace(appID) == "" || strings.TrimSpace(appSecret) == "" || sink == nil {
		return errors.New("feishu discovery configuration is incomplete")
	}
	d := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
		if identity, ok := discoveredIdentity(event); ok {
			sink(identity)
		}
		return nil
	})
	client := ws.NewClient(appID, appSecret, ws.WithEventHandler(d), ws.WithLogLevel(larkcore.LogLevelWarn))
	errCh := make(chan error, 1)
	go func() { errCh <- client.Start(ctx) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		client.Close()
		return nil
	}
}

func discoveredIdentity(event *larkim.P2MessageReceiveV1) (DiscoveredIdentity, bool) {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil || event.Event.Sender.SenderId == nil {
		return DiscoveredIdentity{}, false
	}
	message, sender := event.Event.Message, event.Event.Sender
	if value(sender.SenderType) != "user" || value(message.ChatType) != "group" {
		return DiscoveredIdentity{}, false
	}
	identity := DiscoveredIdentity{ChatID: strings.TrimSpace(value(message.ChatId)), OpenID: strings.TrimSpace(value(sender.SenderId.OpenId))}
	return identity, identity.ChatID != "" && identity.OpenID != ""
}
