package sqlite

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/store"
)

// Question storage (PRD §5.8b). Options, references, the answer, the delegation receipt
// and the side-conversation state are JSON columns; the side-conversation messages
// live in question_messages.

const questionCols = `id, sid, chat_id, task_id, worker_id, agent, account_id, kind, source,
	elicitation_id, prompt, context, why_now, options_json, allow_text, recommendation, rationale,
	category, blocks_json, group_key, refs_json, status, blocked_since, snoozed_until, abandoned,
	ask_count, rejections, answer_json, delegation_json, thread_json, delivered, created_at,
	updated_at, closed_at`

// jsonCol encodes v as a JSON column ("" for nil/empty).
func jsonCol(v any, empty bool) string {
	if empty {
		return ""
	}
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return ""
	}
	return string(b)
}

// fromJSONCol decodes a JSON column into v (left zero when empty or invalid).
func fromJSONCol(s string, v any) {
	if strings.TrimSpace(s) == "" {
		return
	}
	_ = json.Unmarshal([]byte(s), v)
}

func questionDelivered(q store.Question) bool {
	return !(q.Status == store.QuestionAnswered && q.Answer != nil && q.Answer.DeliveredAt == nil)
}

func scanQuestion(sc interface{ Scan(...any) error }) (store.Question, error) {
	var (
		q                                      store.Question
		opts, blocks, refs, ans, deleg, thread string
		blocked, cAt, uAt                      string
		snoozed, closed                        sql.NullString
		allowText, abandoned, delivered        int
	)
	if err := sc.Scan(&q.ID, &q.SID, &q.ChatID, &q.TaskID, &q.WorkerID, &q.Agent, &q.AccountID, &q.Kind, &q.Source,
		&q.ElicitationID, &q.Prompt, &q.Context, &q.WhyNow, &opts, &allowText, &q.Recommendation, &q.Rationale,
		&q.Category, &blocks, &q.GroupKey, &refs, &q.Status, &blocked, &snoozed, &abandoned,
		&q.AskCount, &q.Rejections, &ans, &deleg, &thread, &delivered, &cAt,
		&uAt, &closed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Question{}, store.ErrNotFound
		}
		return store.Question{}, err
	}
	fromJSONCol(opts, &q.Options)
	fromJSONCol(blocks, &q.Blocks)
	fromJSONCol(refs, &q.Refs)
	if ans != "" {
		q.Answer = &store.QuestionAnswer{}
		fromJSONCol(ans, q.Answer)
	}
	if deleg != "" {
		q.Delegation = &store.QuestionDelegation{}
		fromJSONCol(deleg, q.Delegation)
	}
	if thread != "" {
		q.Thread = &store.QuestionThread{}
		fromJSONCol(thread, q.Thread)
	}
	q.AllowText = allowText != 0
	q.Abandoned = abandoned != 0
	q.BlockedSince = parseTime(blocked)
	q.SnoozedUntil = scanNullTime(snoozed)
	q.CreatedAt = parseTime(cAt)
	q.UpdatedAt = parseTime(uAt)
	q.ClosedAt = scanNullTime(closed)
	return q, nil
}

func questionArgs(q store.Question) []any {
	return []any{
		q.ChatID, q.TaskID, q.WorkerID, q.Agent, q.AccountID, q.Kind, q.Source,
		q.ElicitationID, q.Prompt, q.Context, q.WhyNow, jsonCol(q.Options, len(q.Options) == 0), boolInt(q.AllowText),
		q.Recommendation, q.Rationale, q.Category, jsonCol(q.Blocks, len(q.Blocks) == 0), q.GroupKey,
		jsonCol(q.Refs, len(q.Refs) == 0), q.Status, formatTime(q.BlockedSince), nullTime(q.SnoozedUntil),
		boolInt(q.Abandoned), q.AskCount, q.Rejections, jsonCol(q.Answer, q.Answer == nil),
		jsonCol(q.Delegation, q.Delegation == nil), jsonCol(q.Thread, q.Thread == nil), boolInt(questionDelivered(q)),
	}
}

