package remote

import (
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

const (
	defaultCodexSessionLimit = 100
	maxCodexSessionLimit     = 200
)

func (s *Service) handleListCodexSessions(sessionID string, req protocol.ListCodexSessionsPayload) {
	startedAt := time.Now()
	resp, err := buildCodexSessionsResponse(req)
	if err != nil {
		applog.Errorf("[Remote] list-codex-sessions failed: session=%s request=%s cwd=%s err=%v", sessionID, req.RequestID, req.Cwd, err)
		resp = protocol.ListCodexSessionsResponsePayload{
			RequestID: req.RequestID,
			Sessions:  []protocol.CodexLocalSessionEntry{},
			Error:     err.Error(),
		}
	} else {
		applog.Info.Printf(
			"[Remote] list-codex-sessions resolved: session=%s request=%s cwd=%s all=%t count=%d duration=%s",
			sessionID,
			req.RequestID,
			req.Cwd,
			req.IncludeAll,
			len(resp.Sessions),
			time.Since(startedAt).Round(time.Millisecond),
		)
	}

	if err := s.writeJSON(protocol.Message{
		Type:      protocol.TypeListCodexSessionsResponse,
		SessionID: sessionID,
		Payload:   resp,
	}); err != nil {
		applog.Errorf("[Remote] WS write list-codex-sessions-response error: %v", err)
	}
}

func buildCodexSessionsResponse(req protocol.ListCodexSessionsPayload) (protocol.ListCodexSessionsResponsePayload, error) {
	sessions, err := ListCodexLocalSessions(strings.TrimSpace(req.Cwd), req.IncludeAll)
	if err != nil {
		return protocol.ListCodexSessionsResponsePayload{}, err
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultCodexSessionLimit
	}
	if limit > maxCodexSessionLimit {
		limit = maxCodexSessionLimit
	}
	if len(sessions) > limit {
		sessions = sessions[:limit]
	}

	entries := make([]protocol.CodexLocalSessionEntry, 0, len(sessions))
	for _, session := range sessions {
		entries = append(entries, protocol.CodexLocalSessionEntry{
			RuntimeSessionID: session.RuntimeSessionID,
			Cwd:              session.Cwd,
			ModelProvider:    session.ModelProvider,
			CLIVersion:       session.CLIVersion,
			Source:           session.Source,
			Originator:       session.Originator,
			SessionTime:      unixMillis(session.SessionTime),
			UpdatedAt:        unixMillis(session.ModTime),
			LineCount:        session.LineCount,
		})
	}

	return protocol.ListCodexSessionsResponsePayload{
		RequestID: req.RequestID,
		Sessions:  entries,
	}, nil
}

func unixMillis(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}
