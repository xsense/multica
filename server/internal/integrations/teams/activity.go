package teams

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
)

// This file holds the inbound Activity schema (the subset Teams sends) and the
// translation to the engine's normalized channel.InboundMessage.

// Activity is the Bot Framework activity envelope.
type Activity struct {
	Type         string           `json:"type"`
	ID           string           `json:"id"`
	Timestamp    string           `json:"timestamp"`
	ServiceURL   string           `json:"serviceUrl"`
	ChannelID    string           `json:"channelId"`
	From         ChannelAccount   `json:"from"`
	Conversation Conversation     `json:"conversation"`
	Recipient    ChannelAccount   `json:"recipient"`
	TextFormat   string           `json:"textFormat"`
	Text         string           `json:"text"`
	ReplyToID    string           `json:"replyToId"`
	Entities     []Entity         `json:"entities"`
	Attachments  []Attachment     `json:"attachments"`
	ChannelData  json.RawMessage  `json:"channelData"`
	Value        json.RawMessage  `json:"value"`
	MembersAdded []ChannelAccount `json:"membersAdded"`
}

// ChannelAccount identifies a user or bot. For a user, ID is the
// conversation-scoped "29:…" id and AADObjectID the stable Entra object id.
// For the bot, ID is "28:<app id>".
type ChannelAccount struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	AADObjectID string `json:"aadObjectId"`
}

// Conversation is the chat the activity happened in.
type Conversation struct {
	ID               string `json:"id"`
	ConversationType string `json:"conversationType"` // personal | groupChat | channel
	TenantID         string `json:"tenantId"`
	IsGroup          bool   `json:"isGroup"`
	Name             string `json:"name"`
}

// Entity is an activity entity; Teams uses type "mention" for @-mentions.
type Entity struct {
	Type      string          `json:"type"`
	Mentioned *ChannelAccount `json:"mentioned,omitempty"`
	Text      string          `json:"text,omitempty"`
}

// Attachment is a message attachment (file, card, inline image).
type Attachment struct {
	ContentType string `json:"contentType"`
	ContentURL  string `json:"contentUrl"`
	Name        string `json:"name"`
}

