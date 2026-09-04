package hostbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const HostConfigVersion = 1

var ErrConfigNotFound = errors.New("host broker is not configured")

type HostConfig struct {
	Version      int    `json:"version"`
	Addr         string `json:"addr"`
	Provider     string `json:"provider"`
	Executable   string `json:"executable"`
	WorkingDir   string `json:"working_dir"`
	ConfigDir    string `json:"config_dir"`
	BrowserReady bool   `json:"browser_ready"`
}

func (c HostConfig) Validate() error {
	if c.Version != HostConfigVersion || c.Provider != "claude" || !c.BrowserReady {
		return errors.New("host config requires version 1 and a readiness-gated Claude provider")
	}
	if _, err := NewClient(c.Addr); err != nil {
		return err
	}
	for _, path := range []struct {
		name      string
		value     string
		directory bool
	}{
		{name: "Claude executable", value: c.Executable},
		{name: "Claude working directory", value: c.WorkingDir, directory: true},
		{name: "Claude config directory", value: c.ConfigDir, directory: true},
	} {
		info, err := os.Stat(path.value)
		if !filepath.IsAbs(path.value) || len(path.value) > 512 || hasControl(path.value) || err != nil || info.IsDir() != path.directory {
			kind := "file"
			if path.directory {
				kind = "directory"
			}
			return fmt.Errorf("%s must be an existing absolute %s", path.name, kind)
		}
	}
	workingDir, workErr := filepath.EvalSymlinks(c.WorkingDir)
	configDir, configErr := filepath.EvalSymlinks(c.ConfigDir)
	relative, err := filepath.Rel(workingDir, configDir)
	if workErr != nil || configErr != nil || err != nil || relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("Claude config directory must be outside its working directory")
	}
	return nil
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

type ConfigStore interface {
	Load(context.Context) (HostConfig, error)
	Save(context.Context, HostConfig) error
}

type FileConfigStore struct {
	path      string
	protector Protector
}

func NewFileConfigStore(path string, protector Protector) (*FileConfigStore, error) {
	if !filepath.IsAbs(path) || protector == nil {
		return nil, errors.New("config store requires an absolute path and protector")
	}
	return &FileConfigStore{path: path, protector: protector}, nil
}

func (s *FileConfigStore) Load(context.Context) (HostConfig, error) {
	sealed, err := readRegular(s.path, 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return HostConfig{}, ErrConfigNotFound
	}
	if err != nil {
		return HostConfig{}, err
	}
	plain, err := s.protector.Open(sealed)
	if err != nil {
		return HostConfig{}, fmt.Errorf("open host config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	var config HostConfig
	if err := decoder.Decode(&config); err != nil {
		return HostConfig{}, fmt.Errorf("decode host config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return HostConfig{}, errors.New("decode host config: trailing data")
	}
	return config, config.Validate()
}

func (s *FileConfigStore) Save(_ context.Context, config HostConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}
	plain, err := json.Marshal(config)
	if err != nil {
		return err
	}
	sealed, err := s.protector.Seal(plain)
	if err != nil {
		return fmt.Errorf("seal host config: %w", err)
	}
	return writeProtected(s.path, sealed)
}
