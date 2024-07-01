//go:build system

package tests

import (
	"net/http"
	"testing"

	cluster "github.com/andydunstall/piko/workloadv2/cluster"
	"github.com/stretchr/testify/assert"
)

// Tests the admin server.
func TestAdmin(t *testing.T) {
	// Tests /health returns 200.
	t.Run("health", func(t *testing.T) {
		node := cluster.NewNode()
		node.Start()
		defer node.Stop()

		resp, err := http.Get("http://" + node.AdminAddr() + "/health")
		assert.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	// Tests /ready returns 200.
	t.Run("ready", func(t *testing.T) {
		node := cluster.NewNode()
		node.Start()
		defer node.Stop()

		resp, err := http.Get("http://" + node.AdminAddr() + "/ready")
		assert.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	// Tests /metrics returns 200.
	t.Run("metrics", func(t *testing.T) {
		node := cluster.NewNode()
		node.Start()
		defer node.Stop()

		resp, err := http.Get("http://" + node.AdminAddr() + "/metrics")
		assert.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}