// teamsChannelData is the Teams-specific channelData block.
type teamsChannelData struct {
	Tenant struct {
		ID string `json:"id"`
	} `json:"tenant"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
}

// botIDPrefix is how the Connector addresses a bot: "28:<microsoft app id>".
const botIDPrefix = "28:"

// botAppID extracts the App ID the activity was sent to. Untrusted until the
// token's audience has been checked against it.
func (a Activity) botAppID() string {
	id := strings.TrimSpace(a.Recipient.ID)
	if strings.HasPrefix(id, botIDPrefix) {
		id = id[len(botIDPrefix):]
	}
	norm, _ := normalizeGUID(id)
	return norm
}

// tenantID returns the Entra tenant the conversation belongs to.
func (a Activity) tenantID() string {
	t := a.Conversation.TenantID
	if t == "" && len(a.ChannelData) > 0 {
		var cd teamsChannelData
		if json.Unmarshal(a.ChannelData, &cd) == nil {
			t = cd.Tenant.ID
		}
	}
	norm, _ := normalizeGUID(t)
	return norm
}

// teamsRawEvent carries the Teams-specific fields the cross-platform envelope
// does not; read back only inside this package's resolvers and replier.
type teamsRawEvent struct {
	// AppID routes the message to its installation (config->>'app_id').
	AppID string `json:"app_id"`
	// TenantID is the conversation's Entra tenant, already checked against
	// the installation's.
	TenantID string `json:"tenant_id"`
	// ServiceURL is the Connector endpoint replies must go to; it is covered
	// by the signed token.
	ServiceURL string `json:"service_url"`
	// ConversationID is the real Teams conversation id (ChatID can be a
	// composite isolation key).
	ConversationID string `json:"conversation_id"`
	// FromID is the conversation-scoped "29:…" user id the member lookup
	// endpoint takes.
	FromID string `json:"from_id"`
	// SenderName is the display name, for prompts and attribution.
	SenderName string `json:"sender_name,omitempty"`
	// ActivityID is the triggering activity, for quoting replies.
	ActivityID string `json:"activity_id"`
	// EventType is a coarse label for drop audits ("message").
	EventType string `json:"event_type"`
}

// chatTypeOf maps Teams conversationType to the engine's two room shapes.
func chatTypeOf(c Conversation) (channel.ChatType, bool) {
	switch c.ConversationType {
	case "personal":
		return channel.ChatTypeP2P, true
	case "groupChat", "channel":
		return channel.ChatTypeGroup, true
	case "":
		if c.IsGroup {
			return channel.ChatTypeGroup, true
		}
		return channel.ChatTypeP2P, true
	default:
		return "", false
	}
}

// splitChannelThread separates a channel conversation id of the form
// "19:…@thread.tacv2;messageid=<root>" into the channel id and the thread root.
// Group chats and personal chats have no ";messageid=" suffix.
func splitChannelThread(conversationID string) (channelID, rootID string) {
	base, suffix, ok := strings.Cut(conversationID, ";messageid=")
	if !ok {
		return conversationID, ""
	}
	return base, suffix
}

// inboundFromActivity normalizes a verified message activity. ok=false means
// the activity must not reach the core (not a user message, from a bot, empty
// after cleanup, unsupported conversation type).
func inboundFromActivity(a Activity, appID, tenantID string) (channel.InboundMessage, bool) {
	if a.Type != "message" {
		return channel.InboundMessage{}, false
	}
	if a.From.ID == "" || strings.HasPrefix(a.From.ID, botIDPrefix) {
		return channel.InboundMessage{}, false
	}
	chatType, ok := chatTypeOf(a.Conversation)
	if !ok || a.Conversation.ID == "" {
		return channel.InboundMessage{}, false
	}

	mentioned := mentionsBot(a, appID)
	addressed := chatType == channel.ChatTypeP2P || mentioned
	text := cleanText(a, appID)

	msgType := channel.MsgTypeText
	if strings.TrimSpace(text) == "" {
		if hasFileAttachment(a) {
			// Files need the Graph API with extra consent; v1 tells the user
			// instead of silently ignoring them (webhook.go sends the notice).
			msgType = channel.MsgTypeUnknown
		} else {
			return channel.InboundMessage{}, false
		}
	}

	commandText := text
	forceFresh := false
	if control, ok := engine.ParseControlCommand(text); ok {
		text = control.Body
		forceFresh = control.Kind == engine.ControlCommandFreshSession
	}

	_, rootID := splitChannelThread(a.Conversation.ID)
	senderID := a.From.AADObjectID
	if senderID == "" {
		senderID = a.From.ID
	}

	raw, _ := json.Marshal(teamsRawEvent{
		AppID:          appID,
		TenantID:       tenantID,
		ServiceURL:     a.ServiceURL,
		ConversationID: a.Conversation.ID,
		FromID:         a.From.ID,
		SenderName:     a.From.Name,
		ActivityID:     a.ID,
		EventType:      "message",
	})

	var reply *channel.ReplyCtx
	if a.ReplyToID != "" {
		reply = &channel.ReplyCtx{MessageID: a.ReplyToID, RootID: rootID}
	}

	return channel.InboundMessage{
		EventID: a.ID,
		// Activity ids are unique per conversation; the dedup key
		// (installation, message_id) uses the composite form.
		MessageID:      a.Conversation.ID + "|" + a.ID,
		Type:           msgType,
		Text:           text,
		CommandText:    commandText,
		ReplyTo:        reply,
		AddressedToBot: addressed,
		ForceFresh:     forceFresh,
		Source: channel.Source{
			ChannelType: TypeTeams,
			// A channel thread's conversation id already carries its root
			// (";messageid="), so one Teams thread is one Multica session —
			// the Slack-thread isolation shape. Personal and group chats are
			// one continuous session each.
			ChatID:   a.Conversation.ID,
			ChatType: chatType,
			// The Entra object id is stable across bots and conversations in
			// the tenant, so it serves as both ids.
			SenderID:       senderID,
			SenderStableID: a.From.AADObjectID,
			ThreadID:       rootID,
		},
		Raw: raw,
	}, true
}

// mentionsBot reports whether a mention entity targets this bot.
func mentionsBot(a Activity, appID string) bool {
	for _, e := range a.Entities {
		if e.Type != "mention" || e.Mentioned == nil {
			continue
		}
		id := strings.TrimPrefix(e.Mentioned.ID, botIDPrefix)
		if norm, _ := normalizeGUID(id); norm == appID {
			return true
		}
		if a.Recipient.ID != "" && strings.EqualFold(e.Mentioned.ID, a.Recipient.ID) {
			return true
		}
	}
	return false
}

var (
	reAtTag     = regexp.MustCompile(`(?is)<at[^>]*>(.*?)</at>`)
	reBreak     = regexp.MustCompile(`(?i)<br\s*/?>`)
	reBlockEnd  = regexp.MustCompile(`(?i)</(p|div|li|h[1-6]|tr|blockquote|pre)>`)
	reListItem  = regexp.MustCompile(`(?i)<li[^>]*>`)
	reAnyTag    = regexp.MustCompile(`(?s)<[^>]+>`)
	reManyBlank = regexp.MustCompile(`\n{3,}`)
	// reHTMLish recognizes the markup Teams actually sends, so plain text
	// that merely contains "<" is not run through tag stripping.
	reHTMLish = regexp.MustCompile(`(?i)<(at|p|br|div|span|b|i|strong|em|ul|ol|li|a|pre|code|blockquote|h[1-6])[\s>/]`)
)

// cleanText turns Teams message text (plain, or HTML when the user formatted
// it or @-mentioned someone) into the plain text the agent reads. The bot's
// own mention is removed; other mentions keep the person's name.
func cleanText(a Activity, appID string) string {
	text := a.Text
	botNames := botMentionTexts(a, appID)
	// Teams marks formatted messages textFormat "xml" (HTML body); a plain
	// message says "plain" and is taken literally, so "a<b>c" survives.
	isHTML := strings.EqualFold(a.TextFormat, "xml") ||
		(!strings.EqualFold(a.TextFormat, "plain") && reHTMLish.MatchString(text))
	if isHTML {
		text = reAtTag.ReplaceAllStringFunc(text, func(m string) string {
			inner := reAtTag.FindStringSubmatch(m)[1]
			for _, n := range botNames {
				if strings.EqualFold(strings.TrimSpace(html.UnescapeString(reAnyTag.ReplaceAllString(n, ""))),
					strings.TrimSpace(html.UnescapeString(inner))) {
					return ""
				}
			}
			return "@" + inner
		})
		text = reBreak.ReplaceAllString(text, "\n")
		text = reListItem.ReplaceAllString(text, "- ")
		text = reBlockEnd.ReplaceAllString(text, "\n")
		text = reAnyTag.ReplaceAllString(text, "")
		text = html.UnescapeString(text)
	} else {
		// Plain text: drop only a leading mention of the bot ("@Bot ..."),
		// never every occurrence of its name.
		trimmed := strings.TrimSpace(text)
		for _, n := range botNames {
			for _, prefix := range []string{n, "@" + n} {
				if n != "" && strings.HasPrefix(trimmed, prefix) {
					trimmed = strings.TrimSpace(trimmed[len(prefix):])
				}
			}
		}
		text = trimmed
	}
	text = strings.ReplaceAll(text, " ", " ")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = reManyBlank.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// botMentionTexts returns the exact mention texts ("<at>Bot</at>") Teams put
// in the message for this bot, plus the bare display names.
func botMentionTexts(a Activity, appID string) []string {
	var out []string
	for _, e := range a.Entities {
		if e.Type != "mention" || e.Mentioned == nil {
			continue
		}
		id := strings.TrimPrefix(e.Mentioned.ID, botIDPrefix)
		norm, _ := normalizeGUID(id)
		if norm != appID && !strings.EqualFold(e.Mentioned.ID, a.Recipient.ID) {
			continue
		}
		if e.Text != "" {
			out = append(out, e.Text)
		}
		if e.Mentioned.Name != "" {
			out = append(out, e.Mentioned.Name)
		}
	}
	if a.Recipient.Name != "" {
		out = append(out, a.Recipient.Name)
	}
	return out
}

// hasFileAttachment reports a user-sent file or image (as opposed to the
// text/html rendering Teams attaches to every formatted message).
func hasFileAttachment(a Activity) bool {
	for _, att := range a.Attachments {
		switch {
		case att.ContentType == "text/html":
			continue
		case strings.HasPrefix(att.ContentType, "application/vnd.microsoft.teams.file"),
			strings.HasPrefix(att.ContentType, "image/"),
			att.ContentType == "application/vnd.microsoft.card.adaptive" && att.ContentURL == "":
			return true
		}
	}
	return false
}
