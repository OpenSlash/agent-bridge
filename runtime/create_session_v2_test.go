package remote

import (
	"testing"
	"time"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestCreateSessionKeyPrefersStableIdempotencyKey(t *testing.T) {
	req := protocol.CreateSessionPayload{
		RequestID:      "request-2",
		IdempotencyKey: "stable-create-1",
	}
	if got := createSessionKey(req); got != "stable-create-1" {
		t.Fatalf("expected stable idempotency key, got %q", got)
	}
}

func TestBeginCreateSessionWaitsForOriginalResult(t *testing.T) {
	service := NewService()
	req := protocol.CreateSessionPayload{
		RequestID:      "request-1",
		IdempotencyKey: "stable-create-1",
	}
	if _, duplicate := service.beginCreateSession(req); duplicate {
		t.Fatal("first request must own the create attempt")
	}

	result := make(chan protocol.SessionCreatedPayload, 1)
	go func() {
		payload, duplicate := service.beginCreateSession(protocol.CreateSessionPayload{
			RequestID:      "request-retry",
			IdempotencyKey: "stable-create-1",
		})
		if !duplicate {
			result <- protocol.SessionCreatedPayload{Error: "retry unexpectedly became owner"}
			return
		}
		result <- payload
	}()

	select {
	case <-result:
		t.Fatal("duplicate request returned before the original completed")
	case <-time.After(20 * time.Millisecond):
	}

	service.mu.Lock()
	attempt := service.createSessionAttempts["stable-create-1"]
	attempt.payload = protocol.SessionCreatedPayload{SessionID: "session-1"}
	close(attempt.done)
	service.mu.Unlock()

	select {
	case payload := <-result:
		if payload.SessionID != "session-1" {
			t.Fatalf("expected cached session, got %#v", payload)
		}
		if payload.RequestID != "request-retry" {
			t.Fatalf("expected retry request id, got %q", payload.RequestID)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate request did not receive the original result")
	}
}
