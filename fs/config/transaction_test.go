package config_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/stretchr/testify/require"
)

const configTransactionHelperEnv = "RCLONE_TEST_CONFIG_TRANSACTION_HELPER"

// TestFileTransactionSerializesIndependentProcesses verifies the public config
// transaction boundary. Each child writes a different section after both have
// started together; the final config must contain both changes.
func TestFileTransactionSerializesIndependentProcesses(t *testing.T) {
	if value := os.Getenv(configTransactionHelperEnv); value != "" {
		runConfigTransactionHelper(t, value)
		return
	}

	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, []byte("[one]\n[two]\n"), 0o600))

	startAt := time.Now().Add(500 * time.Millisecond).UnixNano()
	commands := []*exec.Cmd{
		configTransactionHelperCommand(configPath, startAt, "one"),
		configTransactionHelperCommand(configPath, startAt, "two"),
	}
	for _, command := range commands {
		require.NoError(t, command.Start())
	}
	for _, command := range commands {
		require.NoError(t, command.Wait())
	}

	contents, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assertConfigContains(t, string(contents), "one", "value", "one")
	assertConfigContains(t, string(contents), "two", "value", "two")
}

// TestFileTransactionPreservesPendingMutation verifies that a strong reload
// retains a change already staged by the current configuration flow.
func TestFileTransactionPreservesPendingMutation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, nil, 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	config.FileSetValue("remote", "type", "test")
	require.NoError(t, config.SetValueAndSave("remote", "token", "value"))

	remoteType, ok := config.FileGetValue("remote", "type")
	require.True(t, ok)
	require.Equal(t, "test", remoteType)
	storedToken, ok := config.FileGetValue("remote", "token")
	require.True(t, ok)
	require.Equal(t, "value", storedToken)
}

// TestFileTransactionCreatesConfigDirectory verifies that the transaction
// sidecar lock does not prevent the normal first-save path from creating a
// custom config directory.
func TestFileTransactionCreatesConfigDirectory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "new", "config", "rclone.conf")

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	err := config.FileTransaction(context.Background(), func() error {
		config.FileSetValue("remote", "type", "test")
		return nil
	})
	require.NoError(t, err)

	contents, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assertConfigContains(t, string(contents), "remote", "type", "test")
}

// TestSaveConfigSharesTransactionLock verifies that an ordinary config save
// cannot overwrite a concurrent transactional credential update.
func TestSaveConfigSharesTransactionLock(t *testing.T) {
	if value := os.Getenv(configTransactionHelperEnv); value != "" {
		runConfigTransactionHelper(t, value)
		return
	}

	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, []byte("[one]\n[two]\n"), 0o600))

	startAt := time.Now().Add(500 * time.Millisecond).UnixNano()
	commands := []*exec.Cmd{
		configTransactionHelperCommand(configPath, startAt, "one"),
		configSaveHelperCommand(configPath, startAt, "two"),
	}
	for _, command := range commands {
		require.NoError(t, command.Start())
	}
	for _, command := range commands {
		require.NoError(t, command.Wait())
	}

	contents, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assertConfigContains(t, string(contents), "one", "value", "one")
	assertConfigContains(t, string(contents), "two", "value", "two")
}

// TestFileTransactionProtectsManagedRotatingToken verifies that a local
// ordinary config edit cannot overwrite transaction metadata another process
// committed after the edit was staged.
func TestFileTransactionProtectsManagedRotatingToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntype = test\ntoken = legacy\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	// Stage a normal update while the config still holds a legacy value.
	config.FileSetValue("remote", "token", `{"refresh_token":"manual"}`)
	// A concurrent rotating-token transaction then installs state metadata.
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntype = test\ntoken = {\"access_token\":\"access\",\"refresh_token\":\"rotated\",\"rclone_token_state\":{\"version\":1,\"generation\":2,\"status\":\"ready\"}}\n"), 0o600))

	err := config.FileTransaction(context.Background(), func() error { return nil })
	require.ErrorContains(t, err, `rclone config reconnect remote:`)

	contents, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(contents), `"status":"ready"`)
	require.Contains(t, string(contents), `rotated`)
}

func configTransactionHelperCommand(configPath string, startAt int64, section string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestFileTransactionSerializesIndependentProcesses$")
	command.Env = append(os.Environ(),
		configTransactionHelperEnv+"="+section,
		"RCLONE_TEST_CONFIG_TRANSACTION_PATH="+configPath,
		"RCLONE_TEST_CONFIG_TRANSACTION_START="+strconv.FormatInt(startAt, 10),
	)
	return command
}

func configSaveHelperCommand(configPath string, startAt int64, section string) *exec.Cmd {
	command := configTransactionHelperCommand(configPath, startAt, section)
	command.Env = append(command.Env, "RCLONE_TEST_CONFIG_TRANSACTION_MODE=save")
	return command
}

func runConfigTransactionHelper(t *testing.T, section string) {
	configPath := os.Getenv("RCLONE_TEST_CONFIG_TRANSACTION_PATH")
	startAt, err := strconv.ParseInt(os.Getenv("RCLONE_TEST_CONFIG_TRANSACTION_START"), 10, 64)
	require.NoError(t, err)
	require.NoError(t, config.SetConfigPath(configPath))

	if wait := time.Until(time.Unix(0, startAt)); wait > 0 {
		time.Sleep(wait)
	}

	if os.Getenv("RCLONE_TEST_CONFIG_TRANSACTION_MODE") == "save" {
		config.FileSetValue(section, "value", section)
		// Race this ordinary config flow with a transactional writer.
		time.Sleep(200 * time.Millisecond)
		config.SaveConfig()
		return
	}
	err = config.FileTransaction(context.Background(), func() error {
		config.FileSetValue(section, "value", section)
		// Without a shared transaction lock both helpers load the same old
		// config, make their separate mutations, and overwrite one another.
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	require.NoError(t, err)
}

func assertConfigContains(t *testing.T, contents, section, key, value string) {
	t.Helper()
	sectionStart := strings.Index(contents, "["+section+"]")
	require.GreaterOrEqual(t, sectionStart, 0, "missing section %q in %s", section, contents)
	sectionContents := contents[sectionStart:]
	if next := strings.Index(sectionContents[1:], "\n["); next >= 0 {
		sectionContents = sectionContents[:next+1]
	}
	require.Contains(t, sectionContents, key+" = "+value)
}
