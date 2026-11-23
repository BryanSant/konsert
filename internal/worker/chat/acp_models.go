package chat

// Model and effort discovery from ACP session/new results. Agents encode effort three ways:
//
//   - grok: one model per id, efforts in _meta.reasoningEfforts; effort is switched with the
//     "reasoning_effort" config option (category thought_level).
//   - agy: effort baked into the id suffix (gemini-4-neon-high, claude-opus-5-5-low); the
//     variants are grouped into one model and effort is switched by set_model on the variant.
//   - cursor: one id per model with bracket parameters (claude-opus-5-5[context=300k,
//     effort=medium,fast=false]). Cursor accepts only the listed ids, so the effort shown is
//     the fixed default of that entry and cannot be changed over ACP.

import (
	"regexp"
	"sort"
	"strings"

	"github.com/BryanSant/konsert/internal/protocol"
)

// effortOrder is the canonical low→high ordering of effort levels.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func effortRank(e string) int {
	for i, x := range effortOrder {
		if x == e {
			return i
		}
	}
	return len(effortOrder)
}

// normalizeEffort maps spellings to canonical effort ids ("extra-high" → "xhigh").
func normalizeEffort(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	switch e {
	case "extra-high", "extra_high", "extrahigh", "extra high", "x-high", "extra":
		return "xhigh"
	case "med":
		return "medium"
	case "maximum":
		return "max"
	}
	return e
}

func effortLabel(e string) (label, short string) {
	switch normalizeEffort(e) {
	case "none":
		return "None", "None"
	case "minimal":
		return "Minimal", "Min"
	case "low":
		return "Low", "Low"
	case "medium":
		return "Medium", "Med"
	case "high":
		return "High", "High"
	case "xhigh":
		return "Extra High", "XHigh"
	case "max":
		return "Max", "Max"
	}
	if e == "" {
		return "", ""
	}
	return strings.ToUpper(e[:1]) + e[1:], ""
}

func mkEffort(id, label string) protocol.AgentEffort {
	l, short := effortLabel(id)
	if label == "" {
		label = l
	}
	return protocol.AgentEffort{ID: id, Label: label, Short: short}
}

var effortSuffixRe = regexp.MustCompile(`^(.+?)-(none|minimal|low|medium|high|xhigh|extra-high|max)$`)

// splitEffortSuffix splits "gemini-4-neon-high" into ("gemini-4-neon", "high").
func splitEffortSuffix(id string) (base, effort string) {
	m := effortSuffixRe.FindStringSubmatch(id)
	if m == nil {
		return id, ""
	}
	return m[1], normalizeEffort(m[2])
}

// parseBracketModel splits "claude-opus-5-5[context=300k,effort=high]" into the base id
// and its parameters.
func parseBracketModel(id string) (base string, params map[string]string, ok bool) {
	i := strings.IndexByte(id, '[')
	if i <= 0 || !strings.HasSuffix(id, "]") {
		return id, nil, false
	}
	params = map[string]string{}
	for _, kv := range strings.Split(id[i+1:len(id)-1], ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		if k != "" {
			params[k] = v
		}
	}
	return id[:i], params, true
}

// bracketEffort returns the effort parameter of a bracket model id, if any.
func bracketEffort(params map[string]string) string {
	for _, k := range []string{"effort", "reasoning_effort", "reasoning"} {
		if v := params[k]; v != "" {
			return normalizeEffort(v)
		}
	}
	return ""
}

var parenSuffixRe = regexp.MustCompile(`\s*\(([^)]*)\)\s*$`)

// stripEffortFromName removes a trailing effort word from a display name:
// "Gemini 4 Neon (High)" → "Gemini 4 Neon", "Gemini 4 Neon  Extra High" → "Gemini 4 Neon".
func stripEffortFromName(name, effort string) string {
	name = strings.Join(strings.Fields(name), " ")
	if effort == "" {
		return name
	}
	if m := parenSuffixRe.FindStringSubmatch(name); m != nil && normalizeEffort(m[1]) == effort {
		return strings.TrimSpace(name[:len(name)-len(m[0])])
	}
	label, _ := effortLabel(effort)
	for _, suf := range []string{label, effort} {
		if suf != "" && strings.HasSuffix(strings.ToLower(name), " "+strings.ToLower(suf)) {
			return strings.TrimSpace(name[:len(name)-len(suf)])
		}
	}
	return name
}

