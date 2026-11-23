package drivers

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// agentDecodeOpts decode JSON written by agent CLIs (session logs, quota caches).
// encoding/json/v2 rejects invalid UTF-8 and duplicate object names by default;
// agent output is not guaranteed to be clean, and one bad record must not hide the
// rest, so both are tolerated (invalid bytes become U+FFFD, the last duplicate
// wins, as in v1). Member names are matched case-sensitively.
var agentDecodeOpts = json.JoinOptions(jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))

// unmarshal decodes agent-produced JSON leniently (see agentDecodeOpts).
func unmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v, agentDecodeOpts)
}
