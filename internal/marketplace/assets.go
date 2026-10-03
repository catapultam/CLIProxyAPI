package marketplace

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// pluginsFS holds every plugin this proxy can serve. The "all:" prefix keeps
// dot-directories such as .claude-plugin, which go:embed otherwise skips.
//
//go:embed all:plugins
var pluginsFS embed.FS

const pluginsRoot = "plugins"

// zipModTime is fixed so the embedded files always zip to the same bytes.
var zipModTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// runtimeExcluded lists plugin-relative paths that are not shipped in the
// zip: tests and the TypeScript project config are development-only.
func runtimeExcluded(rel string) bool {
	return rel == "tsconfig.json" || rel == "tests" || strings.HasPrefix(rel, "tests/")
}

// Asset is one plugin's built, cached artifact.
type Asset struct {
	Name        string
	Version     string
	Description string
	Zip         []byte
	SHA256      string
}

var (
	loadOnce sync.Once
	assets   map[string]Asset
	loadErr  error
)

// load builds every plugin's zip once per process.
func load() {
	loadOnce.Do(func() {
		built, err := buildAll(pluginsFS)
		assets, loadErr = built, err
	})
}

// buildAll builds one Asset per top-level directory under plugins/.
func buildAll(fsys embed.FS) (map[string]Asset, error) {
	entries, errRead := fsys.ReadDir(pluginsRoot)
	if errRead != nil {
		return nil, fmt.Errorf("marketplace: read plugins dir: %w", errRead)
	}
	out := make(map[string]Asset, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		asset, errBuild := buildAsset(fsys, entry.Name())
		if errBuild != nil {
			return nil, errBuild
		}
		out[asset.Name] = asset
	}
	return out, nil
}

// buildAsset zips one plugin directory and reads its manifest.
func buildAsset(fsys embed.FS, dir string) (Asset, error) {
	root := path.Join(pluginsRoot, dir)
	manifest, errManifest := readManifest(fsys, root)
	if errManifest != nil {
		return Asset{}, errManifest
	}
	if manifest.Name != dir {
		return Asset{}, fmt.Errorf("marketplace: plugin.json name %q does not match directory %q", manifest.Name, dir)
	}

	var rels []string
	errWalk := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, errWalk error) error {
		if errWalk != nil {
			return errWalk
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, root+"/")
		if runtimeExcluded(rel) {
			return nil
		}
		rels = append(rels, rel)
		return nil
	})
	if errWalk != nil {
		return Asset{}, fmt.Errorf("marketplace: walk plugin %q: %w", dir, errWalk)
	}
	sort.Strings(rels)

	zipBytes, errZip := buildZip(fsys, root, rels)
	if errZip != nil {
		return Asset{}, fmt.Errorf("marketplace: zip plugin %q: %w", dir, errZip)
	}
	sum := sha256.Sum256(zipBytes)

	return Asset{
		Name:        manifest.Name,
		Version:     manifest.Version,
		Description: manifest.Description,
		Zip:         zipBytes,
		SHA256:      hex.EncodeToString(sum[:]),
	}, nil
}

type pluginManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

func readManifest(fsys embed.FS, root string) (pluginManifest, error) {
	data, errRead := fsys.ReadFile(path.Join(root, ".claude-plugin", "plugin.json"))
	if errRead != nil {
		return pluginManifest{}, fmt.Errorf("marketplace: read plugin.json under %q: %w", root, errRead)
	}
	var m pluginManifest
	if errUnmarshal := json.Unmarshal(data, &m); errUnmarshal != nil {
		return pluginManifest{}, fmt.Errorf("marketplace: parse plugin.json under %q: %w", root, errUnmarshal)
	}
	if m.Name == "" || m.Version == "" {
		return pluginManifest{}, fmt.Errorf("marketplace: plugin.json under %q is missing name or version", root)
	}
	return m, nil
}

// buildZip deterministically zips rels (already sorted) from root, with
// plugin files at the zip root, forward slashes, deflate, and a fixed
// modified time, so identical inputs always produce identical bytes.
func buildZip(fsys embed.FS, root string, rels []string) ([]byte, error) {
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for _, rel := range rels {
		data, errRead := fsys.ReadFile(path.Join(root, rel))
		if errRead != nil {
			return nil, errRead
		}
		hdr := &zip.FileHeader{
			Name:     rel,
			Method:   zip.Deflate,
			Modified: zipModTime,
		}
		w, errCreate := zw.CreateHeader(hdr)
		if errCreate != nil {
			return nil, errCreate
		}
		if _, errWrite := w.Write(data); errWrite != nil {
			return nil, errWrite
		}
	}
	if errClose := zw.Close(); errClose != nil {
		return nil, errClose
	}
	return buf.Bytes(), nil
}

// Names returns the loaded plugin names, sorted.
func Names() ([]string, error) {
	load()
	if loadErr != nil {
		return nil, loadErr
	}
	names := make([]string, 0, len(assets))
	for name := range assets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Get returns one plugin's built asset.
func Get(name string) (Asset, bool, error) {
	load()
	if loadErr != nil {
		return Asset{}, false, loadErr
	}
	a, ok := assets[name]
	return a, ok, nil
}
