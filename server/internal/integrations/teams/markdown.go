package teams

import "strings"

// Teams renders a Markdown subset when textFormat is "markdown" (headings,
// bold/italic, lists, links, inline and fenced code, tables), which covers
// what agents write, so replies are sent as-is rather than converted.
//
// A single message is capped at roughly 28 KB including the envelope; long
// answers are split on paragraph, then line boundaries, never inside a
// fenced code block when it can be helped.

// maxMessageChars keeps each chunk well under the Teams payload cap even for
// multi-byte (Thai) text, which is 3 bytes per rune in UTF-8.
const maxMessageChars = 7000

// chunkMarkdown splits text into pieces of at most limit runes.
func chunkMarkdown(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if runeLen(text) <= limit {
		return []string{text}
	}
	var (
		out     []string
		current strings.Builder
		inFence bool
	)
	flush := func() {
		s := strings.TrimSpace(current.String())
		if s != "" {
			if inFence {
				// Close the fence in this chunk and reopen it in the next so
				// both render as code.
				s += "\n```"
			}
			out = append(out, s)
		}
		current.Reset()
		if inFence {
			current.WriteString("```\n")
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		for runeLen(line) > limit {
			// A single enormous line: hard-split it.
			head, tail := splitRunes(line, limit-runeLen(current.String()))
			if head == "" {
				flush()
				continue
			}
			current.WriteString(head)
			flush()
			line = tail
		}
		if runeLen(current.String())+runeLen(line) > limit {
			flush()
		}
		current.WriteString(line)
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
	}
	inFence = false
	flush()
	return out
}

func runeLen(s string) int { return len([]rune(s)) }

func splitRunes(s string, n int) (string, string) {
	if n <= 0 {
		return "", s
	}
	r := []rune(s)
	if n >= len(r) {
		return s, ""
	}
	return string(r[:n]), string(r[n:])
}
