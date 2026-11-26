package sqlite

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

const policyCols = `id, sid, chat_id, kind, target, until, params_json, reason, summary, status, actor_id, created_at, decided_at, decided_by, applied_id`

func scanPolicyProposal(sc interface{ Scan(...any) error }) (store.PolicyProposal, error) {
	var (
		p              store.PolicyProposal
		paramsJSON     string
		cAt            string
		until, decided sql.NullString
	)
	if err := sc.Scan(
		&p.ID, &p.SID, &p.ChatID, &p.Kind, &p.Target, &until, &paramsJSON,
		&p.Reason, &p.Summary, &p.Status, &p.ActorID, &cAt, &decided, &p.DecidedBy, &p.AppliedID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.PolicyProposal{}, store.ErrNotFound
		}
		return store.PolicyProposal{}, err
	}
	if paramsJSON != "" && paramsJSON != "{}" {
		if err := json.Unmarshal([]byte(paramsJSON), &p.Params); err != nil {
			return store.PolicyProposal{}, err
		}
	}
	p.CreatedAt = parseTime(cAt)
	p.Until = scanNullTime(until)
	p.DecidedAt = scanNullTime(decided)
	return p, nil
}

// CreatePolicyProposal inserts a proposal (id and sid minted when empty).
func (s *SQLiteStore) CreatePolicyProposal(ctx context.Context, p store.PolicyProposal) (store.PolicyProposal, error) {
	if err := s.assignIDs(ctx, ids.Policy, &p.ID, &p.SID); err != nil {
		return p, err
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.Status == "" {
		p.Status = store.PolicyProposalPending
	}
	b, err := json.Marshal(p.Params)
	if err != nil {
		return p, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO policy_proposals (`+policyCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.SID, p.ChatID, p.Kind, p.Target, nullTime(p.Until), string(b),
		p.Reason, p.Summary, p.Status, p.ActorID, formatTime(p.CreatedAt), nullTime(p.DecidedAt), p.DecidedBy, p.AppliedID)
	return p, err
}

// GetPolicyProposal returns a proposal by id.
func (s *SQLiteStore) GetPolicyProposal(ctx context.Context, id string) (store.PolicyProposal, error) {
	return scanPolicyProposal(s.db.QueryRowContext(ctx, `SELECT `+policyCols+` FROM policy_proposals WHERE id = ?`, id))
}

// ListPolicyProposals returns proposals matching f, newest first.
func (s *SQLiteStore) ListPolicyProposals(ctx context.Context, f store.PolicyProposalFilter) ([]store.PolicyProposal, error) {
	q := `SELECT ` + policyCols + ` FROM policy_proposals`
	var args []any
	if f.Status != "" {
		q += ` WHERE status = ?`
		args = append(args, f.Status)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.PolicyProposal
	for rows.Next() {
		p, err := scanPolicyProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DecidePolicyProposal approves or denies a pending proposal.
func (s *SQLiteStore) DecidePolicyProposal(ctx context.Context, id, status, by, appliedID string, at time.Time) (bool, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	status = strings.TrimSpace(status)
	if status != store.PolicyProposalApproved && status != store.PolicyProposalDenied {
		return false, errors.New("policy proposal decision must be approved or denied")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE policy_proposals
		SET status = ?, decided_by = ?, decided_at = ?, applied_id = ?
		WHERE id = ? AND status = ?
	`, status, by, formatTime(at), appliedID, id, store.PolicyProposalPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
