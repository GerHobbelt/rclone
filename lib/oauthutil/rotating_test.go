package oauthutil

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

const rotatingTokenHelperEnv = "RCLONE_TEST_ROTATING_TOKEN_HELPER"

const rotatingTokenHelperModeEnv = "RCLONE_TEST_ROTATING_TOKEN_MODE"

// TestRotatingTokenSourceSharesRefresh verifies the token-source public seam:
// independent instances for one persisted remote consume a one-time refresh
// token once, then both adopt the persisted next generation.
func TestRotatingTokenSourceSharesRefresh(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	expired := &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Minute),
	}
	expiredJSON, err := json.Marshal(expired)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(expiredJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges atomic.Int32
	exchange := func(_ context.Context, refreshToken string) (*oauth2.Token, error) {
		require.Equal(t, "refresh-1", refreshToken)
		exchanges.Add(1)
		time.Sleep(100 * time.Millisecond)
		return &oauth2.Token{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	}

	newSource := func() *RotatingTokenSource {
		mapper := fs.ConfigMap("", nil, "remote", nil)
		source, err := NewRotatingTokenSource(context.Background(), "remote", mapper, exchange)
		require.NoError(t, err)
		return source
	}
	sourceA := newSource()
	sourceB := newSource()

	type result struct {
		token      *oauth2.Token
		generation uint64
		err        error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, source := range []*RotatingTokenSource{sourceA, sourceB} {
		wg.Add(1)
		go func(source *RotatingTokenSource) {
			defer wg.Done()
			<-start
			token, generation, err := source.TokenContext(context.Background())
			results <- result{token: token, generation: generation, err: err}
		}(source)
	}
	close(start)
	wg.Wait()
	close(results)

	for result := range results {
		require.NoError(t, result.err)
		require.Equal(t, "access-2", result.token.AccessToken)
		require.Equal(t, uint64(2), result.generation)
	}
	require.Equal(t, int32(1), exchanges.Load())

	storedJSON, ok := config.FileGetValue("remote", "token")
	require.True(t, ok)
	var stored struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		State        struct {
			Generation uint64 `json:"generation"`
			Status     string `json:"status"`
		} `json:"rclone_token_state"`
	}
	require.NoError(t, json.Unmarshal([]byte(storedJSON), &stored))
	require.Equal(t, "access-2", stored.AccessToken)
	require.Equal(t, "refresh-2", stored.RefreshToken)
	require.Equal(t, uint64(2), stored.State.Generation)
	require.Equal(t, "ready", stored.State.Status)
}

// TestRotatingTokenSourcePersistsUncertainExchange verifies that an ambiguous
// broker failure is fail-closed and cannot trigger a second exchange.
func TestRotatingTokenSourcePersistsUncertainExchange(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	expired := &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Minute),
	}
	expiredJSON, err := json.Marshal(expired)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(expiredJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges atomic.Int32
	exchange := func(context.Context, string) (*oauth2.Token, error) {
		exchanges.Add(1)
		return nil, errors.New("broker response lost")
	}
	source, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), exchange)
	require.NoError(t, err)

	_, _, err = source.TokenContext(context.Background())
	require.Error(t, err)
	persisted, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(persisted), `"status":"uncertain"`)

	_, _, err = source.TokenContext(context.Background())
	require.ErrorIs(t, err, ErrTokenStateUncertain)
	require.Equal(t, int32(1), exchanges.Load())
}

// TestRotatingTokenSourceRetriesOnlyProvenUnsentExchange verifies that a
// transport failure proven not to send the request restores the ready state.
func TestRotatingTokenSourceRetriesOnlyProvenUnsentExchange(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	expired := &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Minute),
	}
	expiredJSON, err := json.Marshal(expired)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(expiredJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges atomic.Int32
	exchange := func(context.Context, string) (*oauth2.Token, error) {
		if exchanges.Add(1) == 1 {
			return nil, NewExchangeError(ExchangeFailureNotSent, errors.New("connection refused"))
		}
		return &oauth2.Token{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	}
	source, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), exchange)
	require.NoError(t, err)

	_, _, err = source.TokenContext(context.Background())
	require.Error(t, err)
	persisted, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(persisted), `"status":"ready"`)
	require.Contains(t, string(persisted), `"generation":1`)

	token, generation, err := source.TokenContext(context.Background())
	require.NoError(t, err)
	require.Equal(t, "access-2", token.AccessToken)
	require.Equal(t, uint64(2), generation)
	require.Equal(t, int32(2), exchanges.Load())
}

