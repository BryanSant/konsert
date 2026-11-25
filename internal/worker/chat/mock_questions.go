package chat

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/BryanSant/syre/internal/protocol"
)

// The mock engine's question features (PRD §5.8b), for hub and e2e tests without paid
// calls: it calls Syre's ask_user over MCP like a real agent ("question", "vague
// question"), forks sessions (side conversations remember the original's prompts),
// answers side-conversation prompts with a draft answer and a summary, and decides
// delegated questions with answer_question.

// mockQuestionArgs is the well-formed question the mock asks on "question".
func mockQuestionArgs() map[string]any {
	return map[string]any{
		"question": "Accept the shortened exit criteria on SYR-2Q0CH?",
		"context":  "SYR-2Q0CH 'Questions and steering' requires a live phone run before it can close. The shortened criteria drop the phone run and keep the automated mobile e2e.",
		"why_now":  "Closing the epic waits on this: with Yes I close it today; with No I schedule the phone run first.",
		"options": []map[string]any{
			{"label": "Yes", "consequence": "The epic closes today on the automated mobile e2e alone.", "recommended": true},
			{"label": "No, keep the strict exit criteria", "consequence": "The epic stays open until someone does the phone run."},
		},
		"rationale":  "The mobile e2e covers the same flow at 390px and has been green for a week.",
		"allow_text": true,
	}
}

// mockMCP returns an MCP client session to the chat's "syre" server (lazily).
func (s *mockSession) mockMCP(ctx context.Context) (*mcp.ClientSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mcpSess != nil {
		return s.mcpSess, nil
	}
	var srv *protocol.AgentMCPServer
	for i := range s.opts.MCPServers {
		if s.opts.MCPServers[i].Name == "syre" {
			srv = &s.opts.MCPServers[i]
		}
	}
	if srv == nil {
		return nil, fmt.Errorf("no syre MCP server in this chat")
	}
	hc := &http.Client{Transport: mockHeaderTransport{headers: srv.Headers}}
	client := mcp.NewClient(&mcp.Implementation{Name: "syre-mock", Version: "1"}, nil)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sess, err := client.Connect(cctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc}, nil)
	if err != nil {
		return nil, err
	}
	s.mcpSess = sess
	return sess, nil
}

type mockHeaderTransport struct{ headers map[string]string }

func (t mockHeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// callSyre calls a syre MCP tool, showing it as a tool call like a real agent.
func (s *mockSession) callSyre(ctx context.Context, turn, tool string, args map[string]any) (string, bool) {
	s.mu.Lock()
	s.toolN++
	id := fmt.Sprintf("mcp-%s-%d", turn, s.toolN)
	s.mu.Unlock()
	tc := &protocol.AgentToolCall{ID: id, Name: "mcp__syre__" + tool, Kind: "other", Title: "syre: " + tool, Status: "running"}
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: turn, ToolCall: tc})
	text, isErr := "", false
	sess, err := s.mockMCP(ctx)
	if err == nil {
		var res *mcp.CallToolResult
		res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err == nil {
			isErr = res.IsError
			for _, c := range res.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					text += t.Text
				}
			}
		}
	}
	if err != nil {
		text, isErr = err.Error(), true
	}
	done := *tc
	done.Status, done.Output = "completed", text
	if isErr {
		done.Status = "failed"
	}
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventToolCall, TurnID: turn, ToolCall: &done})
	return text, !isErr
}

var (
	mockOptionRe   = regexp.MustCompile(`(?m)^Option (\d+): (.+?)( \(recommended\))?(?: — .*)?$`)
	mockQuestionRe = regexp.MustCompile(`question "(q-[0-9a-z]+)"`)
	mockAnsweredRe = regexp.MustCompile(`answered the original question with (.+?)\. Write the note`)
)

// mockRecommended returns the recommended (else first) option label in a question text.
func mockRecommended(text string) string {
	first := ""
	for _, m := range mockOptionRe.FindAllStringSubmatch(text, -1) {
		if first == "" {
			first = m[2]
		}
		if m[3] != "" {
			return m[2]
		}
	}
	return first
}

// runQuestionTurn handles the question-related prompts. It reports false when the
// prompt is not one of them.
func (s *mockSession) runQuestionTurn(ctx context.Context, p protocol.AgentChatPrompt, lower string) bool {
	turn := p.TurnID
	reply := ""
	switch {
	case strings.Contains(p.Text, "[Syre] The user has now answered the original question"):
		ans := "their answer"
		if m := mockAnsweredRe.FindStringSubmatch(p.Text); m != nil {
			ans = m[1]
		}
		reply = fmt.Sprintf("The user asked what SYR-2Q0CH is and why the criteria change; after learning the mobile e2e covers the phone flow, chose %s.", ans)

	case strings.Contains(p.Text, "The user delegated a question to you"):
		qid := ""
		if m := mockQuestionRe.FindStringSubmatch(p.Text); m != nil {
			qid = m[1]
		}
		out, _ := s.callSyre(ctx, turn, "answer_question", map[string]any{
			"question": qid, "option": mockRecommended(p.Text),
			"rationale": "Mock commander: it is the recommended option and nothing in the fleet argues against it.",
		})
		reply = "Decided " + qid + ": " + out

	case strings.Contains(p.Text, "[Syre side conversation]") || (s.forkOf != "" && strings.Contains(p.Text, "The user asks:")):
		asked := p.Text
		if i := strings.LastIndex(asked, "The user asks:"); i >= 0 {
			asked = strings.TrimSpace(strings.SplitN(asked[i+len("The user asks:"):], "\n", 2)[0])
		}
		who := "the commander, answering from a context pack"
		if s.forkOf != "" {
			who = "a read-only fork of " + s.forkOf
			s.eng.mu.Lock()
			if prompts := s.eng.memory[s.forkOf]; len(prompts) > 0 {
				who += fmt.Sprintf(" (the original was asked: %q)", prompts[len(prompts)-1])
			}
			s.eng.mu.Unlock()
		}
		rec := mockRecommended(p.Text)
		s.mu.Lock()
		if rec != "" {
			s.lastRecommended = rec
		}
		rec = s.lastRecommended
		s.mu.Unlock()
		reply = fmt.Sprintf("I'm %s. You asked: %q. SYR-2Q0CH is the 'Questions and steering' epic; the shortened criteria drop the manual phone run because the mobile e2e covers it.", who, asked)
		if rec != "" {
			reply += "\nDRAFT ANSWER: " + rec
		}

	case strings.Contains(lower, "vague question"):
		bad, ok := s.callSyre(ctx, turn, "ask_user", map[string]any{"question": "Accept the shortened exit criteria on SYR-2Q0CH?", "options": []string{"Yes (recommended)", "No"}})
		if ok {
			reply = "Unexpected: the vague question was accepted: " + bad
			break
		}
		first := strings.SplitN(bad, "\n", 2)[0]
		out, _ := s.callSyre(ctx, turn, "ask_user", mockQuestionArgs())
		reply = "Rejected first (" + first + "). Then: " + out

	case strings.Contains(lower, "question"):
		out, _ := s.callSyre(ctx, turn, "ask_user", mockQuestionArgs())
		reply = "Ask result: " + out

	default:
		return false
	}
	if ctx.Err() != nil {
		s.cancelled(turn)
		return true
	}
	if !s.deltas(ctx, turn, "m-"+turn, reply) {
		s.cancelled(turn)
		return true
	}
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: turn, StopReason: "end_turn"})
	return true
}
