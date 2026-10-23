package config

import "testing"

func TestParseConfigBytesClaudeCodeModelListCloaking(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "defaults to enabled cloaking",
			yaml: "port: 8317\n",
			want: false,
		},
		{
			name: "disables model list cloaking",
			yaml: "claude-code:\n  disable-cloaking-model-list: true\n",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.ClaudeCode.DisableCloakingModelList; got != tt.want {
				t.Fatalf("DisableCloakingModelList = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestParseConfigBytesClaudeModelLevelCooling(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "defaults to false",
			yaml: "port: 8317\n",
			want: false,
		},
		{
			name: "enables model-level cooling",
			yaml: "claude:\n  model-level-cooling: true\n",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.Claude.ModelLevelCooling; got != tt.want {
				t.Fatalf("cfg.Claude.ModelLevelCooling = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestParseConfigBytesClaudeCodeHiddenModels(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"legacy claude-code", "claude-code:\n  hidden-models: [claude-opus-5, 'claude-sonnet-*']\n"},
		{"v8 upstream.claude", "upstream:\n  claude:\n    hidden-models: [claude-opus-5, 'claude-sonnet-*']\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			got := cfg.ClaudeCode.HiddenModels
			if len(got) != 2 || got[0] != "claude-opus-5" || got[1] != "claude-sonnet-*" {
				t.Fatalf("HiddenModels = %v", got)
			}
		})
	}
}