// TestClassifyExchangeTransportErrorOnlyMarksPreflightFailuresRetryable
// verifies that the production exchange adapters restore ready state only for
// transport failures which occur before an HTTP request can be sent.
func TestClassifyExchangeTransportErrorOnlyMarksPreflightFailuresRetryable(t *testing.T) {
	for _, err := range []error{
		&url.Error{Op: "Get", URL: "https://broker.example", Err: &net.DNSError{Name: "broker.example", IsNotFound: true}},
		&url.Error{Op: "Get", URL: "https://broker.example", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
	} {
		classified := ClassifyExchangeTransportError(err)
		var exchangeErr ExchangeError
		require.ErrorAs(t, classified, &exchangeErr)
		require.Equal(t, ExchangeFailureNotSent, exchangeErr.ExchangeFailure())
	}

	ambiguous := errors.New("unexpected EOF")
	require.ErrorIs(t, ClassifyExchangeTransportError(ambiguous), ambiguous)
}

// TestRotatingTokenSourceSharesRefreshAcrossProcesses verifies that the
// sidecar token lock serializes two independent rclone processes.
func TestRotatingTokenSourceSharesRefreshAcrossProcesses(t *testing.T) {
	if os.Getenv(rotatingTokenHelperEnv) != "" {
		runRotatingTokenHelper(t)
		return
	}

	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	expired := &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Minute),
	}
	expiredJSON, err := json.Marshal(expired)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(expiredJSON)+"\n"), 0o600))

	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "refresh-1", r.URL.Query().Get("refresh_token"))
		exchanges.Add(1)
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(&oauth2.Token{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			Expiry:       time.Now().Add(time.Hour),
		})
	}))
	defer server.Close()

	startAt := time.Now().Add(500 * time.Millisecond).UnixNano()
	commands := []*exec.Cmd{
		rotatingTokenHelperCommand(configPath, server.URL, startAt),
		rotatingTokenHelperCommand(configPath, server.URL, startAt),
	}
	for _, command := range commands {
		require.NoError(t, command.Start())
	}
	for _, command := range commands {
		require.NoError(t, command.Wait())
	}
	require.Equal(t, int32(1), exchanges.Load())

	persisted, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(persisted), `"access_token":"access-2"`)
	require.Contains(t, string(persisted), `"refresh_token":"refresh-2"`)
	require.Contains(t, string(persisted), `"generation":2`)
}

// TestRotatingTokenSourceMigratesLegacyTokenAcrossProcesses verifies that two
// processes racing to adopt a legacy credential create one ready generation
// without contacting a token broker.
func TestRotatingTokenSourceMigratesLegacyTokenAcrossProcesses(t *testing.T) {
	if os.Getenv(rotatingTokenHelperEnv) != "" {
		runRotatingTokenHelper(t)
		return
	}

	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	legacy, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(legacy)+"\n"), 0o600))

	startAt := time.Now().Add(500 * time.Millisecond).UnixNano()
	commands := []*exec.Cmd{
		rotatingTokenMigrationHelperCommand(configPath, startAt),
		rotatingTokenMigrationHelperCommand(configPath, startAt),
	}
	for _, command := range commands {
		require.NoError(t, command.Start())
	}
	for _, command := range commands {
		require.NoError(t, command.Wait())
	}

	persisted, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(persisted), `"access_token":"access-1"`)
	require.Contains(t, string(persisted), `"refresh_token":"refresh-1"`)
	require.Contains(t, string(persisted), `"generation":1`)
	require.Contains(t, string(persisted), `"status":"ready"`)
}

