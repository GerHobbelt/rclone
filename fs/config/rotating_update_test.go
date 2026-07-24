package config_test

import (
	"context"
	"testing"

	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/rc"
	"github.com/stretchr/testify/require"
)

// TestUpdateRemoteRejectsTerminalRotatingToken verifies that a normal config
// update cannot turn an uncertain one-time credential into a legacy token.
func TestUpdateRemoteRejectsTerminalRotatingToken(t *testing.T) {
	defer testConfigFile(t, simpleOptions, "rotating-update.conf")()

	config.FileSetValue("remote", "type", "config_test_remote")
	config.FileSetValue("remote", config.ConfigToken, `{"access_token":"old","refresh_token":"old-refresh","rclone_token_state":{"version":1,"generation":7,"status":"uncertain"}}`)
	config.SaveConfig()

	_, err := config.UpdateRemote(context.Background(), "remote", rc.Params{
		config.ConfigToken: `{"refresh_token":"new-refresh"}`,
	}, config.UpdateRemoteOpt{NonInteractive: true})
	require.ErrorContains(t, err, `rclone config reconnect remote:`)

	stored, found := config.FileGetValue("remote", config.ConfigToken)
	require.True(t, found)
	require.Contains(t, stored, `"status":"uncertain"`)
	require.Contains(t, stored, `old-refresh`)

	_, err = config.UnsetRemote("remote", config.ConfigToken)
	require.ErrorContains(t, err, `rclone config reconnect remote:`)
}

// TestUpdateRemoteRejectsManagedRotatingToken verifies that a normal config
// update cannot discard state metadata from an otherwise ready credential.
func TestUpdateRemoteRejectsManagedRotatingToken(t *testing.T) {
	defer testConfigFile(t, simpleOptions, "rotating-ready-update.conf")()

	config.FileSetValue("remote", "type", "config_test_remote")
	config.FileSetValue("remote", config.ConfigToken, `{"access_token":"old","refresh_token":"old-refresh","rclone_token_state":{"version":1,"generation":7,"status":"ready"}}`)
	config.SaveConfig()

	_, err := config.UpdateRemote(context.Background(), "remote", rc.Params{
		config.ConfigToken: `{"refresh_token":"new-refresh"}`,
	}, config.UpdateRemoteOpt{NonInteractive: true})
	require.ErrorContains(t, err, `rclone config reconnect remote:`)

	stored, found := config.FileGetValue("remote", config.ConfigToken)
	require.True(t, found)
	require.Contains(t, stored, `"status":"ready"`)
	require.Contains(t, stored, `old-refresh`)

	_, err = config.UnsetRemote("remote", config.ConfigToken)
	require.ErrorContains(t, err, `rclone config reconnect remote:`)
}
