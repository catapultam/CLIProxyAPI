package marketplace

import "fmt"

const (
	marketplaceName        = "homelab"
	marketplaceOwner       = "catapultam"
	marketplaceDescription = "Plugins served by the CLIProxyAPI proxy"
)

// Owner identifies who maintains the marketplace.
type Owner struct {
	Name string `json:"name"`
}

// Source is a Claude Code plugin source of type "archive".
type Source struct {
	Source string `json:"source"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Plugin is one marketplace.json entry. Version is deliberately omitted:
// plugin.json's own version wins, and Claude Code only prompts to update
// when that version changes.
type Plugin struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      Source `json:"source"`
}

// Doc is the marketplace.json document served at /plugins/marketplace.json.
type Doc struct {
	Name        string   `json:"name"`
	Owner       Owner    `json:"owner"`
	Description string   `json:"description,omitempty"`
	Plugins     []Plugin `json:"plugins"`
}

// ZipFileName is the name of the zip served for one plugin's version, as
// referenced by the marketplace.json entry and the /plugins/:file route.
func ZipFileName(name, version string) string {
	return fmt.Sprintf("%s-%s.zip", name, version)
}

// BuildDoc builds the marketplace.json document for every embedded plugin,
// with archive URLs rooted at base (e.g. "https://host:port").
func BuildDoc(base string) (Doc, error) {
	names, errNames := Names()
	if errNames != nil {
		return Doc{}, errNames
	}
	doc := Doc{
		Name:        marketplaceName,
		Owner:       Owner{Name: marketplaceOwner},
		Description: marketplaceDescription,
		Plugins:     make([]Plugin, 0, len(names)),
	}
	for _, name := range names {
		asset, ok, errGet := Get(name)
		if errGet != nil {
			return Doc{}, errGet
		}
		if !ok {
			continue
		}
		doc.Plugins = append(doc.Plugins, Plugin{
			Name:        asset.Name,
			Description: asset.Description,
			Source: Source{
				Source: "archive",
				URL:    base + "/plugins/" + ZipFileName(asset.Name, asset.Version),
				SHA256: asset.SHA256,
			},
		})
	}
	return doc, nil
}
