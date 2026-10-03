package cliproxy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func newInitialAuthsTestService(t *testing.T, auths []*coreauth.Auth, dispatched *[]string) *Service {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.AuthDir = filepath.Join(dir, "auths")
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(dir, "config.yaml")).
		WithCoreAuthManager(coreauth.NewManager(nil, nil, nil)).
		Build()
	if errBuild != nil {
		t.Fatalf("build service: %v", errBuild)
	}
	service.watcher = &WatcherWrapper{
		snapshotAuths: func() []*coreauth.Auth { return auths },
		dispatchPersistedAuthWithRev: func(update *watcher.AuthUpdate) (bool, uint64) {
			*dispatched = append(*dispatched, update.ID)
			return false, 0
		},
	}
	return service
}

// A registration that does not finish in time must not keep the HTTP listener
// closed: registerInitialAuths returns after its bounded wait and lets the
// registration complete in the background.
func TestRegisterInitialAuthsReturnsAfterBoundedWait(t *testing.T) {
	previousWait := initialAuthRegistrationWait
	initialAuthRegistrationWait = time.Millisecond
	t.Cleanup(func() { initialAuthRegistrationWait = previousWait })

	reg := internalregistry.GetGlobalRegistry()
	authID := "initial-auths-bounded-wait-claude"
	reg.UnregisterClient(authID)
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	var dispatched []string
	service := newInitialAuthsTestService(t, []*coreauth.Auth{{ID: authID, Provider: "claude", Status: coreauth.StatusActive}}, &dispatched)

	// Holding the auth update lock stands in for a registration that hangs.
	service.authUpdateMu.Lock()
	done := service.registerInitialAuths(context.Background())
	select {
	case <-done:
		t.Fatal("registration finished while it was blocked")
	default:
	}
	if models := reg.GetModelsForClient(authID); len(models) != 0 {
		t.Fatalf("models registered while the registration was blocked: %d", len(models))
	}

	service.authUpdateMu.Unlock()
	<-done
	if models := reg.GetModelsForClient(authID); len(models) == 0 {
		t.Fatal("background registration did not register the auth's models")
	}
}

// Config API-key auths are registered by registerConfigAPIKeyAuths before the
// initial scan, so registerInitialAuths must not dispatch or register them again.
func TestRegisterInitialAuthsSkipsConfigAPIKeyAuths(t *testing.T) {
	fileAuth := &coreauth.Auth{ID: "initial-auths-file-claude", Provider: "claude", Status: coreauth.StatusActive}
	configAuth := &coreauth.Auth{
		ID:       "initial-auths-config-claude",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "test-key",
			"source":    "config:claude[0]",
		},
	}
	if !coreauth.IsConfigAPIKeyAuth(configAuth) {
		t.Fatal("test fixture is not a config API-key auth")
	}
	reg := internalregistry.GetGlobalRegistry()
	for _, id := range []string{fileAuth.ID, configAuth.ID} {
		reg.UnregisterClient(id)
		t.Cleanup(func() { reg.UnregisterClient(id) })
	}

	var dispatched []string
	service := newInitialAuthsTestService(t, []*coreauth.Auth{fileAuth, configAuth}, &dispatched)
	<-service.registerInitialAuths(context.Background())

	if len(dispatched) != 1 || dispatched[0] != fileAuth.ID {
		t.Fatalf("dispatched %v, want only %s", dispatched, fileAuth.ID)
	}
	if _, ok := service.coreManager.GetByID(configAuth.ID); ok {
		t.Fatal("config API-key auth was registered by the initial scan")
	}
	if _, ok := service.coreManager.GetByID(fileAuth.ID); !ok {
		t.Fatal("file auth was not registered")
	}
}
