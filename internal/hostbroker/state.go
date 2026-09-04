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
	"unicode"
)

var ErrUnsupported = errors.New("host integration is supported only on Windows")

type Credential struct {
	Token           string `json:"token"`
	LauncherSession string `json:"launcher_session"`
	SessionSecret   string `json:"session_secret,omitempty"`
}

type CredentialStore interface {
	Load(context.Context) (Credential, error)
	Save(context.Context, Credential) error
}

type FileCredentialStore struct {
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
	if c.SessionSecret != "" && (len(c.SessionSecret) < 32 || len(c.SessionSecret) > 512 || strings.ContainsAny(c.SessionSecret, "\x00\r\n \t")) {
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
	credential, err := store.Load(ctx)
	if err != nil && !errors.Is(err, ErrCredentialNotFound) {
		return err
	}
	if credential.LauncherSession == "" {
		credential.LauncherSession, err = newLauncherSession()
		if err != nil {
			return err
		}
	}
	credential.Token = value
	return store.Save(ctx, credential)
}

var ErrCredentialNotFound = errors.New("host relay is not paired")

func validateToken(token string) error {
	if len(token) < 32 || len(token) > 512 {
		return errors.New("relay token must contain 32 to 512 characters")
	}
	for _, char := range token {
		if unicode.IsSpace(char) || unicode.IsControl(char) {
			return errors.New("relay token contains whitespace or control characters")
		}
	}
	return nil
}

func newLauncherSession() (string, error) {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate launcher identity: %w", err)
	}
	return "launcher-" + hex.EncodeToString(bytes), nil
}
