package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// ClaimResult reports a granted claim; Stolen marks a takeover from a gone
// holder (PrevName is who lost it).
type ClaimResult struct {
	Granted  bool
	Stolen   bool
	PrevName string
}

// ErrClaimHeld is the conflict returned when a path is claimed by another
// live agent.
type ErrClaimHeld struct {
	Holder   string
	HolderID string
	Note     string
	Since    int64
}

func (e *ErrClaimHeld) Error() string {
	if e.Note == "" {
		return fmt.Sprintf("held by %s (%s)", e.Holder, e.HolderID)
	}
	return fmt.Sprintf("held by %s (%s): %s", e.Holder, e.HolderID, e.Note)
}

// claimRaceHook lets tests interleave a concurrent claimant between observing
// the row and writing; nil in production.
var claimRaceHook func()

// Claim records the caller as owner of a path (or free-form label) in the
// coordination ledger. Re-claiming your own path refreshes note and since; a
// path held by a live agent returns ErrClaimHeld; a gone holder's claim is
// stale and gets taken over. Lost races (insert or takeover) re-run bounded
// against the winner's row.
func (s *Store) Claim(scope, name, path, note string) (ClaimResult, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return ClaimResult{}, errors.New("claim: path required")
	}
	aid, _, err := s.resolveAgent(scope, name)
	if err != nil {
		return ClaimResult{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		now := s.Now().Unix()
		var holderID, heldNote string
		var since int64
		err = s.db.QueryRow(`SELECT agent_id, note, since FROM claims WHERE scope=? AND path=?`,
			scope, path).Scan(&holderID, &heldNote, &since)
		if claimRaceHook != nil {
			claimRaceHook()
		}
		if err == sql.ErrNoRows {
			_, err := s.db.Exec(`INSERT INTO claims (scope, path, agent_id, note, since) VALUES (?,?,?,?,?)`,
				scope, path, aid, note, now)
			if isUniqueViolation(err) {
				continue // lost the insert race: re-run against the winner's row
			}
			return ClaimResult{Granted: err == nil}, err
		}
		if err != nil {
			return ClaimResult{}, err
		}
		if holderID == aid {
			_, err := s.db.Exec(`UPDATE claims SET note=?, since=? WHERE scope=? AND path=?`, note, now, scope, path)
			return ClaimResult{Granted: err == nil}, err
		}
		holderName, live, err := s.holderLiveness(scope, holderID)
		if err != nil {
			return ClaimResult{}, err
		}
		if live {
			return ClaimResult{}, &ErrClaimHeld{Holder: holderName, HolderID: holderID, Note: heldNote, Since: since}
		}
		// Takeover only from the exact gone holder observed: zero rows updated
		// means a rival stole (or the holder released) first - re-run.
		res, err := s.db.Exec(`UPDATE claims SET agent_id=?, note=?, since=? WHERE scope=? AND path=? AND agent_id=?`,
			aid, note, now, scope, path, holderID)
		if err != nil {
			return ClaimResult{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		return ClaimResult{Granted: true, Stolen: true, PrevName: holderName}, nil
	}
	return ClaimResult{}, fmt.Errorf("claim %s: lost repeated races, try again", path)
}

// holderLiveness resolves a claim holder's name and whether it still counts
// as present (any freshStatus but gone). A purged row reports the raw id.
func (s *Store) holderLiveness(scope, holderID string) (name string, live bool, err error) {
	var status string
	var seen int64
	err = s.db.QueryRow(`SELECT name, status, last_seen FROM agents WHERE scope=? AND agent_id=?`,
		scope, holderID).Scan(&name, &status, &seen)
	if err == sql.ErrNoRows {
		return holderID, false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, s.freshStatus(status, seen) != "gone", nil
}

// Release frees a claim held by the caller. Releasing an unheld path is a
// no-op success; releasing someone else's claim is refused.
func (s *Store) Release(scope, name, path string) error {
	path = strings.TrimSpace(path)
	aid, _, err := s.resolveAgent(scope, name)
	if err != nil {
		return err
	}
	var holderID string
	err = s.db.QueryRow(`SELECT agent_id FROM claims WHERE scope=? AND path=?`, scope, path).Scan(&holderID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if holderID != aid {
		holderName, _, _ := s.holderLiveness(scope, holderID)
		return fmt.Errorf("release: %s is held by %s (%s), not you", path, holderName, holderID)
	}
	_, err = s.db.Exec(`DELETE FROM claims WHERE scope=? AND path=?`, scope, path)
	return err
}

// ListClaims returns the scope's ledger with holder names resolved live.
func (s *Store) ListClaims(scope string) ([]protocol.ClaimInfo, error) {
	rows, err := s.db.Query(`
		SELECT c.path, COALESCE(a.name, ''), c.agent_id, c.note, c.since
		FROM claims c
		LEFT JOIN agents a ON a.scope = c.scope AND a.agent_id = c.agent_id
		WHERE c.scope = ? ORDER BY c.since, c.path`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.ClaimInfo
	for rows.Next() {
		var c protocol.ClaimInfo
		if err := rows.Scan(&c.Path, &c.Holder, &c.HolderID, &c.Note, &c.Since); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
