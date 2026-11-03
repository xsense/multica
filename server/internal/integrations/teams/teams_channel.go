package teams

import (
	"context"
	"errors"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// teamsChannel is the per-installation Channel the engine.Supervisor builds.
// Teams pushes activities to our HTTP endpoint instead of letting us hold a
// connection, so there is no receive loop: Connect just parks until the
// Supervisor cancels it. Registering a Factory still matters — without one the
// Supervisor would log a build failure for every Teams installation forever.
type teamsChannel struct {
	appID string
}

func (c *teamsChannel) Type() channel.Type { return TypeTeams }

func (c *teamsChannel) Capabilities() channel.Capability {
	return channel.CapText | channel.CapThreadReply | channel.CapTypingIndicator
}

// Connect blocks until ctx ends. Inbound arrives through Webhook.
func (c *teamsChannel) Connect(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (c *teamsChannel) Disconnect(context.Context) error { return nil }

// Send is not used by the engine; replies go through Outbound and the
// replier, which know the conversation's serviceUrl.
func (c *teamsChannel) Send(context.Context, channel.OutboundMessage) (channel.SendResult, error) {
	return channel.SendResult{}, errors.New("teams: direct Channel.Send is not supported; use the outbound subscriber")
}

// RegisterTeams registers the passive per-installation Factory.
func RegisterTeams(reg *channel.Registry) {
	reg.Register(TypeTeams, func(cfg channel.Config) (channel.Channel, error) {
		pub := DecodePublicConfig(cfg.Raw)
		if pub.AppID == "" {
			return nil, errors.New("teams: installation config has no app_id")
		}
		return &teamsChannel{appID: pub.AppID}, nil
	})
}
