package agentbus

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

// setupScript installs the wake hook on the machine that runs it.
//
//go:embed scripts/setup.sh
var setupScript string

// waiterScript is the wake hook itself, downloaded by setupScript.
//
//go:embed scripts/wait.sh
var waiterScript string

func (s *Store) handleSetup(c *gin.Context) {
	c.Data(http.StatusOK, "text/x-shellscript; charset=utf-8", []byte(setupScript))
}

func (s *Store) handleWaiterScript(c *gin.Context) {
	c.Data(http.StatusOK, "text/x-shellscript; charset=utf-8", []byte(waiterScript))
}