// acpModelEntry is one user-facing model and how to select it over ACP.
type acpModelEntry struct {
	Model protocol.AgentModel
	// Variants maps effort → ACP modelId (effort "" = the id when no effort applies).
	Variants map[string]string
	// EffortConfig is the config option id that switches effort (grok "reasoning_effort").
	EffortConfig string
	// FixedEffort means the effort cannot be changed (cursor bracket ids).
	FixedEffort bool
}

// acpCatalog is the model catalog of one session.
type acpCatalog struct {
	Entries []acpModelEntry
	// Current is the user-facing model id and effort the session reports.
	CurrentModel  string
	CurrentEffort string
}

// buildCatalog maps an ACP session/new result to models with efforts.
func buildCatalog(res *acpSessionResult) acpCatalog {
	var cat acpCatalog
	if res == nil || res.Models == nil || len(res.Models.AvailableModels) == 0 {
		return cat
	}
	effortCfg, hasEffortCfg := findConfig(res.ConfigOptions, "reasoning_effort", "thought_level")
	cur := res.Models.CurrentModelID
	index := map[string]int{}
	add := func(e acpModelEntry) int {
		index[e.Model.ID] = len(cat.Entries)
		cat.Entries = append(cat.Entries, e)
		return len(cat.Entries) - 1
	}

	for _, m := range res.Models.AvailableModels {
		switch {
		case len(m.Meta.ReasoningEfforts) > 0:
			// grok: efforts in _meta.
			e := acpModelEntry{
				Model: protocol.AgentModel{
					ID: m.ModelID, DisplayName: m.Name, Description: m.Description,
					ContextTokens: m.Meta.TotalContextTokens,
				},
				Variants: map[string]string{"": m.ModelID},
			}
			for _, re := range m.Meta.ReasoningEfforts {
				id := re.Value
				if id == "" {
					id = re.ID
				}
				e.Model.Efforts = append(e.Model.Efforts, mkEffort(id, re.Label))
				if re.Default {
					e.Model.DefaultEffort = id
				}
			}
			sortEfforts(e.Model.Efforts)
			if e.Model.DefaultEffort == "" {
				e.Model.DefaultEffort = m.Meta.ReasoningEffort
			}
			if hasEffortCfg {
				e.EffortConfig = effortCfg.ID
			}
			if m.ModelID == cur {
				cat.CurrentModel = m.ModelID
				cat.CurrentEffort = e.Model.DefaultEffort
				if hasEffortCfg && effortCfg.current() != "" {
					cat.CurrentEffort = effortCfg.current()
				}
			}
			add(e)

		case strings.Contains(m.ModelID, "["):
			// cursor: bracket parameters; effort fixed per listed id.
			base, params, _ := parseBracketModel(m.ModelID)
			eff := bracketEffort(params)
			name := stripEffortFromName(m.Name, eff)
			if base == "default" && name == "" {
				name = "Auto"
			}
			e := acpModelEntry{
				Model:       protocol.AgentModel{ID: base, DisplayName: name, Description: m.Description},
				Variants:    map[string]string{"": m.ModelID},
				FixedEffort: true,
			}
			if eff != "" {
				e.Model.Efforts = []protocol.AgentEffort{mkEffort(eff, "")}
				e.Model.DefaultEffort = eff
				e.Variants[eff] = m.ModelID
			}
			if ctx := params["context"]; ctx != "" {
				e.Model.ContextTokens = parseTokenCount(ctx)
			}
			if _, dup := index[base]; dup {
				// Two entries with the same base: keep both, distinguished by the full id.
				e.Model.ID = m.ModelID
			}
			if m.ModelID == cur {
				cat.CurrentModel = e.Model.ID
				cat.CurrentEffort = eff
			}
			add(e)

		default:
			// agy: effort suffix variants, grouped by base id.
			base, eff := splitEffortSuffix(m.ModelID)
			name := stripEffortFromName(m.Name, eff)
			i, ok := index[base]
			if !ok {
				i = add(acpModelEntry{
					Model:    protocol.AgentModel{ID: base, DisplayName: name, Description: m.Description},
					Variants: map[string]string{},
				})
			}
			e := &cat.Entries[i]
			e.Variants[eff] = m.ModelID
			if eff != "" {
				e.Model.Efforts = append(e.Model.Efforts, mkEffort(eff, ""))
				sortEfforts(e.Model.Efforts)
			}
			if m.ModelID == cur {
				cat.CurrentModel = base
				cat.CurrentEffort = eff
				e.Model.DefaultEffort = eff
			}
		}
	}
	// Grouped models: default effort = the session's current one, else "high" when offered,
	// else the middle level.
	for i := range cat.Entries {
		e := &cat.Entries[i]
		if e.Model.DefaultEffort == "" && len(e.Model.Efforts) > 0 {
			e.Model.DefaultEffort = e.Model.Efforts[len(e.Model.Efforts)/2].ID
			for _, ef := range e.Model.Efforts {
				if ef.ID == "high" {
					e.Model.DefaultEffort = "high"
				}
			}
		}
		if e.Model.ID == cat.CurrentModel {
			e.Model.IsDefault = true
		}
	}
	return cat
}

