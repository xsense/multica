package teams

// Inbound media: pictures pasted into any chat, and files sent in a personal
// chat. Both come without Graph consent:
//
//   - A pasted picture arrives as an attachment whose contentUrl lives on the
//     Connector's own attachment store (smba.trafficmanager.net, or the
//     *.asm.skype.com object store). Those URLs take the bot's Connector
//     token.
//   - A file sent to the bot in a personal chat (manifest supportsFiles: true)
//     is uploaded to the sender's OneDrive first and arrives as
//     "application/vnd.microsoft.teams.file.download.info" with a short-lived,
//     pre-authenticated SharePoint downloadUrl. No token is sent there.
//
// Files posted in a channel or group chat are only references to SharePoint
// and need Graph permissions this integration deliberately does not ask for;
// the sender is told to use the personal chat instead.
//
// The engine contract is the same as Telegram's: the adapter puts a
// placeholder into the message body and lists the media in Raw; the Router
// calls ResolveMedia off the ACK path, which downloads each file, writes an
// intent-ledger row BEFORE uploading it to object storage, and returns
// MediaRefs the engine binds as chat attachments (replacing the placeholder
// with a link).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// maxInboundMediaBytes caps one downloaded file.
	maxInboundMediaBytes = 25 << 20
	// maxInboundMediaPerMessage caps how many files one message carries in.
	maxInboundMediaPerMessage = 10

	fileDownloadInfoType = "application/vnd.microsoft.teams.file.download.info"
)

