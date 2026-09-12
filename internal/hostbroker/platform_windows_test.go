//go:build windows

package hostbroker

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestWindowsCredentialAndDPAPISeamsDoNotCallPlatform(t *testing.T) {
	var blob []byte
	store := windowsCredentialStore{
		read: func() ([]byte, error) {
			if len(blob) == 0 {
				return nil, ErrCredentialNotFound
			}
			return append([]byte(nil), blob...), nil
		},
		write: func(value []byte) error {
			blob = append([]byte(nil), value...)
			return nil
		},
		lock: func(context.Context) (Unlock, error) { return func() error { return nil }, nil },
	}
	want := Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}
	if err := store.Update(context.Background(), func(credential *Credential) error { *credential = want; return nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(context.Background()); err != nil || got != want {
		t.Fatalf("credential round trip = (%+v, %v)", got, err)
	}

	var directions []bool
	protector := dpapiProtector{protect: func(value []byte, seal bool) ([]byte, error) {
		directions = append(directions, seal)
		return append([]byte(nil), value...), nil
	}}
	plain := []byte("journal")
	sealed, err := protector.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := protector.Open(sealed)
	if err != nil || !bytes.Equal(opened, plain) || len(directions) != 2 || !directions[0] || directions[1] {
		t.Fatalf("DPAPI seam = (%q, %v, %v)", opened, directions, err)
	}
}

// A corrupt credential must surface as a decode error, never as "unpaired" -
// reporting it as not-found would silently re-pair over a damaged secret.
func TestWindowsMalformedCredentialBlobIsRejected(t *testing.T) {
	store := windowsCredentialStore{
		read:  func() ([]byte, error) { return []byte("not json"), nil },
		write: func([]byte) error { return errors.New("write must not run") },
		lock:  func(context.Context) (Unlock, error) { return func() error { return nil }, nil },
	}
	if _, err := store.Load(context.Background()); err == nil || errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("malformed blob Load err = %v, want a decode error", err)
	}
}
