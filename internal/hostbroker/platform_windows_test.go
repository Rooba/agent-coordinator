//go:build windows

package hostbroker

import (
	"bytes"
	"context"
	"testing"
)

func TestWindowsCredentialAndDPAPISeamsDoNotCallPlatform(t *testing.T) {
	var blob []byte
	store := windowsCredentialStore{
		read: func() ([]byte, error) { return append([]byte(nil), blob...), nil },
		write: func(value []byte) error {
			blob = append([]byte(nil), value...)
			return nil
		},
	}
	want := Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}
	if err := store.Save(context.Background(), want); err != nil {
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