// TestRotatingTokenSourceDoesNotRefreshAfterLateFailure verifies that a late
// authorization failure for generation 1 adopts generation 2 instead of
// consuming the already-rotated refresh token again.
func TestRotatingTokenSourceDoesNotRefreshAfterLateFailure(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	initial := &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	}
	initialJSON, err := json.Marshal(initial)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(initialJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges atomic.Int32
	exchange := func(context.Context, string) (*oauth2.Token, error) {
		exchanges.Add(1)
		return &oauth2.Token{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	}
	sourceA, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), exchange)
	require.NoError(t, err)
	sourceB, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), exchange)
	require.NoError(t, err)

	_, generation, err := sourceA.TokenContext(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(1), generation)
	_, generation, err = sourceA.RefreshIfCurrent(context.Background(), generation)
	require.NoError(t, err)
	require.Equal(t, uint64(2), generation)

	token, generation, err := sourceB.RefreshIfCurrent(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, "access-2", token.AccessToken)
	require.Equal(t, uint64(2), generation)
	require.Equal(t, int32(1), exchanges.Load())
}

// TestRotatingTokenSourceBootstrapsRefreshOnlyCredential verifies that an
// initial persistent refresh token can be exchanged transactionally without an
// access token already present in config.
func TestRotatingTokenSourceBootstrapsRefreshOnlyCredential(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = {\"refresh_token\":\"refresh-1\"}\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges atomic.Int32
	source, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), func(_ context.Context, refreshToken string) (*oauth2.Token, error) {
		require.Equal(t, "refresh-1", refreshToken)
		exchanges.Add(1)
		return &oauth2.Token{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	})
	require.NoError(t, err)

	token, generation, err := source.TokenContext(context.Background())
	require.NoError(t, err)
	require.Equal(t, "access-2", token.AccessToken)
	require.Equal(t, uint64(2), generation)
	require.Equal(t, int32(1), exchanges.Load())
}