// CreateQuestion inserts a question (id and sid minted when empty).
func (s *SQLiteStore) CreateQuestion(ctx context.Context, q store.Question) (store.Question, error) {
	if err := s.assignIDs(ctx, ids.Question, &q.ID, &q.SID); err != nil {
		return q, err
	}
	now := time.Now().UTC()
	if q.CreatedAt.IsZero() {
		q.CreatedAt = now
	}
	q.UpdatedAt = q.CreatedAt
	if q.BlockedSince.IsZero() {
		q.BlockedSince = q.CreatedAt
	}
	if q.Status == "" {
		q.Status = store.QuestionOpen
	}
	if q.AskCount == 0 {
		q.AskCount = 1
	}
	args := append([]any{q.ID, q.SID}, questionArgs(q)...)
	args = append(args, formatTime(q.CreatedAt), formatTime(q.UpdatedAt), nullTime(q.ClosedAt))
	_, err := s.db.ExecContext(ctx, `INSERT INTO questions (`+questionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
	return q, err
}

// GetQuestion returns a question by id.
func (s *SQLiteStore) GetQuestion(ctx context.Context, id string) (store.Question, error) {
	return scanQuestion(s.db.QueryRowContext(ctx, `SELECT `+questionCols+` FROM questions WHERE id = ?`, id))
}

// UpdateQuestion writes every mutable column of an existing question.
func (s *SQLiteStore) UpdateQuestion(ctx context.Context, q store.Question) error {
	args := append(questionArgs(q), formatTime(time.Now().UTC()), nullTime(q.ClosedAt), q.ID)
	res, err := s.db.ExecContext(ctx, `
		UPDATE questions SET chat_id = ?, task_id = ?, worker_id = ?, agent = ?, account_id = ?, kind = ?,
			source = ?, elicitation_id = ?, prompt = ?, context = ?, why_now = ?, options_json = ?,
			allow_text = ?, recommendation = ?, rationale = ?, category = ?, blocks_json = ?, group_key = ?,
			refs_json = ?, status = ?, blocked_since = ?, snoozed_until = ?, abandoned = ?, ask_count = ?,
			rejections = ?, answer_json = ?, delegation_json = ?, thread_json = ?, delivered = ?,
			updated_at = ?, closed_at = ?
		WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListQuestions returns questions matching f, newest first.
func (s *SQLiteStore) ListQuestions(ctx context.Context, f store.QuestionFilter) ([]store.Question, error) {
	var where []string
	var args []any
	var statuses []string
	for _, st := range f.Statuses {
		if st != "" {
			statuses = append(statuses, st)
		}
	}
	if len(statuses) > 0 {
		where = append(where, "status IN (?"+strings.Repeat(", ?", len(statuses)-1)+")")
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	if f.ChatID != "" {
		where = append(where, "chat_id = ?")
		args = append(args, f.ChatID)
	}
	if f.UndeliveredOnly {
		where = append(where, "delivered = 0")
	}
	if !f.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, formatTime(f.Since))
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	q := `SELECT ` + questionCols + ` FROM questions`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Question
	for rows.Next() {
		qq, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, qq)
	}
	return out, rows.Err()
}

// AppendQuestionMessage adds a side-conversation message with the next seq.
func (s *SQLiteStore) AppendQuestionMessage(ctx context.Context, m store.QuestionMessage) (store.QuestionMessage, error) {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO question_messages (question_id, seq, role, text, turn_id, created_at)
		VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM question_messages WHERE question_id = ?), ?, ?, ?, ?)
		RETURNING seq`, m.QuestionID, m.QuestionID, m.Role, m.Text, m.TurnID, formatTime(m.CreatedAt)).Scan(&m.Seq)
	return m, err
}

// ListQuestionMessages returns a question's side conversation in order.
func (s *SQLiteStore) ListQuestionMessages(ctx context.Context, questionID string) ([]store.QuestionMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT question_id, seq, role, text, turn_id, created_at
		FROM question_messages WHERE question_id = ? ORDER BY seq ASC`, questionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.QuestionMessage
	for rows.Next() {
		var m store.QuestionMessage
		var at string
		if err := rows.Scan(&m.QuestionID, &m.Seq, &m.Role, &m.Text, &m.TurnID, &at); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(at)
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddQuestionReject records an under-specified ask_user call.
func (s *SQLiteStore) AddQuestionReject(ctx context.Context, r store.QuestionReject) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO question_rejects (chat_id, agent, missing, created_at) VALUES (?, ?, ?, ?)`,
		r.ChatID, r.Agent, r.Missing, formatTime(r.CreatedAt))
	return err
}

// ListQuestionRejects returns rejects since a time, newest first.
func (s *SQLiteStore) ListQuestionRejects(ctx context.Context, since time.Time) ([]store.QuestionReject, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, chat_id, agent, missing, created_at FROM question_rejects
		WHERE created_at >= ? ORDER BY id DESC LIMIT 5000`, formatTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.QuestionReject
	for rows.Next() {
		var r store.QuestionReject
		var at string
		if err := rows.Scan(&r.ID, &r.ChatID, &r.Agent, &r.Missing, &at); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}
