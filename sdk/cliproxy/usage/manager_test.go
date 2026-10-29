package usage

import (
	"context"
	"testing"
)

func TestStreamFromContextDefaultsMissingToFalse(t *testing.T) {
	if StreamFromContext(context.Background()) {
		t.Fatalf("StreamFromContext(background) = true, want false")
	}
}

func TestStreamFromContextHonorsExplicitTrue(t *testing.T) {
	ctx := WithStream(context.Background(), true)
	if !StreamFromContext(ctx) {
		t.Fatalf("StreamFromContext(true) = false, want true")
	}
}

func TestRecordStreamField(t *testing.T) {
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
		Stream:   true,
	}
	if !record.Stream {
		t.Fatalf("Record.Stream = false, want true")
	}
}

func TestRecordBaseURLField(t *testing.T) {
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
		BaseURL:  "https://custom-gateway.example.com/v1",
	}
	if record.BaseURL != "https://custom-gateway.example.com/v1" {
		t.Fatalf("Record.BaseURL = %q, want %q", record.BaseURL, "https://custom-gateway.example.com/v1")
	}
}

func TestGenerateEnabledDefaultsNilToTrue(t *testing.T) {
	if !GenerateEnabled(nil) {
		t.Fatalf("GenerateEnabled(nil) = false, want true")
	}
}

func TestGenerateEnabledHonorsExplicitFalse(t *testing.T) {
	if GenerateEnabled(GenerateFlag(false)) {
		t.Fatalf("GenerateEnabled(false) = true, want false")
	}
}

func TestGenerateEnabledHonorsExplicitTrue(t *testing.T) {
	if !GenerateEnabled(GenerateFlag(true)) {
		t.Fatalf("GenerateEnabled(true) = false, want true")
	}
}

func TestGenerateFromContextDefaultsMissingToTrue(t *testing.T) {
	if !GenerateFromContext(context.Background()) {
		t.Fatalf("GenerateFromContext(background) = false, want true")
	}
}

func TestGenerateFromContextHonorsExplicitFalse(t *testing.T) {
	ctx := WithGenerate(context.Background(), false)
	if GenerateFromContext(ctx) {
		t.Fatalf("GenerateFromContext(false) = true, want false")
	}
}

func TestRecordOmittedGenerateIsEnabled(t *testing.T) {
	// Existing callers construct Record without setting Generate.
	// Omission must remain distinguishable from explicit false and default to true.
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
	}
	if record.Generate != nil {
		t.Fatalf("Record.Generate = %v, want nil for omitted field", record.Generate)
	}
	if !GenerateEnabled(record.Generate) {
		t.Fatalf("GenerateEnabled(omitted) = false, want true")
	}
}

type blockingPlugin struct {
	release chan struct{}
	handled chan struct{}
}

func (p *blockingPlugin) HandleUsage(context.Context, Record) {
	<-p.release
	close(p.handled)
}

func TestStopAndWaitWaitsForQueuedRecords(t *testing.T) {
	m := NewManager(1)
	p := &blockingPlugin{release: make(chan struct{}), handled: make(chan struct{})}
	m.Register(p)
	m.Publish(context.Background(), Record{Provider: "p"})

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if m.StopAndWait(expired) {
		t.Fatal("StopAndWait reported delivery while the plugin was still blocked")
	}

	close(p.release)
	if !m.StopAndWait(context.Background()) {
		t.Fatal("StopAndWait did not report delivery")
	}
	select {
	case <-p.handled:
	default:
		t.Fatal("StopAndWait returned before the plugin finished")
	}
}

func TestStopAndWaitWithoutStart(t *testing.T) {
	if !NewManager(1).StopAndWait(context.Background()) {
		t.Fatal("never-started manager must report done")
	}
}
