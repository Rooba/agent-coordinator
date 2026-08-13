package store

import (
	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// MessageHistory is the non-destructive message journal: newest-first
// deliveries where the caller is sender or recipient, with read_at showing
// who read what and when (0 = unread). peer narrows to exchanges with that
// agent; limit defaults to 20 and caps at 100. Purely read-only over the
// retained messages+deliveries rows.
func (s *Store) MessageHistory(scope, name, peer string, limit int) ([]protocol.HistoryInfo, error) {
	aid, _, err := s.resolveAgent(scope, name)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	cond, args := `(m.from_agent = ? OR d.agent_id = ?)`, []any{scope, aid, aid}
	if peer != "" {
		peerID, _, err := s.resolveAgent(scope, peer)
		if err != nil {
			return nil, err
		}
		cond = `((m.from_agent = ? AND d.agent_id = ?) OR (m.from_agent = ? AND d.agent_id = ?))`
		args = []any{scope, aid, peerID, peerID, aid}
	}
	rows, err := s.db.Query(`
		SELECT m.id, COALESCE(fa.name, m.from_agent), COALESCE(ta.name, d.agent_id),
		       m.body, m.created_at, COALESCE(d.read_at, 0), m.to_agent IS NULL
		FROM messages m
		JOIN deliveries d ON d.message_id = m.id
		LEFT JOIN agents fa ON fa.scope = m.scope AND fa.agent_id = m.from_agent
		LEFT JOIN agents ta ON ta.scope = m.scope AND ta.agent_id = d.agent_id
		WHERE m.scope = ? AND `+cond+`
		ORDER BY m.id DESC, d.agent_id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.HistoryInfo
	for rows.Next() {
		var h protocol.HistoryInfo
		var body string
		if err := rows.Scan(&h.MessageID, &h.From, &h.To, &body, &h.SentAt, &h.ReadAt, &h.Broadcast); err != nil {
			return nil, err
		}
		h.BodyPreview = preview(body)
		out = append(out, h)
	}
	return out, rows.Err()
}
