package chat

// JSON helpers shared by the stdio protocol engines (claude stream-json, ACP and
// codex JSON-RPC) and the CLI probes. encoding/json/v2 rejects invalid UTF-8 and
// duplicate object names, but agent output (tool results, terminal output, file
// contents) is not guaranteed to be clean. A single bad byte or a repeated key
// must not drop a protocol line or fail a reply, so incoming lines are repaired
// before decoding, decoding tolerates both (last duplicate wins, as in v1), and
// outgoing messages are encoded with invalid bytes replaced by U+FFFD.
//
// Member names ARE matched case-sensitively (the v2 default): every wire struct
// carries the exact tag its protocol uses.

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"unicode/utf8"
)

var replacementChar = []byte("�")

// validUTF8 returns line unchanged when it is valid UTF-8, else a copy with
// each invalid sequence replaced by U+FFFD.
func validUTF8(line []byte) []byte {
	if utf8.Valid(line) {
		return line
	}
	return bytes.ToValidUTF8(line, replacementChar)
}

// scanValidUTF8Lines is bufio.ScanLines whose tokens are always valid UTF-8.
func scanValidUTF8Lines(data []byte, atEOF bool) (int, []byte, error) {
	adv, tok, err := bufio.ScanLines(data, atEOF)
	if tok != nil {
		tok = validUTF8(tok)
	}
	return adv, tok, err
}

// agentDecodeOpts decode JSON produced by agent CLIs and their protocols.
var agentDecodeOpts = json.JoinOptions(jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))

// unmarshal decodes agent-produced JSON leniently (see agentDecodeOpts).
func unmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v, agentDecodeOpts)
}

// lineOpts encode messages for the stdio protocols: invalid UTF-8 in strings is
// replaced instead of failing, and map keys are sorted (as v1 did) so traces and
// logs are stable.
var lineOpts = json.JoinOptions(jsontext.AllowInvalidUTF8(true), json.Deterministic(true))

// marshalLine encodes v for a line-delimited stdio protocol.
func marshalLine(v any) ([]byte, error) {
	return json.Marshal(v, lineOpts)
}
