package hostbroker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var ErrUnsupported = errors.New("host integration is supported only on Windows")

type Credential struct {
	Token           string `json:"token"`
	LauncherSession string `json:"launcher_session"`
	SessionSecret   string `json:"session_secret,omitempty"`
}

type CredentialStore interface {
	Load(context.Context) (Credential, error)
	Update(context.Context, func(*Credential) error) error
}

type FileCredentialStore struct {
	mu        sync.Mutex
	path      string
	protector Protector
}

func NewFileCredentialStore(path string, protector Protector) (*FileCredentialStore, error) {
	if !filepath.IsAbs(path) || protector == nil {
		return nil, errors.New("credential store requires an absolute path and protector")
	}
	return &FileCredentialStore{path: path, protector: protector}, nil
}

func (s *FileCredentialStore) Load(context.Context) (Credential, error) {
	sealed, err := readRegular(s.path, 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return Credential{}, ErrCredentialNotFound
	}
	if err != nil {
		return Credential{}, err
	}
	plain, err := s.protector.Open(sealed)
	if err != nil {
		return Credential{}, fmt.Errorf("open host credential: %w", err)
	}
	return decodeCredential(plain)
}

func (s *FileCredentialStore) Save(_ context.Context, credential Credential) error {
	plain, err := encodeCredential(credential)
	if err != nil {
		return err
	}
	sealed, err := s.protector.Seal(plain)
	if err != nil {
		return fmt.Errorf("seal host credential: %w", err)
	}
	return writeProtected(s.path, sealed)
}

func (s *FileCredentialStore) Update(ctx context.Context, update func(*Credential) error) error {
	if update == nil {
		return errors.New("credential update is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, err := s.Load(ctx)
	if errors.Is(err, ErrCredentialNotFound) {
		credential, err = Credential{}, nil
	}
	if err == nil {
		err = update(&credential)
	}
	if err != nil {
		return err
	}
	return s.Save(ctx, credential)
}

func encodeCredential(credential Credential) ([]byte, error) {
	if err := credential.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(credential)
}

func decodeCredential(data []byte) (Credential, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var credential Credential
	if err := decoder.Decode(&credential); err != nil {
		return Credential{}, fmt.Errorf("decode host credential: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Credential{}, errors.New("decode host credential: trailing data")
	}
	return credential, credential.Validate()
}

func (c Credential) Validate() error {
	if err := validateToken(c.Token); err != nil {
		return err
	}
	if c.LauncherSession == "" || len(c.LauncherSession) > 128 || strings.ContainsAny(c.LauncherSession, "\x00\r\n \t") {
		return errors.New("invalid launcher session identity")
	}
	if !validLowerHex(c.SessionSecret, 32) {
		return errors.New("invalid launcher session secret")
	}
	return nil
}

func Pair(ctx context.Context, store CredentialStore, source io.Reader) error {
	if store == nil || source == nil {
		return errors.New("pair requires a credential store and token input")
	}
	token, err := io.ReadAll(io.LimitReader(source, 513))
	if err != nil {
		return fmt.Errorf("read relay token: %w", err)
	}
	value := strings.TrimSpace(string(token))
	if err := validateToken(value); err != nil {
		return err
	}
	return store.Update(ctx, func(credential *Credential) error {
		if credential.LauncherSession == "" {
			credential.LauncherSession, err = newLauncherSession()
			if err != nil {
				return err
			}
		}
		if credential.SessionSecret == "" {
			credential.SessionSecret, err = newSessionSecret()
			if err != nil {
				return err
			}
		}
		credential.Token = value
		return credential.Validate()
	})
}

var ErrCredentialNotFound = errors.New("host relay is not paired")

func validateToken(token string) error {
	if !validLowerHex(token, 32) {
		return errors.New("relay token must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func validLowerHex(value string, bytes int) bool {
	if len(value) != bytes*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func newLauncherSession() (string, error) {
	value, err := randomHex(12)
	if err != nil {
		return "", fmt.Errorf("generate launcher identity: %w", err)
	}
	return "launcher-" + value, nil
}

func newSessionSecret() (string, error) {
	value, err := randomHex(32)
	if err != nil {
		return "", fmt.Errorf("generate launcher secret: %w", err)
	}
	return value, nil
}

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
