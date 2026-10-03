package cliproxy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// TestRunRegistersInitialAuthModelsBeforeServerStart guards against the startup race
// where the HTTP listener accepted requests before the watcher's initial auth scan had
// registered any models, so the first requests after a restart failed with 400
// "unknown provider for model".
func TestRunRegistersInitialAuthModelsBeforeServerStart(t *testing.T) {
	reg := internalregistry.GetGlobalRegistry()
	authID := "startup-registration-test-claude"
	reg.UnregisterClient(authID)
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = 0
	cfg.AuthDir = filepath.Join(dir, "auths")

	watcherStarted := false
	factory := func(string, string, func(*config.Config)) (*WatcherWrapper, error) {
		return &WatcherWrapper{
			start: func(context.Context) error {
				watcherStarted = true
				return nil
			},
			snapshotAuths: func() []*coreauth.Auth {
				return []*coreauth.Auth{{
					ID:       authID,
					Provider: "claude",
					Status:   coreauth.StatusActive,
				}}
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hookCalled := false
	var startedBeforeListener bool
	var modelsBeforeListener []*ModelInfo
	var providersBeforeListener []string
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(dir, "config.yaml")).
		WithCoreAuthManager(coreauth.NewManager(nil, nil, nil)).
		WithWatcherFactory(factory).
		WithHooks(Hooks{OnBeforeStart: func(*config.Config) {
			// Runs on the Run goroutine immediately before the listener goroutine starts.
			hookCalled = true
			startedBeforeListener = watcherStarted
			modelsBeforeListener = reg.GetModelsForClient(authID)
			if len(modelsBeforeListener) > 0 {
				providersBeforeListener = util.GetProviderName(modelsBeforeListener[0].ID)
			}
			cancel()
		}}).
		Build()
	if errBuild != nil {
		t.Fatalf("build service: %v", errBuild)
	}

	if errRun := service.Run(ctx); errRun != nil && !errors.Is(errRun, context.Canceled) {
		t.Fatalf("run service: %v", errRun)
	}

	if !hookCalled {
		t.Fatal("OnBeforeStart hook was not called")
	}
	if startedBeforeListener {
		t.Fatal("watcher started before the listener; expected the initial registration to run synchronously instead")
	}
	if len(modelsBeforeListener) == 0 {
		t.Fatal("no models registered for the initial auth before the HTTP listener started")
	}
	if len(providersBeforeListener) == 0 {
		t.Fatalf("model %q has no provider before the HTTP listener started", modelsBeforeListener[0].ID)
	}
}