// inboundMedia is what ResolveMedia needs to fetch one file.
type inboundMedia struct {
	Kind channel.MsgType `json:"kind"`
	URL  string          `json:"url"`
	// BotAuth: fetch with the bot's Connector token (Connector-hosted
	// pictures). False for pre-authenticated SharePoint download links.
	BotAuth  bool   `json:"bot_auth,omitempty"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	UniqueID string `json:"unique_id,omitempty"`
	// Placeholder is the marker in the body the bound attachment replaces;
	// PlaceholderIndex is which occurrence of it is this file's.
	Placeholder      string `json:"placeholder"`
	PlaceholderIndex int    `json:"placeholder_index,omitempty"`
}

type fileDownloadInfo struct {
	DownloadURL string `json:"downloadUrl"`
	UniqueID    string `json:"uniqueId"`
	FileType    string `json:"fileType"`
}

// extractMedia lists the downloadable media in an activity, and reports
// whether it also carried a file this integration cannot fetch.
func extractMedia(a Activity) (media []inboundMedia, unsupported bool) {
	seen := map[string]bool{}
	for _, att := range a.Attachments {
		ct := strings.ToLower(strings.TrimSpace(att.ContentType))
		switch {
		case ct == "text/html", ct == "application/vnd.microsoft.card.adaptive", strings.HasPrefix(ct, "application/vnd.microsoft.card."):
			continue // the message body / cards, not a file
		case ct == fileDownloadInfoType:
			var info fileDownloadInfo
			if len(att.Content) == 0 || json.Unmarshal(att.Content, &info) != nil || !sharePointDownloadURL(info.DownloadURL) {
				unsupported = true
				continue
			}
			if seen[info.DownloadURL] {
				continue
			}
			seen[info.DownloadURL] = true
			name := cleanFilename(att.Name)
			media = append(media, inboundMedia{
				Kind:        kindForName(name, info.FileType),
				URL:         info.DownloadURL,
				FileName:    name,
				UniqueID:    info.UniqueID,
				Placeholder: "[File: " + firstNonEmpty(name, "attachment") + "]",
			})
		case strings.HasPrefix(ct, "image/"):
			if !botAttachmentURL(att.ContentURL) {
				// e.g. a Graph hostedContents URL in a channel: needs Graph.
				unsupported = true
				continue
			}
			if seen[att.ContentURL] {
				continue
			}
			seen[att.ContentURL] = true
			mt := ct
			if mt == "image/*" {
				mt = ""
			}
			media = append(media, inboundMedia{
				Kind:        channel.MsgTypeImage,
				URL:         att.ContentURL,
				BotAuth:     true,
				FileName:    cleanFilename(att.Name),
				MimeType:    mt,
				Placeholder: "[Image]",
			})
		case ct == "application/vnd.microsoft.teams.file.download.info.v2",
			ct == "reference",
			strings.HasPrefix(ct, "application/vnd.microsoft.teams.file"),
			att.ContentURL != "":
			// A SharePoint reference posted in a channel / group chat.
			unsupported = true
		}
	}
	if len(media) > maxInboundMediaPerMessage {
		media = media[:maxInboundMediaPerMessage]
	}
	return media, unsupported
}

// composeMediaBody puts each file's placeholder above the sender's text, so
// the text reads as the caption once the engine swaps the markers for
// attachment links, and records which occurrence of each marker is whose.
func composeMediaBody(media []inboundMedia, text string) string {
	counts := map[string]int{}
	lines := make([]string, 0, len(media)+1)
	for i := range media {
		media[i].PlaceholderIndex = counts[media[i].Placeholder]
		counts[media[i].Placeholder]++
		lines = append(lines, media[i].Placeholder)
	}
	if strings.TrimSpace(text) != "" {
		lines = append(lines, text)
	}
	return strings.Join(lines, "\n")
}

func mediaMsgType(media []inboundMedia) channel.MsgType {
	for _, m := range media {
		if m.Kind != channel.MsgTypeImage {
			return channel.MsgTypeFile
		}
	}
	return channel.MsgTypeImage
}

func kindForName(name, fileType string) channel.MsgType {
	ext := strings.ToLower(strings.TrimPrefix(firstNonEmpty(fileType, strings.TrimPrefix(path.Ext(name), ".")), "."))
	switch ext {
	case "png", "jpg", "jpeg", "gif", "webp", "bmp", "heic":
		return channel.MsgTypeImage
	case "mp4", "mov", "webm":
		return channel.MsgTypeVideo
	case "mp3", "m4a", "wav", "ogg":
		return channel.MsgTypeAudio
	}
	return channel.MsgTypeFile
}

// botAttachmentURL: Connector-hosted picture URLs, the only hosts the bot's
// Connector token is ever sent to for a download.
func botAttachmentURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Port() != "" || u.User != nil {
		return allowInsecureServiceURLForTests && err == nil && u.Scheme == "http"
	}
	host := strings.ToLower(u.Hostname())
	return trustedServiceHosts[host] || strings.HasSuffix(host, ".asm.skype.com")
}

// sharePointDownloadURL: the pre-authenticated OneDrive/SharePoint links
// Teams hands out for personal-chat files. Fetched without credentials.
func sharePointDownloadURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Port() != "" || u.User != nil {
		return allowInsecureServiceURLForTests && err == nil && u.Scheme == "http"
	}
	host := strings.ToLower(u.Hostname())
	return strings.HasSuffix(host, ".sharepoint.com")
}

// ---- resolver ----

// objectStore is the slice of storage.Storage the media paths need.
type objectStore interface {
	Upload(ctx context.Context, key string, data []byte, contentType string, filename string) (string, error)
	ObjectURL(key string) string
	KeyFromURL(rawURL string) string
	GetReader(ctx context.Context, key string) (io.ReadCloser, error)
}

type mediaResolver struct {
	transport *Transport
	storage   objectStore
	ledger    engine.MediaIntentLedger
	client    *http.Client
	logger    *slog.Logger
}

var _ engine.MediaResolver = (*mediaResolver)(nil)

// NewMediaResolver builds the Teams media resolver.
func NewMediaResolver(transport *Transport, storage objectStore, ledger engine.MediaIntentLedger, logger *slog.Logger) engine.MediaResolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &mediaResolver{
		transport: transport,
		storage:   storage,
		ledger:    ledger,
		client:    newMediaClient(nil),
		logger:    logger,
	}
}

// newMediaClient follows redirects only to hosts the original request could
// have gone to; Go drops the Authorization header on a cross-host hop.
func newMediaClient(base *http.Client) *http.Client {
	c := &http.Client{Timeout: 60 * time.Second}
	if base != nil {
		c.Transport = base.Transport
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if !botAttachmentURL(req.URL.String()) && !sharePointDownloadURL(req.URL.String()) {
			return fmt.Errorf("redirect to untrusted host %q", req.URL.Hostname())
		}
		return nil
	}
	return c
}

func (r *mediaResolver) HasMedia(msg channel.InboundMessage) bool {
	raw, err := decodeRaw(msg)
	return err == nil && len(raw.Media) > 0
}

func (r *mediaResolver) ResolveMedia(ctx context.Context, inst engine.ResolvedInstallation, _ engine.ResolvedIdentity, _ pgtype.UUID, chatMessageID pgtype.UUID, msg channel.InboundMessage) channel.InboundMessage {
	raw, err := decodeRaw(msg)
	if err != nil || len(raw.Media) == 0 {
		return msg
	}
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		r.logger.Warn("teams media: installation row unavailable", "message_id", msg.MessageID)
		return msg
	}
	conn, creds, err := r.transport.connectorFor(row.Config)
	if err != nil {
		r.logger.Warn("teams media: decode credentials failed", "error", err)
		return msg
	}
	failed := false
	for i, m := range raw.Media {
		ref, err := r.ingest(ctx, inst, creds, chatMessageID, i, m)
		if err != nil {
			r.logger.Warn("teams media: file not ingested", "message_id", msg.MessageID, "error", err)
			failed = true
			continue
		}
		msg.MediaRefs = append(msg.MediaRefs, ref)
	}
	if failed {
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := conn.SendMessage(nctx, raw.ServiceURL, firstNonEmpty(raw.ConversationID, msg.Source.ChatID), msgMediaUnavailable, ""); err != nil {
			r.logger.Warn("teams media: unavailable notice failed", "error", err)
		}
	}
	return msg
}

func (r *mediaResolver) ingest(ctx context.Context, inst engine.ResolvedInstallation, creds credentials, chatMessageID pgtype.UUID, index int, m inboundMedia) (channel.MediaRef, error) {
	key := mediaObjectKey(inst, chatMessageID, index, m)
	link := r.storage.ObjectURL(key)
	owned, err := r.ledger.RecordPendingMediaObject(ctx, engine.RecordPendingMediaObjectParams{
		StorageKey:     key,
		WorkspaceID:    inst.WorkspaceID,
		ChatMessageID:  chatMessageID,
		StorageURL:     link,
		InstallationID: inst.ID,
	})
	if err != nil {
		return channel.MediaRef{}, fmt.Errorf("record media intent: %w", err)
	}
	if !owned {
		return channel.MediaRef{}, errors.New("media key owned by reconciler")
	}
	data, responseType, err := r.download(ctx, creds, m)
	if err != nil {
		return channel.MediaRef{}, err
	}
	contentType := mediaContentType(m.MimeType, responseType, data)
	filename := mediaFilename(m, contentType)
	if _, err := r.storage.Upload(ctx, key, data, contentType, filename); err != nil {
		return channel.MediaRef{}, fmt.Errorf("upload file: %w", err)
	}
	kind := m.Kind
	if kind == channel.MsgTypeFile && strings.HasPrefix(contentType, "image/") {
		kind = channel.MsgTypeImage
	}
	return channel.MediaRef{
		Type:              kind,
		StorageKey:        key,
		StorageURL:        link,
		Filename:          filename,
		MimeType:          contentType,
		SizeBytes:         int64(len(data)),
		InlinePlaceholder: m.Placeholder,
		InlineIndex:       m.PlaceholderIndex,
	}, nil
}

func (r *mediaResolver) download(ctx context.Context, creds credentials, m inboundMedia) ([]byte, string, error) {
	switch {
	case m.BotAuth && botAttachmentURL(m.URL):
	case !m.BotAuth && sharePointDownloadURL(m.URL):
	default:
		return nil, "", errors.New("media URL host is not trusted")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return nil, "", err
	}
	if m.BotAuth {
		token, err := r.transport.tokens.Token(ctx, creds)
		if err != nil {
			return nil, "", fmt.Errorf("bot token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxInboundMediaBytes {
		return nil, "", fmt.Errorf("file is %d bytes, over the %d MiB limit", resp.ContentLength, maxInboundMediaBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInboundMediaBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("download: %w", err)
	}
	if len(data) > maxInboundMediaBytes {
		return nil, "", fmt.Errorf("file is over the %d MiB limit", maxInboundMediaBytes>>20)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// mediaObjectKey is keyed by the owning chat message and the file's position,
// so a re-ingested message never collides with its own earlier ledger row.
func mediaObjectKey(inst engine.ResolvedInstallation, chatMessageID pgtype.UUID, index int, m inboundMedia) string {
	sum := sha256.Sum256([]byte(util.UUIDToString(chatMessageID) + "\x00" + strconv.Itoa(index) + "\x00" + firstNonEmpty(m.UniqueID, m.URL)))
	return path.Join("workspaces", util.UUIDToString(inst.WorkspaceID), "teams", util.UUIDToString(inst.ID), hex.EncodeToString(sum[:]))
}

func mediaContentType(declared, responseType string, data []byte) string {
	clean := func(ct string) string {
		ct = strings.ToLower(strings.TrimSpace(ct))
		if semi := strings.IndexByte(ct, ';'); semi >= 0 {
			ct = strings.TrimSpace(ct[:semi])
		}
		return ct
	}
	if ct := clean(declared); ct != "" && ct != "image/*" {
		return ct
	}
	if ct := clean(responseType); ct != "" && ct != "application/octet-stream" {
		return ct
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	return clean(http.DetectContentType(sniff))
}

func mediaFilename(m inboundMedia, contentType string) string {
	name := cleanFilename(m.FileName)
	if name == "" {
		if m.Kind == channel.MsgTypeImage {
			name = "teams-image"
		} else {
			name = "teams-file"
		}
	}
	if path.Ext(name) == "" {
		name += mediaExtension(contentType)
	}
	return name
}

func cleanFilename(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if strings.Trim(name, ".") == "" || name == "/" {
		return ""
	}
	return name
}

func mediaExtension(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	}
	if exts, err := mime.ExtensionsByType(contentType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}
