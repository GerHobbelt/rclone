package _115

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/115/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// TestNewFsUsesPersistentTokenSection verifies that a runtime override suffix
// never becomes part of the section that owns the rotating token.
func TestNewFsUsesPersistentTokenSection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	tokenJSON, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(2 * time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[abc]\ntype = 115\ntoken = "+string(tokenJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	regInfo, err := fs.Find("115")
	require.NoError(t, err)
	mapper := fs.ConfigMap(regInfo.Prefix, regInfo.Options, "abc", configmap.Simple{
		"root_folder_id": "root",
	})
	gotFs, err := NewFs(context.Background(), "abc{A1fie}", "", mapper)
	require.NoError(t, err)
	backend, ok := gotFs.(*Fs)
	require.True(t, ok)
	defer func() { require.NoError(t, backend.Shutdown(context.Background())) }()

	require.NotNil(t, backend.rotatingToken)
	persisted, ok := config.FileGetValue("abc", config.ConfigToken)
	require.True(t, ok)
	require.Contains(t, persisted, `"rclone_token_state"`)
	_, suffixed := config.FileGetValue("abc{A1fie}", config.ConfigToken)
	require.False(t, suffixed)
}

// TestLateTokenFailureAdoptsPersistedGeneration verifies that a request made
// with an earlier generation does not consume the replacement refresh token.
func TestLateTokenFailureAdoptsPersistedGeneration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	tokenJSON, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntype = 115\ntoken = "+string(tokenJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges int
	exchange := func(context.Context, string) (*oauth2.Token, error) {
		exchanges++
		return &oauth2.Token{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	}
	mapper := fs.ConfigMap("", nil, "remote", nil)
	sourceA, err := oauthutil.NewRotatingTokenSource(context.Background(), "remote", mapper, exchange)
	require.NoError(t, err)
	sourceB, err := oauthutil.NewRotatingTokenSource(context.Background(), "remote", mapper, exchange)
	require.NoError(t, err)

	backend := &Fs{rotatingToken: sourceA}
	opts := rest.Opts{}
	require.NoError(t, backend.prepareTokenForRequest(context.Background(), &opts))
	require.Equal(t, "Bearer access-1", opts.ExtraHeaders["Authorization"])

	_, generation, err := sourceB.RefreshIfCurrent(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, uint64(2), generation)

	retry, err := backend.handleTokenError(context.Background(), &opts, api.NewTokenError("late access-token failure"), false)
	require.NoError(t, err)
	require.True(t, retry)
	require.Equal(t, "Bearer access-2", opts.ExtraHeaders["Authorization"])
	require.Equal(t, 1, exchanges)
}

// TestRefreshTokenRejectionDoesNotRefreshAgain verifies that a provider's
// definitive refresh-token rejection becomes a terminal durable state instead
// of sending that refresh token back to the provider.
func TestRefreshTokenRejectionDoesNotRefreshAgain(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	tokenJSON, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntype = 115\ntoken = "+string(tokenJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges int
	source, err := oauthutil.NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), func(context.Context, string) (*oauth2.Token, error) {
		exchanges++
		return nil, nil
	})
	require.NoError(t, err)
	backend := &Fs{rotatingToken: source}
	opts := rest.Opts{}
	require.NoError(t, backend.prepareTokenForRequest(context.Background(), &opts))

	retry, err := backend.handleTokenError(context.Background(), &opts, api.NewTokenError("refresh token expired", true), false)
	require.False(t, retry)
	require.ErrorIs(t, err, oauthutil.ErrTokenReauthenticationRequired)
	require.Equal(t, 0, exchanges)

	persisted, found := config.FileGetValue("remote", config.ConfigToken)
	require.True(t, found)
	require.Contains(t, persisted, `"status":"reauth_required"`)
}

// TestConfigReconnectRequestsCookie verifies that terminal token recovery uses
// the explicit config reconnect flow rather than an automatic cookie login.
func TestConfigReconnectRequestsCookie(t *testing.T) {
	mapper := fs.ConfigMap("", nil, "remote", nil)

	out, err := Config(context.Background(), "remote", mapper, fs.ConfigIn{})
	require.NoError(t, err)
	require.Equal(t, "cookie", out.State)
	require.Equal(t, "config_cookie", out.Option.Name)

	out, err = Config(context.Background(), "remote", mapper, fs.ConfigIn{
		State:  "replace_token",
		Result: "true",
	})
	require.NoError(t, err)
	require.Equal(t, "cookie", out.State)
}
