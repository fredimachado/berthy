package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// An approval is a change that an AI agent's token asked for on a protected environment. It is held, with the plan a dry run of
// the same request returned, until an administrator approves or rejects it, or it expires. Status is pending, approved,
// rejected, expired or failed.
type Approval struct {
	ID                         string
	ActorID, ActorName, Remote string
	Method, Path, Summary      string
	Plan                       json.RawMessage
	Created, Expires           time.Time
	Status, DecidedBy          string
	DecidedAt                  time.Time // zero while the approval is pending
	Result                     string
}

var (
	// ErrApprovalNotPending means the approval was already decided or has expired.
	ErrApprovalNotPending = errors.New("approval is not pending")
	// ErrApprovalExpired means the approval ran past its expiry before it was decided. It is marked expired.
	ErrApprovalExpired = errors.New("approval expired")
	// ErrTooManyApprovals means the environment already holds the most pending approvals it accepts.
	ErrTooManyApprovals = errors.New("too many pending approvals")
)

const (
	// MaxPendingApprovals bounds the requests an environment holds at once, so a token cannot fill the database.
	MaxPendingApprovals = 100
	// MaxDecidedApprovals is how many decided approvals an environment keeps, newest first.
	MaxDecidedApprovals = 500
	maxApprovalResult   = 300
)

// systemActor is who the audit log names when an approval expires without anyone acting on it.
var systemActor = Actor{ID: "berthy", Name: "berthy"}

const approvalColumns = `id,actor_id,actor_name,remote,method,path,summary,plan,created,expires,status,decided_by,decided_at,result`

func scanApproval(row interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var created, expires, decided int64
	if err := row.Scan(&a.ID, &a.ActorID, &a.ActorName, &a.Remote, &a.Method, &a.Path, &a.Summary, &a.Plan, &created, &expires, &a.Status, &a.DecidedBy, &decided, &a.Result); err != nil {
		return Approval{}, err
	}
	a.Created, a.Expires, a.DecidedAt = fromMilli(created), fromMilli(expires), fromMilli(decided)
	return a, nil
}

// CreateApproval holds a request as a pending approval. a carries the actor, the route, the summary, the plan and the times;
// request is the request body, which can hold a secret value and is stored sealed. It audits the request as the agent. It
// returns ErrTooManyApprovals when the environment already holds MaxPendingApprovals pending approvals.
func (s *Store) CreateApproval(ctx context.Context, a Approval, request []byte) (Approval, error) {
	if a.ActorID == "" || a.Method == "" || a.Path == "" || a.Summary == "" || !validJSON(a.Plan) || a.Created.IsZero() || !a.Expires.After(a.Created) {
		return Approval{}, errors.New("invalid approval")
	}
	id, err := randomID()
	if err != nil {
		return Approval{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Approval{}, err
	}
	defer tx.Rollback()
	// The request is sealed inside the transaction: a key rotation commits and swaps keys while it holds the connection, so
	// what is sealed here always uses the key the committed state names.
	sealed, err := s.seal(approvalRecord(id), request)
	if err != nil {
		return Approval{}, err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM approvals WHERE environment=? AND status='pending' AND expires>?`, s.env, toMilli(a.Created)).Scan(&pending); err != nil {
		return Approval{}, err
	}
	if pending >= MaxPendingApprovals {
		return Approval{}, ErrTooManyApprovals
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO approvals(id,environment,actor_id,actor_name,remote,method,path,request_sealed,summary,plan,created,expires,status,decided_by,decided_at,result) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'pending','',0,'')`,
		id, s.env, a.ActorID, a.ActorName, a.Remote, a.Method, a.Path, sealed, a.Summary, []byte(a.Plan), toMilli(a.Created), toMilli(a.Expires)); err != nil {
		return Approval{}, err
	}
	if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: a.ActorID, ActorName: a.ActorName, Remote: a.Remote, Kind: "approval_request:" + a.Summary}, "held "+id); err != nil {
		return Approval{}, err
	}
	// Only decided approvals are dropped: a pending one is still waiting for a person, and an expired one is decided by the sweep.
	if _, err = tx.ExecContext(ctx, `DELETE FROM approvals WHERE environment=? AND status<>'pending' AND id NOT IN (SELECT id FROM approvals WHERE environment=? AND status<>'pending' ORDER BY created DESC, rowid DESC LIMIT ?)`, s.env, s.env, MaxDecidedApprovals); err != nil {
		return Approval{}, err
	}
	if err = tx.Commit(); err != nil {
		return Approval{}, err
	}
	a.ID, a.Status = id, "pending"
	return a, nil
}

