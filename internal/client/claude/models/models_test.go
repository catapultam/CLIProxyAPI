package models

import "testing"

func TestBuildResponse(t *testing.T) {
	availableModels := []map[string]any{
		{"id": "claude-z", "display_name": "Zebra", "max_tokens": 64000},
		{"id": "gpt-4o", "display_name": "Alpha"},
		{"id": "claude-c", "display_name": "Alpha"},
		{"id": "claude-b", "display_name": "Beta"},
	}

	response := BuildResponse(availableModels, false, nil)
	models, ok := response["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want []map[string]any", response["data"])
	}

	wantIDs := []string{
		"claude-c",
		"claude-fable-5-dd-o4-tpg",
		"claude-b",
		"claude-z",
	}
	if len(models) != len(wantIDs) {
		t.Fatalf("len(data) = %d, want %d", len(models), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got, _ := models[i]["id"].(string); got != want {
			t.Fatalf("data[%d].id = %q, want %q", i, got, want)
		}
	}
	if got := models[3]["max_tokens"]; got != 64000 {
		t.Fatalf("max_tokens = %v, want 64000", got)
	}
	if got := response["has_more"]; got != false {
		t.Fatalf("has_more = %v, want false", got)
	}
	if got := response["first_id"]; got != wantIDs[0] {
		t.Fatalf("first_id = %v, want %q", got, wantIDs[0])
	}
	if got := response["last_id"]; got != wantIDs[len(wantIDs)-1] {
		t.Fatalf("last_id = %v, want %q", got, wantIDs[len(wantIDs)-1])
	}

	if got := availableModels[1]["id"]; got != "gpt-4o" {
		t.Fatalf("BuildResponse mutated input id to %v", got)
	}
	if got := availableModels[0]["id"]; got != "claude-z" {
		t.Fatalf("BuildResponse reordered input: first id = %v", got)
	}
}

func TestBuildResponseWithCloakingDisabled(t *testing.T) {
	availableModels := []map[string]any{
		{"id": "gpt-4o", "display_name": "GPT-4o"},
	}

	response := BuildResponse(availableModels, true, nil)
	models, ok := response["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want []map[string]any", response["data"])
	}
	if len(models) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(models))
	}
	if got := models[0]["id"]; got != "gpt-4o" {
		t.Fatalf("data[0].id = %v, want gpt-4o", got)
	}
	if got := response["first_id"]; got != "gpt-4o" {
		t.Fatalf("first_id = %v, want gpt-4o", got)
	}
	if got := response["last_id"]; got != "gpt-4o" {
		t.Fatalf("last_id = %v, want gpt-4o", got)
	}
}

func TestBuildResponseEmpty(t *testing.T) {
	response := BuildResponse(nil, false, nil)
	models, ok := response["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want []map[string]any", response["data"])
	}
	if len(models) != 0 {
		t.Fatalf("len(data) = %d, want 0", len(models))
	}
	if response["first_id"] != "" || response["last_id"] != "" {
		t.Fatalf("empty response IDs = (%v, %v), want empty", response["first_id"], response["last_id"])
	}
}

