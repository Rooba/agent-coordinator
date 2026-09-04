package paths

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// defaultRelayAddr is where the opt-in relay listens. Loopback only: the
// relay is a hop between WSL and the Windows desktop session, never a
// network service.
const defaultRelayAddr = "127.0.0.1:7400"

// RelayListen returns the address the daemon should offer to relay clients,
// or "" when AC_RELAY_LISTEN is unset - the relay is off by default. "1" or
// "true" means the default address; anything else is a host:port. A
// non-loopback host is refused outright so a typo cannot publish the daemon
// to the network.
func RelayListen() (string, error) {
	v := strings.TrimSpace(os.Getenv("AC_RELAY_LISTEN"))
	switch strings.ToLower(v) {
	case "":
		return "", nil
	case "1", "true":
		return defaultRelayAddr, nil
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", fmt.Errorf("AC_RELAY_LISTEN %q: want host:port", v)
	}
	if !loopback(host) {
		return "", fmt.Errorf("AC_RELAY_LISTEN %q: the relay must listen on loopback", v)
	}
	return net.JoinHostPort(host, port), nil
}

// loopback reports whether a host string names this machine and nothing else.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RelayTokenPath is the shared relay token's file, next to the state
// database so pairing a Windows broker is one file copy.
func RelayTokenPath() (string, error) {
	db, err := DB()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(db), "relay.token"), nil
}

// RelayToken returns the shared relay secret: AC_TOKEN wins for clients,
// otherwise the token file, created 0600 with 32 CSPRNG bytes on first use
// (the daemon's first relay listen).
func RelayToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv("AC_TOKEN")); t != "" {
		return t, nil
	}
	path, err := RelayTokenPath()
	if err != nil {
		return "", err
	}
	token, err := readToken(path)
	if err != nil || token != "" {
		return token, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token = hex.EncodeToString(b)
	// O_EXCL settles a race between two creators on one token rather than two.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readToken(path) // a racing creator won; use its token
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(token + "\n"); err != nil {
		return "", err
	}
	return token, nil
}

// readToken reads the token file, reporting "" when it is absent so the
// caller can mint one. An existing but empty file is an error, never a
// blank secret that would authorize every caller.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("relay token file %s is empty: delete it to mint a new token", path)
	}
	return token, nil
}

// RelayInsecure reports whether the token check is disabled - local test
// rigs only.
func RelayInsecure() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AC_RELAY_INSECURE"))) {
	case "1", "true":
		return true
	}
	return false
}
