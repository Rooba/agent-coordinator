package paths

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	// Bind a literal address, never a name: "localhost" is whatever the
	// resolver decides, so the loopback guarantee has to hold on the address
	// that actually gets bound.
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("AC_RELAY_LISTEN %q: the relay must listen on loopback", v)
	}
	// A service name or a junk port would otherwise surface only as a bind
	// failure, which degrades to unix-only in silence. 0 stays legal: it is
	// "any free port", which is what tests bind.
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("AC_RELAY_LISTEN %q: port must be a number in 0-65535", v)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
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
	return publishToken(path, token)
}

// publishToken makes the token file appear whole or not at all: a 0600
// temporary in the same directory is written and flushed first, then linked
// into place. First writer wins, so a racer reads the published token rather
// than an empty file, and a crash leaves nothing to unwedge by hand.
func publishToken(path, token string) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".relay.token-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Link(f.Name(), path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		return readToken(path) // a racing creator won; use its token
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