func TestEnsureClaudeModelIDPrefix(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"empty", "", ""},
		{"already has claude prefix", "claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"contains claude mid-string is reversed", "my-claude-custom", "claude-fable-5-dd-motsuc-edualc-ym"},
		{"uppercase Claude prefix is reversed", "Claude-Opus-4", "claude-fable-5-dd-4-supO-edualC"},
		{"gpt model is reversed", "gpt-4o", "claude-fable-5-dd-o4-tpg"},
		{"gemini model is reversed", "gemini-2.5-pro", "claude-fable-5-dd-orp-5.2-inimeg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EnsureClaudeModelIDPrefix(tt.id); got != tt.want {
				t.Fatalf("EnsureClaudeModelIDPrefix(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestResolveClaudeModelIDPrefix(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"empty", "", ""},
		{"plain claude id unchanged", "claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"non encoded id unchanged", "gpt-4o", "gpt-4o"},
		{"encoded gpt model", "claude-fable-5-dd-o4-tpg", "gpt-4o"},
		{"encoded gemini model", "claude-fable-5-dd-orp-5.2-inimeg", "gemini-2.5-pro"},
		{"empty encoded body unchanged", "claude-fable-5-dd-", "claude-fable-5-dd-"},
		{"preserves thinking suffix", "claude-fable-5-dd-o4-tpg(high)", "gpt-4o(high)"},
		{"round trip", EnsureClaudeModelIDPrefix("custom-model-x"), "custom-model-x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveClaudeModelIDPrefix(tt.id); got != tt.want {
				t.Fatalf("ResolveClaudeModelIDPrefix(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestBuildResponseHiddenModels(t *testing.T) {
	newInput := func() []map[string]any {
		return []map[string]any{
			{"id": "claude-opus-5", "display_name": "Opus 5"},
			{"id": "claude-opus-4-8", "display_name": "Opus 4.8"},
			{"id": "claude-sonnet-5", "display_name": "Sonnet 5"},
			{"id": "claude-haiku-5", "display_name": "Haiku 5"},
			{"id": "gpt-5", "display_name": "GPT 5"},
		}
	}
	ids := func(t *testing.T, response map[string]any) []string {
		t.Helper()
		models, ok := response["data"].([]map[string]any)
		if !ok {
			t.Fatalf("data type = %T, want []map[string]any", response["data"])
		}
		out := make([]string, len(models))
		for i, model := range models {
			out[i], _ = model["id"].(string)
		}
		return out
	}

	tests := []struct {
		name            string
		hidden          []string
		disableCloaking bool
		want            []string
	}{
		{"exact", []string{"claude-opus-5"}, true, []string{"claude-opus-4-8", "claude-sonnet-5", "claude-haiku-5", "gpt-5"}},
		{"case-insensitive and trimmed", []string{"  CLAUDE-Opus-5 ", ""}, true, []string{"claude-opus-4-8", "claude-sonnet-5", "claude-haiku-5", "gpt-5"}},
		{"prefix wildcard", []string{"claude-opus-*"}, true, []string{"claude-sonnet-5", "claude-haiku-5", "gpt-5"}},
		{"suffix and substring wildcard", []string{"*-5", "*opus*"}, true, []string{}},
		{"non-Claude hidden by original ID while cloaking", []string{"gpt-5"}, false, []string{"claude-opus-5", "claude-opus-4-8", "claude-sonnet-5", "claude-haiku-5"}},
		{"cloaked ID does not match", []string{EnsureClaudeModelIDPrefix("gpt-5")}, false, nil},
		{"empty list hides nothing", nil, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := BuildResponse(newInput(), tt.disableCloaking, tt.hidden)
			got := ids(t, response)
			if tt.want == nil {
				if len(got) != 5 {
					t.Fatalf("len(data) = %d, want 5 (%v)", len(got), got)
				}
				return
			}
			want := map[string]bool{}
			for _, id := range tt.want {
				want[id] = true
			}
			if len(got) != len(want) {
				t.Fatalf("ids = %v, want %v", got, tt.want)
			}
			for _, id := range got {
				if !want[id] {
					t.Fatalf("ids = %v, want %v", got, tt.want)
				}
			}
			wantFirst, wantLast := "", ""
			if len(got) > 0 {
				wantFirst, wantLast = got[0], got[len(got)-1]
			}
			if response["first_id"] != wantFirst || response["last_id"] != wantLast {
				t.Fatalf("first_id/last_id = %v/%v, want %s/%s", response["first_id"], response["last_id"], wantFirst, wantLast)
			}
		})
	}

	t.Run("first_id and last_id come from filtered list", func(t *testing.T) {
		response := BuildResponse(newInput(), true, []string{"claude-haiku-5", "gpt-5"})
		// Sorted by display name: Opus 4.8, Opus 5, Sonnet 5.
		if response["first_id"] != "claude-opus-4-8" || response["last_id"] != "claude-sonnet-5" {
			t.Fatalf("first_id/last_id = %v/%v", response["first_id"], response["last_id"])
		}
	})

	t.Run("input is not mutated", func(t *testing.T) {
		input := newInput()
		BuildResponse(input, false, []string{"claude-opus-5", "gpt-5"})
		if len(input) != 5 || input[0]["id"] != "claude-opus-5" || input[4]["id"] != "gpt-5" {
			t.Fatalf("input mutated: %v", input)
		}
	})
}
