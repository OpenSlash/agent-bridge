package remote

import (
	"strings"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"
)

func (s *Service) AddMessageObserver(observer func(protocol.Message) error) func() {
	if observer == nil {
		return func() {}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.messageObservers == nil {
		s.messageObservers = make(map[int]messageSink)
	}
	id := s.nextMessageObserverID
	s.nextMessageObserverID++
	s.messageObservers[id] = messageSink(observer)

	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.messageObservers, id)
	}
}

func (s *Service) notifyMessageObservers(msg protocol.Message) {
	s.mu.Lock()
	observers := make([]messageSink, 0, len(s.messageObservers))
	for _, observer := range s.messageObservers {
		observers = append(observers, observer)
	}
	s.mu.Unlock()

	for _, observer := range observers {
		if err := observer(msg); err != nil {
			applog.Errorf("[Remote] message observer error: %v", err)
		}
	}
}

func (s *Service) emitLocalSessionLifecycle(sessionID, state, reason string) {
	sessionID = strings.TrimSpace(sessionID)
	state = strings.TrimSpace(state)
	reason = strings.TrimSpace(reason)
	if sessionID == "" || state == "" {
		return
	}

	msg := protocol.Message{
		Type:      protocol.TypeSessionLifecycle,
		SessionID: sessionID,
		Payload: protocol.SessionLifecyclePayload{
			State:      state,
			Reason:     reason,
			OccurredAt: time.Now().UnixMilli(),
		},
	}

	s.mu.Lock()
	localMode := s.localMode
	sink := s.sink
	s.mu.Unlock()

	if localMode && sink != nil {
		_ = sink(msg)
	}
	s.notifyMessageObservers(msg)
}
