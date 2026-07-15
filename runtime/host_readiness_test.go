package remote

import (
	"testing"

	"github.com/OpenSlash/agent-bridge/protocol"
)

func TestUpdateHostReadinessCopiesIssues(t *testing.T) {
	service := NewService()
	service.cfg.Management = true
	readiness := protocol.HostReadiness{
		State: "blocked",
		Issues: []protocol.HostReadinessIssue{
			{Code: "auth_expired", Blocking: true},
		},
		CheckedAt: 123,
	}

	service.UpdateHostReadiness(readiness)
	readiness.Issues[0].Code = "mutated"

	snapshot := service.getHostReadinessSnapshot()
	if snapshot.State != "blocked" || snapshot.CheckedAt != 123 {
		t.Fatalf("unexpected readiness snapshot: %+v", snapshot)
	}
	if len(snapshot.Issues) != 1 || snapshot.Issues[0].Code != "auth_expired" {
		t.Fatalf("expected copied readiness issues, got %+v", snapshot.Issues)
	}

	snapshot.Issues[0].Code = "changed-again"
	second := service.getHostReadinessSnapshot()
	if second.Issues[0].Code != "auth_expired" {
		t.Fatalf("snapshot mutation leaked into service config: %+v", second.Issues)
	}
}
