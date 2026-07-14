package maxclient

import "strings"

// normalizeMessageText adapts the editor's standard Markdown headings to the
// smaller MAX Markdown dialect. MAX documents a single heading marker (`#`),
// so legacy levels 2-6 are flattened without changing the saved draft.
func normalizeMessageText(text string, format Format) string {
	if format != FormatMarkdown || !strings.Contains(text, "##") {
		return text
	}

	lines := strings.Split(text, "\n")
	inFence := false
	var fenceMarker byte
	var fenceLength int
	for index, line := range lines {
		marker, length, rest, fence := markdownFence(line)
		if inFence {
			if fence && marker == fenceMarker && length >= fenceLength && strings.TrimSpace(rest) == "" {
				inFence = false
			}
			continue
		}
		if fence {
			// A backtick info string cannot itself contain a backtick. Treating
			// such a line as ordinary text follows fenced-code Markdown rules.
			if marker != '`' || !strings.Contains(rest, "`") {
				inFence, fenceMarker, fenceLength = true, marker, length
			}
			continue
		}
		lines[index] = normalizeLegacyHeading(line)
	}
	return strings.Join(lines, "\n")
}

func normalizeLegacyHeading(line string) string {
	markerLength := 0
	for markerLength < len(line) && line[markerLength] == '#' {
		markerLength++
	}
	if markerLength < 2 || markerLength > 6 {
		return line
	}
	if markerLength < len(line) {
		next := line[markerLength]
		if next != ' ' && next != '\t' && next != '\r' {
			return line
		}
	}
	return "#" + line[markerLength:]
}

func markdownFence(line string) (marker byte, length int, rest string, ok bool) {
	line = strings.TrimSuffix(line, "\r")
	offset := 0
	for offset < len(line) && line[offset] == ' ' && offset < 4 {
		offset++
	}
	if offset > 3 || offset >= len(line) || (line[offset] != '`' && line[offset] != '~') {
		return 0, 0, "", false
	}
	marker = line[offset]
	end := offset
	for end < len(line) && line[end] == marker {
		end++
	}
	if end-offset < 3 {
		return 0, 0, "", false
	}
	return marker, end - offset, line[end:], true
}
