package teams

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"strings"
)

// This file builds the Teams app package (manifest.json + two icons, zipped)
// an admin uploads in the Teams admin center so people can add the bot. It is
// generated per installation so the bot id, name and domain are always right.
//
// Schema: https://learn.microsoft.com/microsoftteams/platform/resources/schema/manifest-schema

const manifestSchemaVersion = "1.17"

// ManifestParams describe one installation's Teams app.
type ManifestParams struct {
	AppID     string // Microsoft App ID of the Azure Bot
	BotName   string // display name in Teams (≤ 30 chars)
	AgentName string // the Multica agent behind the bot
	AppURL    string // Multica web app origin (https)
	Version   string // app package version, e.g. "1.0.0"
}

type manifest struct {
	Schema          string        `json:"$schema"`
	ManifestVersion string        `json:"manifestVersion"`
	Version         string        `json:"version"`
	ID              string        `json:"id"`
	Developer       manifestDev   `json:"developer"`
	Name            manifestText  `json:"name"`
	Description     manifestText  `json:"description"`
	Icons           manifestIcons `json:"icons"`
	AccentColor     string        `json:"accentColor"`
	Bots            []manifestBot `json:"bots"`
	Permissions     []string      `json:"permissions"`
	ValidDomains    []string      `json:"validDomains"`
}

type manifestDev struct {
	Name          string `json:"name"`
	WebsiteURL    string `json:"websiteUrl"`
	PrivacyURL    string `json:"privacyUrl"`
	TermsOfUseURL string `json:"termsOfUseUrl"`
}

type manifestText struct {
	Short string `json:"short"`
	Full  string `json:"full"`
}

type manifestIcons struct {
	Color   string `json:"color"`
	Outline string `json:"outline"`
}

type manifestBot struct {
	BotID              string                `json:"botId"`
	Scopes             []string              `json:"scopes"`
	SupportsFiles      bool                  `json:"supportsFiles"`
	IsNotificationOnly bool                  `json:"isNotificationOnly"`
	CommandLists       []manifestCommandList `json:"commandLists"`
}

type manifestCommandList struct {
	Scopes   []string          `json:"scopes"`
	Commands []manifestCommand `json:"commands"`
}

type manifestCommand struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

const accentColor = "#4F46E5"

// BuildAppPackage returns the zipped Teams app package.
func BuildAppPackage(p ManifestParams) ([]byte, error) {
	appID, ok := normalizeGUID(p.AppID)
	if !ok {
		return nil, ErrInvalidAppID
	}
	u, err := url.Parse(strings.TrimRight(p.AppURL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("teams: app package needs an https MULTICA_APP_URL, got %q", p.AppURL)
	}
	origin := u.Scheme + "://" + u.Host
	name := strings.TrimSpace(p.BotName)
	if name == "" {
		name = strings.TrimSpace(p.AgentName)
	}
	if name == "" {
		name = "Multica agent"
	}
	if r := []rune(name); len(r) > 30 {
		name = string(r[:30])
	}
	version := p.Version
	if version == "" {
		version = "1.0.0"
	}
	agent := strings.TrimSpace(p.AgentName)
	if agent == "" {
		agent = name
	}
	full := fmt.Sprintf("Chat with the Multica agent \"%s\" from Teams. Ask questions, hand it work, or create issues with /issue.", agent)
	if r := []rune(full); len(r) > 4000 {
		full = string(r[:4000])
	}

	m := manifest{
		Schema:          "https://developer.microsoft.com/en-us/json-schemas/teams/v" + manifestSchemaVersion + "/MicrosoftTeams.schema.json",
		ManifestVersion: manifestSchemaVersion,
		Version:         version,
		// The app id may equal the bot id; one GUID per bot keeps re-uploads
		// of a regenerated package an update rather than a second app.
		ID: appID,
		Developer: manifestDev{
			Name:          "Multica",
			WebsiteURL:    origin,
			PrivacyURL:    origin + "/privacy",
			TermsOfUseURL: origin + "/licensing",
		},
		Name:        manifestText{Short: name, Full: name + " (Multica)"},
		Description: manifestText{Short: "Multica agent in Teams", Full: full},
		Icons:       manifestIcons{Color: "color.png", Outline: "outline.png"},
		AccentColor: accentColor,
		Bots: []manifestBot{{
			BotID:  appID,
			Scopes: []string{"personal", "team", "groupChat"},
			CommandLists: []manifestCommandList{{
				Scopes: []string{"personal", "team", "groupChat"},
				Commands: []manifestCommand{
					{Title: "/issue", Description: "Create an issue assigned to this agent"},
					{Title: "/new", Description: "Start a new Multica chat"},
					{Title: "/clear", Description: "Run the next message without previous context"},
				},
			}},
		}},
		Permissions:  []string{"identity", "messageTeamMembers"},
		ValidDomains: []string{u.Host},
	}
	manifestJSON, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	colorPNG, err := renderColorIcon()
	if err != nil {
		return nil, err
	}
	outlinePNG, err := renderOutlineIcon()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"manifest.json", manifestJSON},
		{"color.png", colorPNG},
		{"outline.png", outlinePNG},
	} {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderColorIcon draws the 192×192 full-color icon: an accent background
// with a white "M"-like glyph made of three bars.
func renderColorIcon() ([]byte, error) {
	const size = 192
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{0x4F, 0x46, 0xE5, 0xFF}
	fg := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	fill(img, 0, 0, size, size, bg)
	drawGlyph(img, size, fg)
	return encodePNG(img)
}

// renderOutlineIcon draws the 32×32 outline icon: white on transparent, as
// the Teams schema requires.
func renderOutlineIcon() ([]byte, error) {
	const size = 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	drawGlyph(img, size, color.RGBA{0xFF, 0xFF, 0xFF, 0xFF})
	return encodePNG(img)
}

// drawGlyph draws a simple "M": two outer bars and a shorter middle bar.
func drawGlyph(img *image.RGBA, size int, c color.RGBA) {
	pad := size / 4
	bar := size / 8
	top, bottom := pad, size-pad
	left, right := pad, size-pad-bar
	mid := (size - bar) / 2
	fill(img, left, top, left+bar, bottom, c)
	fill(img, right, top, right+bar, bottom, c)
	fill(img, mid, top+size/8, mid+bar, bottom-size/8, c)
	fill(img, left, top, right+bar, top+bar, c)
}

func fill(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
