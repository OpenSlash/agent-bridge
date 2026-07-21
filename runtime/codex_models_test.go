package remote

import "testing"

func TestRuntimeModelInfoFromCodexEntryPreservesReasoningCapabilities(t *testing.T) {
	model, ok := runtimeModelInfoFromCodexEntry(codexModelListEntry{
		ID: "gpt-5.6-sol", DisplayName: "GPT-5.6-Sol", Description: "Latest model",
		IsDefault: true, DefaultReasoningEffort: "low",
		SupportedReasoningEfforts: []codexReasoningEffortOption{
			{ReasoningEffort: "low", Description: "Fast"},
			{ReasoningEffort: "ultra", Description: "Delegates tasks"},
		},
	})
	if !ok || model.ID != "gpt-5.6-sol" || !model.IsDefault {
		t.Fatalf("unexpected model: %#v, ok=%t", model, ok)
	}
	if model.DefaultReasoningEffort != "low" || len(model.SupportedReasoningEfforts) != 2 {
		t.Fatalf("reasoning capabilities were not preserved: %#v", model)
	}
	if model.SupportedReasoningEfforts[1].ID != "ultra" || model.SupportedReasoningEfforts[1].Title != "Ultra" {
		t.Fatalf("unexpected reasoning effort: %#v", model.SupportedReasoningEfforts[1])
	}
}

func TestRuntimeModelInfoFromCodexEntryRejectsHiddenModel(t *testing.T) {
	if _, ok := runtimeModelInfoFromCodexEntry(codexModelListEntry{ID: "hidden", Hidden: true}); ok {
		t.Fatal("expected hidden model to be rejected")
	}
}