// expireDue marks every pending approval whose expiry has passed at now as expired and audits each as actor.
func (s *Store) expireDue(ctx context.Context, tx *sql.Tx, actor Actor, now time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM approvals WHERE environment=? AND status='pending' AND expires<=?`, s.env, toMilli(now))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE approvals SET status='expired',decided_by=?,decided_at=? WHERE id=? AND status='pending'`, actor.Name, toMilli(now), id); err != nil {
			return nil, err
		}
		if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "approval_expired"}, "expired "+id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// ExpireApprovals marks the pending approvals that have run past their expiry at now as expired, audited as actor, and returns
// how many.
func (s *Store) ExpireApprovals(ctx context.Context, actor Actor, now time.Time) (int, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ids, err := s.expireDue(ctx, tx, actor, now)
	if err != nil {
		return 0, err
	}
	return len(ids), tx.Commit()
}

// Approvals returns the environment's approvals, newest first, after marking those past their expiry at now as expired. status
// limits the list to one status, and "" lists them all.
func (s *Store) Approvals(ctx context.Context, status string, now time.Time) ([]Approval, error) {
	if _, err := s.ExpireApprovals(ctx, systemActor, now); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE environment=? AND (?='' OR status=?) ORDER BY created DESC, rowid DESC`, s.env, status, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Approval returns one approval as it is stored. It returns sql.ErrNoRows when there is none. Expiry is not applied here: a
// pending approval past its expiry is expired by ExpireApprovals or by the decision that finds it.
func (s *Store) Approval(ctx context.Context, id string) (Approval, error) {
	return scanApproval(s.queryRow(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE environment=? AND id=?`, s.env, id))
}

// ApprovalRequest returns the request body held with an approval.
func (s *Store) ApprovalRequest(ctx context.Context, id string) ([]byte, error) {
	var sealed []byte
	if err := s.queryRow(ctx, `SELECT request_sealed FROM approvals WHERE environment=? AND id=?`, s.env, id).Scan(&sealed); err != nil {
		return nil, err
	}
	return s.open(approvalRecord(id), sealed)
}

// DecideApproval moves a pending approval to approved, rejected or failed on behalf of decider, and audits it. result is the
// reason, when there is one. An approval past its expiry at now is marked expired instead and the call returns
// ErrApprovalExpired; one that is not pending returns ErrApprovalNotPending, and an unknown ID sql.ErrNoRows. Exactly one of
// two concurrent decisions succeeds.
func (s *Store) DecideApproval(ctx context.Context, id string, decider Actor, decision, result string, now time.Time) (Approval, error) {
	var action string
	switch decision {
	case "approved":
		action = "approval_approve"
	case "rejected":
		action = "approval_reject"
	case "failed":
		action = "approval_failed"
	default:
		return Approval{}, errors.New("invalid approval decision")
	}
	if decider.ID == "" {
		return Approval{}, errors.New("actor required")
	}
	result = boundResult(result)
	tx, err := s.begin(ctx)
	if err != nil {
		return Approval{}, err
	}
	defer tx.Rollback()
	expired, err := s.expireDue(ctx, tx, decider, now)
	if err != nil {
		return Approval{}, err
	}
	a, err := scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE environment=? AND id=?`, s.env, id))
	if err != nil {
		return Approval{}, err
	}
	if a.Status != "pending" {
		// The expiry that this call just recorded is kept, and reported as such.
		if slices.Contains(expired, id) {
			if err = tx.Commit(); err != nil {
				return Approval{}, err
			}
			return a, ErrApprovalExpired
		}
		return a, ErrApprovalNotPending
	}
	if _, err = tx.ExecContext(ctx, `UPDATE approvals SET status=?,decided_by=?,decided_at=?,result=? WHERE id=? AND status='pending'`, decision, decider.Name, toMilli(now), result, id); err != nil {
		return Approval{}, err
	}
	if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: decider.ID, ActorName: decider.Name, Remote: decider.Remote, Kind: action}, decision+" "+id); err != nil {
		return Approval{}, err
	}
	if err = tx.Commit(); err != nil {
		return Approval{}, err
	}
	a.Status, a.DecidedBy, a.DecidedAt, a.Result = decision, decider.Name, now, result
	return a, nil
}

// FinishApproval records how an approved request ended: status stays approved with the result, or becomes failed. It returns
// ErrApprovalNotPending unless the approval is approved and has no result yet, so the outcome is recorded once.
func (s *Store) FinishApproval(ctx context.Context, id, status, result string) error {
	if status != "approved" && status != "failed" {
		return errors.New("invalid approval status")
	}
	res, err := s.exec(ctx, `UPDATE approvals SET status=?,result=? WHERE environment=? AND id=? AND status='approved' AND result=''`, status, boundResult(result), s.env, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrApprovalNotPending
	}
	return nil
}

// boundResult keeps a result to one short line.
func boundResult(s string) string {
	if len(s) > maxApprovalResult {
		s = s[:maxApprovalResult]
	}
	return s
}
