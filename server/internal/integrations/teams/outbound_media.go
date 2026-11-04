package teams

// Outbound media: files the agent attached to its reply (`multica attachment
// upload`, bound to the assistant message by CompleteTask).
//
//   - Pictures (PNG, JPEG, GIF) go inline as base64 attachments, one message
//     each. Teams renders inline images up to 1024×1024 px and 1 MB, so a
//     larger still picture is scaled down and re-encoded first. Base64 image
//     bytes do not count toward the 100 KB bot message limit.
//   - Everything else (and any picture that cannot be sent) is listed in one
//     message with a link to the chat in Multica, where the file is attached.
//     Teams has no way for a bot to post a file into a channel without Graph
//     consent, and a link keeps the file behind Multica's own login.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	maxInlineImageBytes  = 1 << 20 // Teams inline image ceiling
	maxInlineImageSide   = 1024
	maxOutboundReadBytes = 30 << 20 // never read a larger object into memory
	attachmentBudget     = 3 * time.Minute
)

// deliverAttachments sends the reply's files after its text. Best effort:
// the files stay attached to the reply in Multica whatever happens here.
func (o *Outbound) deliverAttachments(parent context.Context, conn *connector, target replyTarget, e events.Event) {
	if o.objects == nil {
		return
	}
	messageID, err := util.ParseUUID(chatDoneMessageID(e.Payload))
	if err != nil || !messageID.Valid {
		return
	}
	workspaceID, err := util.ParseUUID(e.WorkspaceID)
	if err != nil || !workspaceID.Valid {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), attachmentBudget)
	defer cancel()
	rows, err := o.q.ListAttachmentsByChatMessage(ctx, db.ListAttachmentsByChatMessageParams{
		ChatMessageID: messageID, WorkspaceID: workspaceID,
	})
	if err != nil {
		o.logger.WarnContext(ctx, "teams outbound: attachment lookup failed", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	var linked []db.Attachment
	failed := false
	for _, row := range rows {
		if !isInlineImageType(row.ContentType) {
			linked = append(linked, row)
			continue
		}
		if err := o.sendInlineImage(ctx, conn, target, row); err != nil {
			o.logger.WarnContext(ctx, "teams outbound: inline image not sent; linking it instead",
				"attachment_id", util.UUIDToString(row.ID), "error", err)
			linked = append(linked, row)
		}
	}
	if len(linked) > 0 {
		text := o.fileLinkMessage(ctx, linked, workspaceID, e.ChatSessionID)
		if _, err := sendWithRetry(ctx, func(ctx context.Context) (string, error) {
			return conn.SendMessage(ctx, target.ServiceURL, target.ConversationID, text, "")
		}); err != nil {
			o.logger.WarnContext(ctx, "teams outbound: file list not sent", "error", err)
			failed = true
		}
	}
	if failed {
		_, _ = conn.SendMessage(ctx, target.ServiceURL, target.ConversationID, msgOutboundFilesFailed, "")
	}
}

func (o *Outbound) sendInlineImage(ctx context.Context, conn *connector, target replyTarget, row db.Attachment) error {
	if row.SizeBytes > maxOutboundReadBytes {
		return fmt.Errorf("attachment is %d bytes", row.SizeBytes)
	}
	data, err := o.readObject(ctx, row.Url)
	if err != nil {
		return err
	}
	data, contentType, err := fitInlineImage(data, row.ContentType)
	if err != nil {
		return err
	}
	name := cleanFilename(row.Filename)
	if name == "" {
		name = "image" + mediaExtension(contentType)
	}
	_, err = sendWithRetry(ctx, func(ctx context.Context) (string, error) {
		return conn.SendAttachments(ctx, target.ServiceURL, target.ConversationID, "", []outAttachment{{
			ContentType: contentType,
			ContentURL:  "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data),
			Name:        name,
		}})
	})
	return err
}

func (o *Outbound) readObject(ctx context.Context, rawURL string) ([]byte, error) {
	key := o.objects.KeyFromURL(rawURL)
	if key == "" {
		return nil, errors.New("attachment is not an object this deployment stores")
	}
	rc, err := o.objects.GetReader(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxOutboundReadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	if len(data) > maxOutboundReadBytes {
		return nil, errors.New("attachment is too large to send inline")
	}
	return data, nil
}

// fileLinkMessage lists files by name and size with one link to the chat in
// Multica (the files are attached to the reply there).
func (o *Outbound) fileLinkMessage(ctx context.Context, rows []db.Attachment, workspaceID pgtype.UUID, sessionID string) string {
	var b strings.Builder
	b.WriteString(msgOutboundFilesHeader)
	b.WriteString("\n")
	for _, row := range rows {
		name := cleanFilename(row.Filename)
		if name == "" {
			name = "attachment"
		}
		fmt.Fprintf(&b, "\n- %s (%s)", escapeMarkdown(name), humanSize(row.SizeBytes))
	}
	if link := o.chatLink(ctx, workspaceID, sessionID); link != "" {
		b.WriteString("\n\n")
		fmt.Fprintf(&b, msgOutboundFilesLink, link)
	}
	return b.String()
}

func (o *Outbound) chatLink(ctx context.Context, workspaceID pgtype.UUID, sessionID string) string {
	if o.appURL == "" || sessionID == "" {
		return ""
	}
	ws, err := o.q.GetWorkspace(ctx, workspaceID)
	if err != nil || ws.Slug == "" {
		return ""
	}
	return o.appURL + "/" + url.PathEscape(ws.Slug) + "/chat?session=" + url.QueryEscape(sessionID)
}

func isInlineImageType(ct string) bool {
	switch normalizeContentType(ct) {
	case "image/png", "image/jpeg", "image/gif":
		return true
	}
	return false
}

func normalizeContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if semi := strings.IndexByte(ct, ';'); semi >= 0 {
		ct = strings.TrimSpace(ct[:semi])
	}
	return ct
}

// fitInlineImage returns bytes Teams will render inline: unchanged when they
// already fit, otherwise a still picture scaled to ≤1024 px and re-encoded
// (PNG kept for transparency when it fits, else JPEG). An oversized GIF is
// not re-encoded (it may be animated) and is linked instead.
func fitInlineImage(data []byte, declared string) ([]byte, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("not a readable image: %w", err)
	}
	contentType := "image/" + format
	if format == "jpeg" || format == "png" || format == "gif" {
		if len(data) <= maxInlineImageBytes && cfg.Width <= maxInlineImageSide && cfg.Height <= maxInlineImageSide {
			return data, contentType, nil
		}
	} else {
		return nil, "", fmt.Errorf("format %q cannot be shown inline", format)
	}
	if format == "gif" {
		return nil, "", errors.New("gif is over the inline limits")
	}
	if cfg.Width*cfg.Height > 64<<20 {
		return nil, "", errors.New("image dimensions are too large to process")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}
	scaled := scaleToFit(img, maxInlineImageSide)
	if format == "png" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, scaled); err == nil && buf.Len() <= maxInlineImageBytes {
			return buf.Bytes(), "image/png", nil
		}
	}
	for _, q := range []int{85, 70, 55} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, flattenOnWhite(scaled), &jpeg.Options{Quality: q}); err != nil {
			return nil, "", fmt.Errorf("encode jpeg: %w", err)
		}
		if buf.Len() <= maxInlineImageBytes {
			return buf.Bytes(), "image/jpeg", nil
		}
	}
	_ = declared
	return nil, "", errors.New("image does not fit the inline size limit")
}

