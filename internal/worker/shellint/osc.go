package shellint

import (
	"bytes"
	"strconv"
	"strings"
)

// MarkKind identifies a shell-integration mark.
type MarkKind byte

const (
	MarkPromptStart  MarkKind = 'A' // OSC 133;A
	MarkPromptEnd    MarkKind = 'B' // OSC 133;B
	MarkPreExec      MarkKind = 'C' // OSC 133;C
	MarkCommandDone  MarkKind = 'D' // OSC 133;D[;exit]
	MarkCommandLine  MarkKind = 'E' // OSC 633;E;<cmd>;<nonce>
	MarkCwd          MarkKind = 'P' // OSC 633;P;Cwd=<path>;<nonce>
	maxOSCPayload             = 256 << 10
	konsertOSCPrefix          = "633;"
)

// Mark is one parsed shell-integration mark.
type Mark struct {
	Kind MarkKind
	// Value is the decoded command line (E) or path (P).
	Value string
	// ExitCode is the status of D; HasExit is false when D carried none.
	ExitCode int
	HasExit  bool
	// Nonce is the trailing nonce field of E and P.
	Nonce string
}

type oscState uint8

const (
	stGround   oscState = iota
	stEsc               // saw ESC
	stOSC               // inside ESC ] ... ; deciding or passing through
	stOSCEsc            // inside an OSC, saw ESC (maybe ST)
	stOSCSkip           // over-long OSC being passed through without capture
	stOSCSkipE          // over-long OSC, saw ESC
)

// Parser extracts OSC 133 and OSC 633 marks from a terminal byte stream. It is
// incremental: sequences split across Feed calls are handled. OSC 633 sequences are
// removed from the returned bytes; everything else passes through unchanged.
//
// To keep latency low, bytes are held back only while they may still be the start of an
// OSC 633 sequence (a trailing ESC, "ESC ]" plus a partial "633;" prefix) or are inside
// one. An OSC longer than 256 KiB is passed through uninterpreted.
type Parser struct {
	st      oscState
	payload []byte // OSC payload so far (between "ESC ]" and the terminator)
	strip   bool   // the current OSC is a 633 sequence: hold and drop it
	held    bool   // bytes from "ESC" onward are being held (not yet emitted)
}

// Feed consumes chunk and returns the bytes to forward to the terminal (sharing no
// memory with chunk) and any complete marks.
func (p *Parser) Feed(chunk []byte) ([]byte, []Mark) {
	out := make([]byte, 0, len(chunk)+8)
	var marks []Mark
	i := 0
	for i < len(chunk) {
		switch p.st {
		case stGround:
			j := bytes.IndexByte(chunk[i:], 0x1b)
			if j < 0 {
				out = append(out, chunk[i:]...)
				return out, marks
			}
			out = append(out, chunk[i:i+j]...)
			i += j + 1
			p.st, p.held = stEsc, true
		case stEsc:
			b := chunk[i]
			if b == ']' {
				i++
				p.st, p.payload, p.strip = stOSC, p.payload[:0], false
				continue
			}
			// Not an OSC: release the ESC, reprocess b in ground (it may be another ESC).
			out = append(out, 0x1b)
			p.st, p.held = stGround, false
		case stOSC:
			b := chunk[i]
			i++
			switch b {
			case 0x07:
				out = p.finish(out, &marks, []byte{0x07})
			case 0x1b:
				p.st = stOSCEsc
			case 0x18, 0x1a: // CAN/SUB abort the sequence
				out = p.abort(out, b)
			default:
				out = p.addPayload(out, b)
			}
		case stOSCEsc:
			b := chunk[i]
			if b == '\\' {
				i++
				out = p.finish(out, &marks, []byte{0x1b, '\\'})
				continue
			}
			// ESC not followed by "\" ends the OSC (as in xterm) and starts a new sequence.
			out = p.abort(out, 0)
			p.st, p.held = stEsc, true
		case stOSCSkip:
			j := bytes.IndexAny(chunk[i:], "\x07\x1b\x18\x1a")
			if j < 0 {
				out = append(out, chunk[i:]...)
				return out, marks
			}
			b := chunk[i+j]
			out = append(out, chunk[i:i+j]...)
			i += j + 1
			if b == 0x1b {
				p.st = stOSCSkipE
			} else {
				out = append(out, b)
				p.st = stGround
			}
		case stOSCSkipE:
			if chunk[i] == '\\' {
				out = append(out, 0x1b, '\\')
				i++
				p.st = stGround
				continue
			}
			// The ESC ends the skipped OSC and starts a new sequence.
			p.st, p.held = stEsc, true
		}
	}
	return out, marks
}

