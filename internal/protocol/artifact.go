package protocol

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Inspect kinds. digest and transcript are served by the hub; the rest are read
// from the daemon that ran the work (edge principle).
const (
	InspectKindDigest     = "digest"
	InspectKindDiffStat   = "diff_stat"
	InspectKindDiff       = "diff"
	InspectKindStderr     = "stderr"
	InspectKindLog        = "log"
	InspectKindTranscript = "transcript"
	InspectKindFile       = "file"
	InspectKindResult     = "result"
	InspectKindEvents     = "events"
	InspectKindAudit      = "audit"
	// InspectKindProfile returns the work profiles of a target's work key (or of the
	// work key given as the target), per machine class.
	InspectKindProfile = "profile"
	// InspectKindPlacement returns where a task or ticket was placed and why (SYR-QY2TG).
	InspectKindPlacement = "placement"

	// InspectDefaultMaxLines is the page size when the caller omits max_lines.
	InspectDefaultMaxLines = 200
	// InspectMaxLinesLimit is the hard ceiling on max_lines.
	InspectMaxLinesLimit = 2000
	// InspectByteCap is the hard byte ceiling of one artifact page, enforced where
	// the bytes are produced (the daemon for edge artifacts, the hub for transcripts).
	InspectByteCap = 64 * 1024
)

// ArtifactFetchRequest is the hub → daemon request for one edge artifact.
type ArtifactFetchRequest struct {
	TaskID   string `json:"task_id,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	Kind     string `json:"kind"`
	Workdir  string `json:"workdir,omitempty"`
	Path     string `json:"path,omitempty"`
	MaxLines int    `json:"max_lines,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	// Base, for diff and diff_stat, diffs the working tree against this commit (e.g. the
	// worktree's base SHA, covering the agent's commits too) instead of HEAD.
	Base string `json:"base,omitempty"`
}

// ArtifactFetchResult is one capped page of an artifact.
type ArtifactFetchResult struct {
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"next_offset,omitempty"`
	MaxLines   int    `json:"max_lines"`
	Note       string `json:"note,omitempty"`
	Error      string `json:"error,omitempty"`
}

// TextPage is one capped slice of text. NextOffset is the offset to pass to
// fetch the following page when Truncated is set.
type TextPage struct {
	Text       string
	Truncated  bool
	Offset     int
	NextOffset int
	MaxLines   int
	Note       string
}

// ClampInspectMaxLines applies the default (200) and the hard maximum (2000).
func ClampInspectMaxLines(n int) int {
	if n <= 0 {
		return InspectDefaultMaxLines
	}
	if n > InspectMaxLinesLimit {
		return InspectMaxLinesLimit
	}
	return n
}

// PageText returns one page of s. See PageReader.
func PageText(s string, maxLines, offset int) TextPage {
	return PageReader(strings.NewReader(s), maxLines, offset)
}

// PageReader returns lines [offset, offset+maxLines) of r, stopping early so the
// page is at most InspectByteCap bytes. A truncated page says so and gives the
// next line offset. A single line longer than the byte cap is cut and counted
// as consumed, so the next page continues after it.
func PageReader(r io.Reader, maxLines, offset int) TextPage {
	maxLines = ClampInspectMaxLines(maxLines)
	if offset < 0 {
		offset = 0
	}
	page := TextPage{Offset: offset, MaxLines: maxLines}
	br := bufio.NewReader(r)
	var b strings.Builder
	taken := 0
	skipped := 0
	for {
		line, err := br.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			break
		}
		if skipped < offset {
			skipped++
			if err != nil {
				break
			}
			continue
		}
		room := InspectByteCap - b.Len()
		if len(line) > room {
			if taken == 0 && room > 0 {
				b.Write(line[:room])
				taken++
			}
			page.Truncated = true
			break
		}
		b.Write(line)
		taken++
		if err != nil {
			break
		}
		if taken >= maxLines {
			if _, peekErr := br.Peek(1); peekErr == nil {
				page.Truncated = true
			}
			break
		}
	}
	page.Text = b.String()
	if page.Truncated {
		page.NextOffset = offset + taken
		page.Note = fmt.Sprintf("truncated; call inspect again with offset=%d", page.NextOffset)
	}
	return page
}

// ToArtifact copies a page into a daemon reply.
func (p TextPage) ToArtifact(kind string) ArtifactFetchResult {
	return ArtifactFetchResult{
		Kind:       kind,
		Text:       p.Text,
		Truncated:  p.Truncated,
		Offset:     p.Offset,
		NextOffset: p.NextOffset,
		MaxLines:   p.MaxLines,
		Note:       p.Note,
	}
}
