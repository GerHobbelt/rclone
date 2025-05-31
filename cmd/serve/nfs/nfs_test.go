//go:build unix

// The serving is tested in cmd/nfsmount - here we test anything else
package nfs

import (
	"testing"

	_ "github.com/artpar/rclone/backend/local"
	"github.com/artpar/rclone/cmd/serve/servetest"
	"github.com/artpar/rclone/fs/rc"
)

func TestRc(t *testing.T) {
	servetest.TestRc(t, rc.Params{
		"type":           "nfs",
		"vfs_cache_mode": "off",
	})
}
