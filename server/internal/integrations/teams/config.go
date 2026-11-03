// Package teams is the Microsoft Teams integration for the channel-agnostic
// engine. It follows the BYO model the Slack and Telegram adapters use: the
// workspace admin registers an Azure Bot (single-tenant Entra app), pastes its
// App ID, tenant ID and client secret into Multica, and the installation is
// keyed by the App ID (config->>'app_id').
//
// Transport differs from the other adapters. Teams has no outbound socket a
// server can hold open: the Bot Framework Connector POSTs every activity to
// the bot's messaging endpoint. So inbound is an HTTP handler (webhook.go)
// that verifies the Connector's JWT, translates the activity and hands it to
// the shared engine.Router. The Supervisor still builds one Channel per active
// installation (teams_channel.go), but that Channel is passive — it exists so
// the installation is accounted for, not to run a receive loop.
//
// Outbound replies post the agent's finished answer through the Connector REST
// API (outbound.go); verdict notices (binding prompt, offline agent, /issue
// outcomes) live in replier.go. A typing indicator is refreshed while the
// agent works (typing.go).
//
// This package is a downstream fork addition (xsense/multica), not upstream
// code. Keep it self-contained so rebasing onto a new upstream tag only
// touches the small wiring edits listed in docs/teams-integration.md.
package teams

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// TypeTeams is the channel discriminator for the Teams adapter. Defined here
// (not in the channel core) so registering the platform never edits the core.
const TypeTeams channel.Type = "teams"

// installConfig is the JSON shape stored in channel_installation.config for a
// Teams installation.
//
// app_id is the bot's Microsoft App ID (the Entra application/client id). It
// fills the generic (channel_type, config->>'app_id') routing slot, so one
// Azure Bot maps to exactly one agent across all workspaces. The Connector
// addresses the bot as recipient "28:<app_id>" and puts the App ID in the
// token audience, which is how inbound finds its installation.
//
// app_secret_encrypted is base64 secretbox ciphertext, never plaintext.
type installConfig struct {
	AppID              string `json:"app_id"`
	TenantID           string `json:"tenant_id"`
	BotName            string `json:"bot_name,omitempty"`
	AppSecretEncrypted string `json:"app_secret_encrypted"`
}

// credentials is the decoded, decrypted form the transports run on.
type credentials struct {
	AppID     string
	TenantID  string
	BotName   string
	AppSecret string
}

// Decrypter turns stored ciphertext into plaintext. The wiring injects a
// secretbox-backed implementation; tests inject nil (stored bytes are treated
// as plaintext).
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

// decodeCredentials parses the per-installation config blob and decrypts the
// stored client secret. It is the single place the Teams config JSON is
// interpreted.
func decodeCredentials(raw []byte, decrypt Decrypter) (credentials, error) {
	if len(raw) == 0 {
		return credentials{}, errors.New("teams: empty installation config")
	}
	var cfg installConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return credentials{}, fmt.Errorf("decode teams installation config: %w", err)
	}
	secret, err := decryptSecret(cfg.AppSecretEncrypted, decrypt)
	if err != nil {
		return credentials{}, fmt.Errorf("decrypt app secret: %w", err)
	}
	if cfg.AppID == "" || cfg.TenantID == "" || secret == "" {
		return credentials{}, errors.New("teams: installation config is missing app_id, tenant_id or secret")
	}
	return credentials{
		AppID:     cfg.AppID,
		TenantID:  cfg.TenantID,
		BotName:   cfg.BotName,
		AppSecret: secret,
	}, nil
}

// PublicConfig is the non-secret subset of an installation config, safe to
// surface on the management API (the encrypted secret is never included).
type PublicConfig struct {
	AppID    string
	TenantID string
	BotName  string
}

// DecodePublicConfig extracts the display-safe fields from a stored config
// blob. A decode miss yields a zero PublicConfig rather than an error so the
// management list still renders the row.
func DecodePublicConfig(raw []byte) PublicConfig {
	var cfg installConfig
	_ = json.Unmarshal(raw, &cfg)
	return PublicConfig{AppID: cfg.AppID, TenantID: cfg.TenantID, BotName: cfg.BotName}
}

// decryptSecret base64-decodes the stored ciphertext (tolerating MIME newline
// wrapping) and runs it through the injected Decrypter.
func decryptSecret(enc string, decrypt Decrypter) (string, error) {
	if enc == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(stripWhitespace(enc))
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if decrypt == nil {
		return string(ciphertext), nil
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeGUID lower-cases and trims a GUID-shaped id (App ID, tenant ID) and
// reports whether it has the 8-4-4-4-12 hex shape. Entra ids are GUIDs; the
// routing slot and the token audience compare them as strings, so they are
// stored in one canonical form.
func normalizeGUID(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 36 {
		return s, false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return s, false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return s, false
			}
		}
	}
	return s, true
}
