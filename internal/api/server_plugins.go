package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/marketplace"
	log "github.com/sirupsen/logrus"
)

// registerPluginRoutes serves the Claude Code plugin marketplace from
// embedded plugin sources. It is mounted outside the /v1 group: Claude
// Code's marketplace and archive fetchers send no API key, and this proxy
// is reachable only on the tailnet.
func (s *Server) registerPluginRoutes() {
	s.engine.GET("/plugins/:file", s.handlePluginsFile)
}

// handlePluginsFile dispatches on the requested file name: the marketplace
// manifest, or one plugin's versioned zip archive.
func (s *Server) handlePluginsFile(c *gin.Context) {
	file := c.Param("file")
	if file == "marketplace.json" {
		s.handleMarketplaceManifest(c)
		return
	}
	s.handlePluginArchive(c, file)
}

func (s *Server) handleMarketplaceManifest(c *gin.Context) {
	doc, errBuild := marketplace.BuildDoc(marketplace.BaseURL(c.Request))
	if errBuild != nil {
		log.Errorf("marketplace: build manifest: %v", errBuild)
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Header("Cache-Control", "no-cache")
	c.JSON(http.StatusOK, doc)
}

func (s *Server) handlePluginArchive(c *gin.Context, file string) {
	names, errNames := marketplace.Names()
	if errNames != nil {
		log.Errorf("marketplace: list plugins: %v", errNames)
		c.Status(http.StatusInternalServerError)
		return
	}
	for _, name := range names {
		asset, ok, errGet := marketplace.Get(name)
		if errGet != nil {
			log.Errorf("marketplace: load plugin %q: %v", name, errGet)
			c.Status(http.StatusInternalServerError)
			return
		}
		if !ok || file != marketplace.ZipFileName(asset.Name, asset.Version) {
			continue
		}
		c.Data(http.StatusOK, "application/zip", asset.Zip)
		return
	}
	c.Status(http.StatusNotFound)
}