// Flush returns bytes still held back (a partial sequence) at end of stream. A partial
// OSC 633 sequence is dropped.
func (p *Parser) Flush() []byte {
	var out []byte
	switch p.st {
	case stEsc:
		out = []byte{0x1b}
	case stOSC, stOSCEsc:
		if p.held && !p.strip {
			out = p.raw()
			if p.st == stOSCEsc {
				out = append(out, 0x1b)
			}
		}
	case stOSCSkipE:
		out = []byte{0x1b}
	}
	p.st, p.held, p.strip, p.payload = stGround, false, false, p.payload[:0]
	return out
}

// raw re-creates the held bytes of the current OSC.
func (p *Parser) raw() []byte {
	return append([]byte{0x1b, ']'}, p.payload...)
}

func (p *Parser) addPayload(out []byte, b byte) []byte {
	if len(p.payload) >= maxOSCPayload {
		// Too long to be ours: pass it through and stop interpreting it.
		if p.held && !p.strip {
			out = append(out, p.raw()...)
		}
		if !p.strip {
			out = append(out, b)
		}
		p.held, p.payload = false, p.payload[:0]
		if p.strip {
			// An over-long 633 sequence is still dropped, but no longer buffered.
			p.payload = append(p.payload[:0], konsertOSCPrefix...)
			return out
		}
		p.st = stOSCSkip
		return out
	}
	p.payload = append(p.payload, b)
	if p.held && !p.strip {
		n := len(p.payload)
		if n <= len(konsertOSCPrefix) && p.payload[n-1] == konsertOSCPrefix[n-1] {
			if n == len(konsertOSCPrefix) {
				p.strip = true
			}
			return out
		}
		// Not a 633 sequence: release what was held and pass the rest straight through.
		out = append(out, p.raw()...)
		p.held = false
		return out
	}
	if !p.strip {
		out = append(out, b)
	}
	return out
}

// finish handles a terminated OSC.
func (p *Parser) finish(out []byte, marks *[]Mark, term []byte) []byte {
	if !p.strip {
		if p.held {
			out = append(out, p.raw()...)
		}
		out = append(out, term...)
	}
	if m, ok := parseMark(p.payload, p.strip); ok {
		*marks = append(*marks, m)
	}
	p.st, p.held, p.strip, p.payload = stGround, false, false, p.payload[:0]
	return out
}

// abort ends the current OSC without a terminator (CAN/SUB, or an ESC that starts a
// new sequence). The aborting byte (if non-zero) is forwarded unless the OSC was ours.
func (p *Parser) abort(out []byte, b byte) []byte {
	if !p.strip {
		if p.held {
			out = append(out, p.raw()...)
		}
		if b != 0 {
			out = append(out, b)
		}
	}
	p.st, p.held, p.strip, p.payload = stGround, false, false, p.payload[:0]
	return out
}

// parseMark interprets an OSC payload. Over-long 633 payloads have been truncated to
// the bare prefix and parse to nothing.
func parseMark(payload []byte, konsert bool) (Mark, bool) {
	s := string(payload)
	if konsert {
		rest := strings.TrimPrefix(s, konsertOSCPrefix)
		switch {
		case strings.HasPrefix(rest, "E;"):
			fields := strings.Split(rest[2:], ";")
			if len(fields) < 2 {
				return Mark{}, false
			}
			return Mark{Kind: MarkCommandLine, Value: Unescape(fields[0]), Nonce: fields[len(fields)-1]}, true
		case strings.HasPrefix(rest, "P;Cwd="):
			fields := strings.Split(rest[len("P;Cwd="):], ";")
			if len(fields) < 2 {
				return Mark{}, false
			}
			return Mark{Kind: MarkCwd, Value: Unescape(fields[0]), Nonce: fields[len(fields)-1]}, true
		}
		return Mark{}, false
	}
	rest, ok := strings.CutPrefix(s, "133;")
	if !ok || rest == "" {
		return Mark{}, false
	}
	kind := MarkKind(rest[0])
	if len(rest) > 1 && rest[1] != ';' {
		return Mark{}, false
	}
	switch kind {
	case MarkPromptStart, MarkPromptEnd, MarkPreExec:
		return Mark{Kind: kind}, true
	case MarkCommandDone:
		m := Mark{Kind: kind}
		if len(rest) > 2 {
			arg, _, _ := strings.Cut(rest[2:], ";")
			if n, err := strconv.Atoi(arg); err == nil {
				m.ExitCode, m.HasExit = n, true
			}
		}
		return m, true
	}
	return Mark{}, false
}

// Unescape decodes a VS Code-style escaped OSC value ("\\" and "\xNN"). Invalid UTF-8
// is replaced.
func Unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		switch n := s[i+1]; {
		case n == '\\':
			b.WriteByte('\\')
			i++
		case n == 'x' && i+3 < len(s) && isHex(s[i+2]) && isHex(s[i+3]):
			v, _ := strconv.ParseUint(s[i+2:i+4], 16, 8)
			b.WriteByte(byte(v))
			i += 3
		default:
			b.WriteByte(c)
		}
	}
	return strings.ToValidUTF8(b.String(), "�")
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