// scaleToFit shrinks img so its longer side is at most side, averaging the
// source pixels each destination pixel covers (box filter).
func scaleToFit(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= side && h <= side {
		return img
	}
	nw, nh := side, side
	if w >= h {
		nh = max(1, h*side/w)
	} else {
		nw = max(1, w*side/h)
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		sy0 := b.Min.Y + y*h/nh
		sy1 := max(sy0+1, b.Min.Y+(y+1)*h/nh)
		for x := 0; x < nw; x++ {
			sx0 := b.Min.X + x*w/nw
			sx1 := max(sx0+1, b.Min.X+(x+1)*w/nw)
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					cr, cg, cb, ca := img.At(sx, sy).RGBA()
					r, g, bl, a = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca)
					n++
				}
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r / n >> 8), G: uint8(g / n >> 8), B: uint8(bl / n >> 8), A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}

// flattenOnWhite composites a picture with transparency onto white for JPEG.
func flattenOnWhite(img image.Image) image.Image {
	b := img.Bounds()
	out := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			inv := 0xffff - a
			out.SetRGBA(x, y, color.RGBA{
				R: uint8((r + inv) >> 8), G: uint8((g + inv) >> 8), B: uint8((bl + inv) >> 8), A: 0xff,
			})
		}
	}
	return out
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// escapeMarkdown keeps a file name from turning into markup or a link.
func escapeMarkdown(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "`", "\\`", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// chatDoneMessageID pulls the assistant message id out of a chat:done payload.
func chatDoneMessageID(payload any) string {
	switch p := payload.(type) {
	case protocol.ChatDonePayload:
		return p.MessageID
	case map[string]any:
		if s, ok := p["message_id"].(string); ok {
			return s
		}
	}
	return ""
}

// Register the GIF decoder for image.DecodeConfig (png/jpeg are imported above).
var _ = gif.Decode
