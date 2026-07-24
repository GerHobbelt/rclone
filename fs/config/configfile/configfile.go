// Package configfile implements a config file loader and saver
package configfile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/lib/file"
	"github.com/unknwon/goconfig" //nolint:misspell // Don't include misspell when running golangci-lint
)

// Install installs the config file handler
func Install() {
	config.SetData(&Storage{})
}

// Storage implements config.Storage for saving and loading config
// data in a simple INI based file.
type Storage struct {
	mu              sync.Mutex           // to protect the following variables
	gc              *goconfig.ConfigFile // config file loaded - not thread safe
	fi              os.FileInfo          // stat of the file when last loaded
	dirtyValues     map[string]map[string]*string
	deletedSections map[string]struct{}
}

// Check to see if we need to reload the config
//
// mu must be held when calling this
func (s *Storage) _check() {
	if configPath := config.GetConfigPath(); configPath != "" {
		// Check to see if config file has changed since it was last loaded
		fi, err := os.Stat(configPath)
		if err == nil {
			// check to see if config file has changed and if it has, reload it
			if s.fi == nil || !fi.ModTime().Equal(s.fi.ModTime()) || fi.Size() != s.fi.Size() {
				fs.Debugf(nil, "Config file has changed externally - reloading")
				err := s._load()
				if err != nil {
					fs.Errorf(nil, "Failed to read config file - using previous config: %v", err)
				}
			}
		}
	}
}

// _load the config from permanent storage, decrypting if necessary
//
// mu must be held when calling this
func (s *Storage) _load() (err error) {
	// Make sure we have a sensible default even when we error
	defer func() {
		if s.gc == nil {
			s.gc, _ = goconfig.LoadFromReader(bytes.NewReader([]byte{}))
			s.applyDirty()
		}
	}()

	configPath := config.GetConfigPath()
	if configPath == "" {
		return config.ErrorConfigFileNotFound
	}

	fd, err := os.Open(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return config.ErrorConfigFileNotFound
		}
		return err
	}
	defer fs.CheckClose(fd, &err)

	// Update s.fi with the current file info
	s.fi, _ = os.Stat(configPath)

	cryptReader, err := config.Decrypt(fd)
	if err != nil {
		return err
	}

	gc, err := goconfig.LoadFromReader(cryptReader)
	if err != nil {
		return err
	}
	if err := s.rejectManagedTokenOverwrite(gc); err != nil {
		return err
	}
	s.gc = gc
	s.applyDirty()

	return nil
}

// Load the config from permanent storage, decrypting if necessary
func (s *Storage) Load() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s._load()
}

// Save the config to permanent storage, encrypting if necessary
func (s *Storage) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	configPath := config.GetConfigPath()
	if configPath == "" {
		return fmt.Errorf("failed to save config file, path is empty")
	}
	configDir, configName := filepath.Split(configPath)

	info, err := os.Lstat(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to resolve config file path: %w", err)
		}
	} else {
		if info.Mode()&os.ModeSymlink != 0 {
			configPath, err = os.Readlink(configPath)
			if err != nil {
				return fmt.Errorf("failed to resolve config file symbolic link: %w", err)
			}
			if !filepath.IsAbs(configPath) {
				configPath = filepath.Join(configDir, configPath)
			}
			configDir = filepath.Dir(configPath)
		}
	}
	err = file.MkdirAll(configDir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	f, err := os.CreateTemp(configDir, configName)
	if err != nil {
		return fmt.Errorf("failed to create temp file for new config: %w", err)
	}
	defer func() {
		_ = f.Close()
		if err := os.Remove(f.Name()); err != nil && !os.IsNotExist(err) {
			fs.Errorf(nil, "Failed to remove temp file for new config: %v", err)
		}
	}()

	var buf bytes.Buffer
	if err := goconfig.SaveConfigData(s.gc, &buf); err != nil {
		return fmt.Errorf("failed to save config file: %w", err)
	}

	if err := config.Encrypt(&buf, f); err != nil {
		return err
	}

	_ = f.Sync()
	err = f.Close()
	if err != nil {
		return fmt.Errorf("failed to close config file: %w", err)
	}

	var fileMode os.FileMode = 0600
	info, err = os.Stat(configPath)
	if err != nil {
		fs.Debugf(nil, "Using default permissions for config file: %v", fileMode)
	} else if info.Mode() != fileMode {
		fs.Debugf(nil, "Keeping previous permissions for config file: %v", info.Mode())
		fileMode = info.Mode()
	}

	attemptCopyGroup(configPath, f.Name())

	err = os.Chmod(f.Name(), fileMode)
	if err != nil {
		fs.Errorf(nil, "Failed to set permissions on config file: %v", err)
	}

	fbackup, err := os.CreateTemp(configDir, configName+".old")
	if err != nil {
		return fmt.Errorf("failed to create temp file for old config backup: %w", err)
	}
	err = fbackup.Close()
	if err != nil {
		return fmt.Errorf("failed to close temp file for old config backup: %w", err)
	}
	keepBackup := true
	defer func() {
		if !keepBackup {
			if err := os.Remove(fbackup.Name()); err != nil && !os.IsNotExist(err) {
				fs.Errorf(nil, "Failed to remove temp file for old config backup: %v", err)
			}
		}
	}()

	if err = os.Rename(configPath, fbackup.Name()); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to move previous config to backup location: %w", err)
		}
		keepBackup = false // no existing file, no need to keep backup even if writing of new file fails
	}
	if err = os.Rename(f.Name(), configPath); err != nil {
		return fmt.Errorf("failed to move newly written config from %s to final location: %v", f.Name(), err)
	}
	keepBackup = false // new file was written, no need to keep backup

	// Update s.fi with the newly written file
	s.fi, _ = os.Stat(configPath)
	s.clearDirty()

	return nil
}

