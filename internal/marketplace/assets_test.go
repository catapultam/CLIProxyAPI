package marketplace

import (
	"archive/zip"
	"bytes"
	"io"
	"reflect"
	"testing"
)

func TestBuildAssetIsDeterministic(t *testing.T) {
	first, errFirst := buildAsset(pluginsFS, "agentbus")
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	second, errSecond := buildAsset(pluginsFS, "agentbus")
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if !bytes.Equal(first.Zip, second.Zip) {
		t.Fatal("zip bytes differ between builds")
	}
	if first.SHA256 != second.SHA256 {
		t.Fatalf("sha256 differs: %s vs %s", first.SHA256, second.SHA256)
	}
}

func TestAgentbusZipContainsOnlyRuntimeFiles(t *testing.T) {
	asset, ok, err := Get("agentbus")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("agentbus asset not found")
	}

	zr, errOpen := zip.NewReader(bytes.NewReader(asset.Zip), int64(len(asset.Zip)))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	want := []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("zip entries = %v, want %v", names, want)
	}
}

func TestAgentbusZipFilesReadBack(t *testing.T) {
	asset, ok, err := Get("agentbus")
	if err != nil || !ok {
		t.Fatalf("Get(agentbus) = %v, %v, %v", ok, err, asset)
	}
	zr, errOpen := zip.NewReader(bytes.NewReader(asset.Zip), int64(len(asset.Zip)))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	f, errOpenFile := zr.Open(".claude-plugin/plugin.json")
	if errOpenFile != nil {
		t.Fatal(errOpenFile)
	}
	defer func() { _ = f.Close() }()
	data, errReadAll := io.ReadAll(f)
	if errReadAll != nil {
		t.Fatal(errReadAll)
	}
	if !bytes.Contains(data, []byte(`"name": "agentbus"`)) {
		t.Fatalf("plugin.json content unexpected: %s", data)
	}
	if asset.Version != "0.3.5" {
		t.Fatalf("version = %q, want 0.3.5", asset.Version)
	}
	// The mod reports its own version to the proxy, which gates commands on
	// it, so the constant in register.ts must match plugin.json.
	hooks, errOpenHooks := zr.Open("hooks/register.ts")
	if errOpenHooks != nil {
		t.Fatal(errOpenHooks)
	}
	defer func() { _ = hooks.Close() }()
	src, errReadHooks := io.ReadAll(hooks)
	if errReadHooks != nil {
		t.Fatal(errReadHooks)
	}
	if want := "export const VERSION = '" + asset.Version + "'"; !bytes.Contains(src, []byte(want)) {
		t.Fatalf("register.ts does not declare %s", want)
	}
}

func TestNamesIncludesAgentbus(t *testing.T) {
	names, err := Names()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == "agentbus" {
			found = true
		}
	}
	if !found {
		t.Fatalf("names = %v, want agentbus", names)
	}
}

func TestGetUnknownPlugin(t *testing.T) {
	if _, ok, err := Get("does-not-exist"); ok || err != nil {
		t.Fatalf("Get(unknown) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}