// TestPutRotatingTokenReplacesTerminalState verifies that an explicit
// reconnect can replace an uncertain credential and make its fresh refresh
// token available to a new source.
func TestPutRotatingTokenReplacesTerminalState(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	stored, err := json.Marshal(storedRotatingToken{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Hour),
		State: &rotatingTokenState{
			Version:    rotatingTokenStateVersion,
			Generation: 7,
			Status:     tokenStateUncertain,
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(stored)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	mapper := fs.ConfigMap("", nil, "remote", nil)
	require.NoError(t, PutRotatingToken(context.Background(), "remote", mapper, &oauth2.Token{
		AccessToken:  "access-8",
		RefreshToken: "refresh-8",
		Expiry:       time.Now().Add(time.Hour),
	}))

	storedJSON, ok := config.FileGetValue("remote", config.ConfigToken)
	require.True(t, ok)
	require.Contains(t, storedJSON, `"access_token":"access-8"`)
	require.Contains(t, storedJSON, `"refresh_token":"refresh-8"`)
	require.Contains(t, storedJSON, `"status":"ready"`)
	require.Contains(t, storedJSON, `"generation":8`)

	source, err := NewRotatingTokenSource(context.Background(), "remote", mapper, func(context.Context, string) (*oauth2.Token, error) {
		return nil, errors.New("must not exchange a complete replacement token")
	})
	require.NoError(t, err)
	token, generation, err := source.TokenContext(context.Background())
	require.NoError(t, err)
	require.Equal(t, "access-8", token.AccessToken)
	require.Equal(t, uint64(8), generation)
}

// TestPutRotatingTokenRejectsIncompleteCredential verifies that no public
// writer can mark a refresh-token-only value ready.
func TestPutRotatingTokenRejectsIncompleteCredential(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	err := PutRotatingToken(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), &oauth2.Token{RefreshToken: "refresh-only"})
	require.ErrorContains(t, err, "complete token")
}

// TestReauthenticateRotatingTokenHoldsSectionLock verifies that a device-login
// callback itself is serialized with the same lock as runtime token refreshes.
func TestReauthenticateRotatingTokenHoldsSectionLock(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	stored, err := json.Marshal(storedRotatingToken{
		AccessToken:  "access-7",
		RefreshToken: "refresh-7",
		Expiry:       time.Now().Add(time.Hour),
		State: &rotatingTokenState{
			Version:    rotatingTokenStateVersion,
			Generation: 7,
			Status:     tokenStateUncertain,
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(stored)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	started := make(chan struct{})
	release := make(chan struct{})
	secondStarted := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	mapper := fs.ConfigMap("", nil, "remote", nil)

	go func() {
		_, _, err := ReauthenticateRotatingToken(context.Background(), "remote", mapper, func(context.Context) (*oauth2.Token, error) {
			close(started)
			<-release
			return &oauth2.Token{
				AccessToken:  "access-8",
				RefreshToken: "refresh-8",
				Expiry:       time.Now().Add(time.Hour),
			}, nil
		})
		firstDone <- err
	}()
	<-started

	go func() {
		_, _, err := ReauthenticateRotatingToken(context.Background(), "remote", mapper, func(context.Context) (*oauth2.Token, error) {
			close(secondStarted)
			return &oauth2.Token{
				AccessToken:  "access-9",
				RefreshToken: "refresh-9",
				Expiry:       time.Now().Add(time.Hour),
			}, nil
		})
		secondDone <- err
	}()

	select {
	case <-secondStarted:
		t.Fatal("second device authorization started before the first released the token lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)

	persisted, state, legacy, err := loadStoredToken("remote")
	require.NoError(t, err)
	require.False(t, legacy)
	require.Equal(t, "access-9", persisted.AccessToken)
	require.Equal(t, uint64(9), state.Generation)
	require.Equal(t, tokenStateReady, state.Status)
}

// TestReauthenticateRotatingTokenLeavesStateOnFailure verifies that a failed
// device authorization never changes the current terminal credential state.
func TestReauthenticateRotatingTokenLeavesStateOnFailure(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	stored, err := json.Marshal(storedRotatingToken{
		AccessToken:  "access-7",
		RefreshToken: "refresh-7",
		Expiry:       time.Now().Add(-time.Hour),
		State: &rotatingTokenState{
			Version:    rotatingTokenStateVersion,
			Generation: 7,
			Status:     tokenStateUncertain,
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(stored)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	_, _, err = ReauthenticateRotatingToken(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), func(context.Context) (*oauth2.Token, error) {
		return nil, errors.New("device authorization rejected")
	})
	require.ErrorContains(t, err, "device authorization rejected")

	persisted, state, legacy, err := loadStoredToken("remote")
	require.NoError(t, err)
	require.False(t, legacy)
	require.Equal(t, "access-7", persisted.AccessToken)
	require.Equal(t, "refresh-7", persisted.RefreshToken)
	require.Equal(t, uint64(7), state.Generation)
	require.Equal(t, tokenStateUncertain, state.Status)
}

// TestStoredRotatingTokenRemainsOAuthCompatible verifies that standard OAuth
// decoders retain the credential fields while ignoring transaction metadata.
func TestStoredRotatingTokenRemainsOAuthCompatible(t *testing.T) {
	stored, err := json.Marshal(storedRotatingToken{
		AccessToken:  "access-1",
		TokenType:    "Bearer",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().UTC().Round(0),
		State: &rotatingTokenState{
			Version:    rotatingTokenStateVersion,
			Generation: 7,
			Status:     tokenStateReady,
		},
	})
	require.NoError(t, err)

	var token oauth2.Token
	require.NoError(t, json.Unmarshal(stored, &token))
	require.Equal(t, "access-1", token.AccessToken)
	require.Equal(t, "Bearer", token.TokenType)
	require.Equal(t, "refresh-1", token.RefreshToken)
	require.False(t, token.Expiry.IsZero())
}

// TestReconnectRotatingTokenExchangesFreshCredential verifies that explicit
// recovery exchanges the user-supplied refresh token before it replaces a
// terminal persisted state.
func TestReconnectRotatingTokenExchangesFreshCredential(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	stored, err := json.Marshal(storedRotatingToken{
		AccessToken:  "access-7",
		RefreshToken: "refresh-7",
		Expiry:       time.Now().Add(-time.Hour),
		State: &rotatingTokenState{
			Version:    rotatingTokenStateVersion,
			Generation: 7,
			Status:     tokenStateUncertain,
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(stored)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges atomic.Int32
	token, generation, err := ReconnectRotatingToken(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), &oauth2.Token{
		RefreshToken: "refresh-8",
	}, func(_ context.Context, refreshToken string) (*oauth2.Token, error) {
		require.Equal(t, "refresh-8", refreshToken)
		exchanges.Add(1)
		return &oauth2.Token{
			AccessToken:  "access-8",
			RefreshToken: "refresh-9",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	})
	require.NoError(t, err)
	require.Equal(t, "access-8", token.AccessToken)
	require.Equal(t, uint64(8), generation)
	require.Equal(t, int32(1), exchanges.Load())

	persistedToken, state, legacy, err := loadStoredToken("remote")
	require.NoError(t, err)
	require.False(t, legacy)
	require.Equal(t, "access-8", persistedToken.AccessToken)
	require.Equal(t, "refresh-9", persistedToken.RefreshToken)
	require.Equal(t, tokenStateReady, state.Status)
	require.Equal(t, uint64(8), state.Generation)
}

// TestReconnectRotatingTokenPreservesTerminalStateOnRejectedCredential
// verifies that a rejected user-supplied credential does not overwrite an
// existing terminal state.
func TestReconnectRotatingTokenPreservesTerminalStateOnRejectedCredential(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	stored, err := json.Marshal(storedRotatingToken{
		AccessToken:  "access-7",
		RefreshToken: "refresh-7",
		Expiry:       time.Now().Add(-time.Hour),
		State: &rotatingTokenState{
			Version:    rotatingTokenStateVersion,
			Generation: 7,
			Status:     tokenStateUncertain,
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(stored)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	_, _, err = ReconnectRotatingToken(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), &oauth2.Token{
		RefreshToken: "refresh-8",
	}, func(context.Context, string) (*oauth2.Token, error) {
		return nil, NewExchangeError(ExchangeFailureReauthenticationRequired, errors.New("refresh token rejected"))
	})
	require.Error(t, err)

	persistedToken, state, legacy, err := loadStoredToken("remote")
	require.NoError(t, err)
	require.False(t, legacy)
	require.Equal(t, "refresh-7", persistedToken.RefreshToken)
	require.Equal(t, tokenStateUncertain, state.Status)
	require.Equal(t, uint64(7), state.Generation)
}

// TestRotatingTokenSourceFailsClosedForOrphanedInFlight verifies that a state
// left behind after a process exits during an exchange is never retried.
func TestRotatingTokenSourceFailsClosedForOrphanedInFlight(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	stored, err := json.Marshal(storedRotatingToken{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Hour),
		State: &rotatingTokenState{
			Version:    rotatingTokenStateVersion,
			Generation: 1,
			Status:     tokenStateInFlight,
			Owner:      "dead-process",
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(stored)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges atomic.Int32
	source, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), func(context.Context, string) (*oauth2.Token, error) {
		exchanges.Add(1)
		return nil, errors.New("must not exchange")
	})
	require.NoError(t, err)
	_, state, legacy, err := loadStoredToken("remote")
	require.NoError(t, err)
	require.False(t, legacy)
	require.Equal(t, tokenStateUncertain, state.Status)
	_, _, err = source.TokenContext(context.Background())
	require.ErrorIs(t, err, ErrTokenStateUncertain)
	require.Equal(t, int32(0), exchanges.Load())

	_, state, legacy, err = loadStoredToken("remote")
	require.NoError(t, err)
	require.False(t, legacy)
	require.Equal(t, tokenStateUncertain, state.Status)
}

// TestRotatingTokenSourceMarksCurrentGenerationReauthenticationRequired
// verifies that a provider's definitive refresh-token rejection commits a
// terminal state without sending the stored refresh token again.
func TestRotatingTokenSourceMarksCurrentGenerationReauthenticationRequired(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	tokenJSON, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(tokenJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges atomic.Int32
	source, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), func(context.Context, string) (*oauth2.Token, error) {
		exchanges.Add(1)
		return nil, errors.New("must not exchange")
	})
	require.NoError(t, err)
	_, generation, err := source.TokenContext(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(1), generation)

	_, _, err = source.MarkReauthenticationRequiredIfCurrent(context.Background(), generation)
	require.ErrorIs(t, err, ErrTokenReauthenticationRequired)
	require.Equal(t, int32(0), exchanges.Load())

	_, state, legacy, err := loadStoredToken("remote")
	require.NoError(t, err)
	require.False(t, legacy)
	require.Equal(t, tokenStateReauthenticationRequired, state.Status)
}

// TestRotatingTokenSourcePersistsReauthenticationRequired verifies that a
// provider's definitive refresh-token rejection terminates automatic refresh.
func TestRotatingTokenSourcePersistsReauthenticationRequired(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	expiredJSON, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(expiredJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	var exchanges atomic.Int32
	source, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), func(context.Context, string) (*oauth2.Token, error) {
		exchanges.Add(1)
		return nil, NewExchangeError(ExchangeFailureReauthenticationRequired, errors.New("refresh token rejected"))
	})
	require.NoError(t, err)

	_, _, err = source.TokenContext(context.Background())
	require.Error(t, err)
	_, _, err = source.TokenContext(context.Background())
	require.ErrorIs(t, err, ErrTokenReauthenticationRequired)
	require.Equal(t, int32(1), exchanges.Load())

	_, state, legacy, err := loadStoredToken("remote")
	require.NoError(t, err)
	require.False(t, legacy)
	require.Equal(t, tokenStateReauthenticationRequired, state.Status)
}

// TestRotatingTokenSourceReconnectHintUsesPersistentSection verifies that an
// overridden runtime name never leaks into the recovery command.
func TestRotatingTokenSourceReconnectHintUsesPersistentSection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	stored, err := json.Marshal(storedRotatingToken{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Hour),
		State: &rotatingTokenState{
			Version:    rotatingTokenStateVersion,
			Generation: 1,
			Status:     tokenStateUncertain,
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[abc]\ntoken = "+string(stored)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	source, err := NewRotatingTokenSource(context.Background(), "abc{A1fie}", fs.ConfigMap("", nil, "abc", nil), func(context.Context, string) (*oauth2.Token, error) {
		return nil, errors.New("must not exchange")
	})
	require.NoError(t, err)
	_, _, err = source.TokenContext(context.Background())
	require.ErrorIs(t, err, ErrTokenStateUncertain)
	require.ErrorContains(t, err, "rclone config reconnect abc:")
	require.NotContains(t, err.Error(), "abc{A1fie}")
}

// TestRotatingTokenSourcePersistsUncertainAfterCancelledExchange verifies that
// cancellation after a broker call has begun still records the fail-closed
// outcome before returning to the caller.
func TestRotatingTokenSourcePersistsUncertainAfterCancelledExchange(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	expiredJSON, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(expiredJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source, err := NewRotatingTokenSource(ctx, "remote", fs.ConfigMap("", nil, "remote", nil), func(context.Context, string) (*oauth2.Token, error) {
		cancel()
		return nil, context.Canceled
	})
	require.NoError(t, err)
	_, _, err = source.TokenContext(ctx)
	require.ErrorIs(t, err, context.Canceled)

	_, state, legacy, err := loadStoredToken("remote")
	require.NoError(t, err)
	require.False(t, legacy)
	require.Equal(t, tokenStateUncertain, state.Status)
}

// TestRotatingTokenSourceRejectsTokenOverrides verifies that a higher-priority
// value cannot be mistaken for the persistent rotating credential.
func TestRotatingTokenSourceRejectsTokenOverrides(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	tokenJSON, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-file",
		RefreshToken: "refresh-file",
		Expiry:       time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(tokenJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	mapper := fs.ConfigMap("", nil, "remote", configmap.Simple{config.ConfigToken: "override"})
	_, err = NewRotatingTokenSource(context.Background(), "remote", mapper, func(context.Context, string) (*oauth2.Token, error) {
		return nil, errors.New("must not exchange")
	})
	require.ErrorContains(t, err, "must be stored in the local config")

	persisted, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(persisted), "refresh-file")
	require.NotContains(t, string(persisted), "override")
}

// TestRotatingRenewRefreshesDuringUpload verifies that the long-operation
// renewer uses the same durable source as ordinary requests.
func TestRotatingRenewRefreshesDuringUpload(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	expired := &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Minute),
	}
	expiredJSON, err := json.Marshal(expired)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(expiredJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	refreshed := make(chan struct{}, 1)
	source, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), func(context.Context, string) (*oauth2.Token, error) {
		refreshed <- struct{}{}
		return &oauth2.Token{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	})
	require.NoError(t, err)
	renewer := NewRotatingRenew(context.Background(), "remote", source)
	defer renewer.Shutdown()
	renewer.Start()

	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rotating renewer")
	}
}

// TestRotatingRenewShutdownWaitsForExchange verifies that shutdown cancels an
// active renewal and does not leave a background config reader behind.
func TestRotatingRenewShutdownWaitsForExchange(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	expired, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(expired)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchangeStarted := make(chan struct{})
	exchangeStopped := make(chan struct{})
	source, err := NewRotatingTokenSource(baseCtx, "remote", fs.ConfigMap("", nil, "remote", nil), func(ctx context.Context, _ string) (*oauth2.Token, error) {
		close(exchangeStarted)
		<-ctx.Done()
		close(exchangeStopped)
		return nil, ctx.Err()
	})
	require.NoError(t, err)

	renewer := NewRotatingRenew(baseCtx, "remote", source)
	renewer.Start()
	select {
	case <-exchangeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rotating renewer exchange")
	}
	renewer.Shutdown()
	select {
	case <-exchangeStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("rotating renewer shutdown returned before its exchange stopped")
	}
}

func rotatingTokenHelperCommand(configPath, serverURL string, startAt int64) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestRotatingTokenSourceSharesRefreshAcrossProcesses$")
	command.Env = append(os.Environ(),
		rotatingTokenHelperEnv+"=1",
		"RCLONE_TEST_ROTATING_TOKEN_PATH="+configPath,
		"RCLONE_TEST_ROTATING_TOKEN_SERVER="+serverURL,
		"RCLONE_TEST_ROTATING_TOKEN_START="+strconv.FormatInt(startAt, 10),
	)
	return command
}

func rotatingTokenMigrationHelperCommand(configPath string, startAt int64) *exec.Cmd {
	command := rotatingTokenHelperCommand(configPath, "", startAt)
	command.Env = append(command.Env, rotatingTokenHelperModeEnv+"=migrate")
	return command
}

func runRotatingTokenHelper(t *testing.T) {
	configPath := os.Getenv("RCLONE_TEST_ROTATING_TOKEN_PATH")
	serverURL := os.Getenv("RCLONE_TEST_ROTATING_TOKEN_SERVER")
	mode := os.Getenv(rotatingTokenHelperModeEnv)
	startAt, err := strconv.ParseInt(os.Getenv("RCLONE_TEST_ROTATING_TOKEN_START"), 10, 64)
	require.NoError(t, err)
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()

	if wait := time.Until(time.Unix(0, startAt)); wait > 0 {
		time.Sleep(wait)
	}
	exchange := func(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
		if mode == "migrate" {
			return nil, errors.New("legacy migration must not exchange a token")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"?refresh_token="+refreshToken, nil)
		if err != nil {
			return nil, NewExchangeError(ExchangeFailureNotSent, err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return nil, errors.New(response.Status)
		}
		var token oauth2.Token
		if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
			return nil, err
		}
		return &token, nil
	}

	source, err := NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), exchange)
	require.NoError(t, err)
	token, generation, err := source.TokenContext(context.Background())
	require.NoError(t, err)
	if mode == "migrate" {
		require.Equal(t, "access-1", token.AccessToken)
		require.Equal(t, uint64(1), generation)
		return
	}
	require.Equal(t, "access-2", token.AccessToken)
	require.Equal(t, uint64(2), generation)
}
