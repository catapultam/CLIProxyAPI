package usage

import (
	"context"
	"testing"
"net/http"
"runtime"
"time"
"weak"
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

type notifyUsagePlugin struct {
	delivered chan struct{}
}

func (p *notifyUsagePlugin) HandleUsage(_ context.Context, _ Record) {
	p.delivered <- struct{}{}
}

type retainedRequestPayload struct {
	data [64 << 10]byte
}

type retainedRequestContextKey struct{}

//go:noinline
func publishConsumableRecord(mgr *Manager) (weak.Pointer[retainedRequestPayload], weak.Pointer[CacheDetails], weak.Pointer[string]) {
	ctxPayload := &retainedRequestPayload{}
	cacheDetails := &CacheDetails{}
	headerSlice := []string{"req-header-value"}

	ctx := context.WithValue(context.Background(), retainedRequestContextKey{}, ctxPayload)
	mgr.Publish(ctx, Record{
		Provider:        "openai",
		Model:           "gpt-5.4",
		ResponseHeaders: http.Header{"X-Request-Id": headerSlice},
		Detail: Detail{
			CacheDetails: cacheDetails,
		},
	})

	return weak.Make(ctxPayload), weak.Make(cacheDetails), weak.Make(&headerSlice[0])
}

func TestManagerReleasesConsumedQueueItemReferencesWhenIdle(t *testing.T) {
	mgr := NewManager(10)
	t.Cleanup(mgr.Stop)

	plugin := &notifyUsagePlugin{delivered: make(chan struct{}, 1)}
	mgr.Register(plugin)

	ctxWeak, detailWeak, headerWeak := publishConsumableRecord(mgr)

	select {
	case <-plugin.delivered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record dispatch")
	}

	for range 10 {
		runtime.Gosched()
		runtime.GC()
		if ctxWeak.Value() == nil && detailWeak.Value() == nil && headerWeak.Value() == nil {
			break
		}
	}

	if ctxWeak.Value() != nil {
		t.Fatalf("consumed request context payload is still retained while manager is idle")
	}
	if detailWeak.Value() != nil {
		t.Fatalf("consumed record Detail.CacheDetails is still retained while manager is idle")
	}
	if headerWeak.Value() != nil {
		t.Fatalf("consumed record ResponseHeaders backing value is still retained while manager is idle")
	}
}
