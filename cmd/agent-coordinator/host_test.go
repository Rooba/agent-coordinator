package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/hostbroker"
)

type hostTestCredentials struct{ saved hostbroker.Credential }

func (*hostTestCredentials) Load(context.Context) (hostbroker.Credential, error) {
	return hostbroker.Credential{}, hostbroker.ErrCredentialNotFound
}

func (s *hostTestCredentials) Save(_ context.Context, credential hostbroker.Credential) error {
	s.saved = credential
	return nil
}

func TestHostPairRejectsAndNeverPrintsTokens(t *testing.T) {
	store := &hostTestCredentials{}
	previous := openHostCredentialStore
	openHostCredentialStore = func() (hostbroker.CredentialStore, error) { return store, nil }
	defer func() { openHostCredentialStore = previous }()

	for _, token := range []string{"secret token", strings.Repeat("x", 64)} {
		var stdout bytes.Buffer
		err := hostCommand(context.Background(), []string{"pair"}, strings.NewReader(token), &stdout)
		if stdout.Len() != 0 || err != nil && strings.Contains(err.Error(), token) {
			t.Fatalf("pair leaked token: stdout=%q err=%v", stdout.String(), err)
		}
		if token == "secret token" && err == nil {
			t.Fatal("invalid token accepted")
		}
		if len(token) == 64 && (err != nil || store.saved.Token != token) {
			t.Fatalf("valid pair = (%+v, %v)", store.saved, err)
		}
	}
}
