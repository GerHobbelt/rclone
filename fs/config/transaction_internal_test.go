package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type saveTrackingStorage struct {
	*defaultStorage
	saves int
}

func (s *saveTrackingStorage) Save() error {
	s.saves++
	return nil
}

// TestSaveConfigKeepsMemoryStorage verifies that ordinary config saves retain
// their original no-file behavior when rclone is explicitly memory-only.
func TestSaveConfigKeepsMemoryStorage(t *testing.T) {
	oldConfigPath := configPath
	oldData := data
	oldDataLoaded := dataLoaded
	defer func() {
		configPath = oldConfigPath
		data = oldData
		dataLoaded = oldDataLoaded
	}()

	require.NoError(t, SetConfigPath(filepath.Join(t.TempDir(), "rclone.conf")))
	storage := &saveTrackingStorage{defaultStorage: newDefaultStorage()}
	SetData(storage)
	require.NoError(t, SetConfigPath(""))

	SaveConfig()
	require.Equal(t, 1, storage.saves)

	require.NoError(t, SetValueAndSave("remote", "token", "value"))
	require.Equal(t, 2, storage.saves)
	value, found := FileGetValue("remote", "token")
	require.True(t, found)
	require.Equal(t, "value", value)
}
