package helps

import "testing"

func TestParseClaudeUsageSplitsCacheCreationByTTL(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":5,"cache_creation_input_tokens":300,"cache_read_input_tokens":1000,"output_tokens":7,
	"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}}`)
	detail := ParseClaudeUsage(data)
	if detail.CacheCreationTokens != 300 || detail.CacheCreation5mTokens != 100 || detail.CacheCreation1hTokens != 200 {
		t.Fatalf("detail = %+v", detail)
	}

	plain := ParseClaudeUsage([]byte(`{"usage":{"input_tokens":5,"cache_creation_input_tokens":300}}`))
	if plain.CacheCreation5mTokens != 0 || plain.CacheCreation1hTokens != 0 {
		t.Fatalf("split invented without upstream data: %+v", plain)
	}
}

func TestClaudeStreamUsageKeepsCacheTTLSplitAcrossEvents(t *testing.T) {
	var buf StreamUsageBuffer
	buf.ObserveClaudeStream([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"cache_creation_input_tokens":300,"cache_read_input_tokens":1000,"output_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}}}`))
	buf.ObserveClaudeStream([]byte(`data: {"type":"message_delta","usage":{"output_tokens":42}}`))
	detail, ok := buf.Detail()
	if !ok {
		t.Fatal("no usage")
	}
	if detail.OutputTokens != 42 || detail.CacheCreationTokens != 300 || detail.CacheCreation5mTokens != 100 || detail.CacheCreation1hTokens != 200 {
		t.Fatalf("merged detail = %+v", detail)
	}
}