// Serialize the config into a string
func (s *Storage) Serialize() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s._check()
	var buf bytes.Buffer
	if err := goconfig.SaveConfigData(s.gc, &buf); err != nil {
		return "", fmt.Errorf("failed to save config file: %w", err)
	}

	return buf.String(), nil
}

// HasSection returns true if section exists in the config file
func (s *Storage) HasSection(section string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s._check()
	_, err := s.gc.GetSection(section)
	return err == nil
}

// DeleteSection removes the named section and all config from the
// config file
func (s *Storage) DeleteSection(section string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s._check()
	s.gc.DeleteSection(section)
	if s.deletedSections == nil {
		s.deletedSections = make(map[string]struct{})
	}
	s.deletedSections[section] = struct{}{}
	delete(s.dirtyValues, section)
}

// GetSectionList returns a slice of strings with names for all the
// sections
func (s *Storage) GetSectionList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s._check()
	return s.gc.GetSectionList()
}

// GetKeyList returns the keys in this section
func (s *Storage) GetKeyList(section string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s._check()
	return s.gc.GetKeyList(section)
}

// GetValue returns the key in section with a found flag
func (s *Storage) GetValue(section string, key string) (value string, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s._check()
	value, err := s.gc.GetValue(section, key)
	if err != nil {
		return "", false
	}
	return value, true
}

// SetValue sets the value under key in section
func (s *Storage) SetValue(section string, key string, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s._check()
	if strings.HasPrefix(section, ":") {
		fs.Logf(nil, "Can't save config %q for on the fly backend %q", key, section)
		return
	}
	s.gc.SetValue(section, key, value)
	s.markSet(section, key, value)
}

// DeleteKey removes the key under section
func (s *Storage) DeleteKey(section string, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s._check()
	deleted := s.gc.DeleteKey(section, key)
	if deleted {
		s.markDelete(section, key)
	}
	return deleted
}

// markSet records a mutation which must survive a strong reload before Save.
//
// s.mu must be held when calling this.
func (s *Storage) markSet(section, key, value string) {
	if s.dirtyValues == nil {
		s.dirtyValues = make(map[string]map[string]*string)
	}
	values := s.dirtyValues[section]
	if values == nil {
		values = make(map[string]*string)
		s.dirtyValues[section] = values
	}
	valueCopy := value
	values[key] = &valueCopy
}

// markDelete records a key removal which must survive a strong reload before
// Save.
//
// s.mu must be held when calling this.
func (s *Storage) markDelete(section, key string) {
	if s.dirtyValues == nil {
		s.dirtyValues = make(map[string]map[string]*string)
	}
	values := s.dirtyValues[section]
	if values == nil {
		values = make(map[string]*string)
		s.dirtyValues[section] = values
	}
	values[key] = nil
}

// rejectManagedTokenOverwrite rejects a pending ordinary token write when the
// durable config has acquired rotating-token transaction metadata meanwhile.
//
// s.mu must be held when calling this.
func (s *Storage) rejectManagedTokenOverwrite(gc *goconfig.ConfigFile) error {
	for section, values := range s.dirtyValues {
		next, changed := values[config.ConfigToken]
		if !changed {
			continue
		}
		current, err := gc.GetValue(section, config.ConfigToken)
		if err != nil || !config.IsManagedRotatingToken(current) {
			continue
		}
		if next != nil && *next == current {
			continue
		}
		return fmt.Errorf("rotating token for %q is managed by its backend; run \"rclone config reconnect %s:\"", section, section)
	}
	return nil
}

// applyDirty merges pending mutations into the most recently loaded config.
//
// s.mu must be held when calling this.
func (s *Storage) applyDirty() {
	if s.gc == nil {
		return
	}
	for section := range s.deletedSections {
		s.gc.DeleteSection(section)
	}
	for section, values := range s.dirtyValues {
		for key, value := range values {
			if value == nil {
				s.gc.DeleteKey(section, key)
				continue
			}
			s.gc.SetValue(section, key, *value)
		}
	}
}

// clearDirty clears mutations after they have been durably saved.
//
// s.mu must be held when calling this.
func (s *Storage) clearDirty() {
	s.dirtyValues = nil
	s.deletedSections = nil
}

// Check the interface is satisfied
var _ config.Storage = (*Storage)(nil)