func sortEfforts(es []protocol.AgentEffort) {
	sort.SliceStable(es, func(i, j int) bool { return effortRank(es[i].ID) < effortRank(es[j].ID) })
}

func parseTokenCount(s string) int64 {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1000, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1000000, strings.TrimSuffix(s, "m")
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n * mult
}

// Models returns the user-facing model list.
func (c acpCatalog) Models() []protocol.AgentModel {
	out := make([]protocol.AgentModel, 0, len(c.Entries))
	for _, e := range c.Entries {
		out = append(out, e.Model)
	}
	return out
}

// find returns the entry for a user-facing id or a raw ACP model id.
func (c acpCatalog) find(model string) (acpModelEntry, string, bool) {
	for _, e := range c.Entries {
		if e.Model.ID == model {
			return e, "", true
		}
	}
	for _, e := range c.Entries {
		for eff, id := range e.Variants {
			if id == model {
				return e, eff, true
			}
		}
	}
	return acpModelEntry{}, "", false
}

// acpSelection says how to apply a model/effort choice.
type acpSelection struct {
	ModelID      string // ACP modelId for session/set_model ("" = unchanged)
	Model        string // user-facing model id
	Effort       string // effective effort
	EffortConfig string // config option to set Effort through ("" = none)
	Note         string // why a requested effort could not be applied
}

// resolve maps (model, effort) to the ACP selection. Empty model means the current one.
func (c acpCatalog) resolve(model, effort string) (acpSelection, bool) {
	if model == "" {
		model = c.CurrentModel
	}
	effort = normalizeEffort(effort)
	e, rawEff, ok := c.find(model)
	if !ok {
		return acpSelection{}, false
	}
	sel := acpSelection{Model: e.Model.ID}
	if effort == "" {
		effort = rawEff
	}
	if effort == "" && e.Model.ID == c.CurrentModel {
		effort = c.CurrentEffort
	}
	if effort == "" {
		effort = e.Model.DefaultEffort
	}
	switch {
	case e.EffortConfig != "":
		sel.ModelID = e.Variants[""]
		if effort != "" && hasEffort(e.Model.Efforts, effort) {
			sel.Effort, sel.EffortConfig = effort, e.EffortConfig
		} else if effort != "" {
			sel.Note = "effort " + effort + " is not offered by " + e.Model.ID
		}
	case e.FixedEffort:
		sel.ModelID = e.Variants[""]
		sel.Effort = e.Model.DefaultEffort
		if effort != "" && effort != sel.Effort {
			sel.Note = e.Model.ID + " runs at its fixed effort over ACP; pick a different model entry to change it"
		}
	default:
		if id, ok := e.Variants[effort]; ok && effort != "" {
			sel.ModelID, sel.Effort = id, effort
		} else if id, ok := e.Variants[""]; ok {
			sel.ModelID = id
		} else if id, ok := e.Variants[e.Model.DefaultEffort]; ok {
			sel.ModelID, sel.Effort = id, e.Model.DefaultEffort
			if effort != "" {
				sel.Note = "effort " + effort + " is not offered by " + e.Model.ID
			}
		}
	}
	return sel, true
}

func hasEffort(es []protocol.AgentEffort, id string) bool {
	for _, e := range es {
		if e.ID == id {
			return true
		}
	}
	return false
}
