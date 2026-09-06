package paths

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
// (the daemon's first relay listen). Both are held to the minted shape - a
// hand-typed secret opens the whole relay wherever it came from.
func RelayToken() (string, error) {
	if t := os.Getenv("AC_TOKEN"); t != "" {
		if !mintedToken(t) {
			return "", fmt.Errorf("AC_TOKEN must hold 64 lowercase hex characters")
		}
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
	if _, err := f.WriteString(token); err != nil {
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
// caller can mint one. What is there must be exactly what this daemon writes
// - 32 CSPRNG bytes as lowercase hex, mode 0600 - and anything else is
// reported rather than repaired: a weak or shared-readable secret opens the
// whole relay, and silently rewriting it would strand the paired broker.
func readToken(path string) (string, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() {
		return "", fmt.Errorf("relay token file %s must be a regular, non-symlink file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// What was opened must be the file that was checked: a path swapped
	// underneath - for a link, a device, or somebody else's file - is refused
	// rather than read.
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, after) {
		return "", fmt.Errorf("relay token file %s changed while opening", path)
	}
	if err := checkTokenPerm(path, after); err != nil {
		return "", err
	}
	// One byte past the longest token file this daemon has ever written, so
	// an oversized file is refused instead of read into memory.
	b, err := io.ReadAll(io.LimitReader(f, 66))
	if err != nil {
		return "", err
	}
	// Earlier builds ended the file with a newline. Exactly that one line
	// ending is forgiven, so an upgrade does not strand the paired broker;
	// everything else has to be the 64 hex characters that were minted.
	token, hadNewline := strings.CutSuffix(string(b), "\n")
	if hadNewline {
		token = strings.TrimSuffix(token, "\r")
	}
	if !mintedToken(token) {
		return "", fmt.Errorf("relay token file %s must hold exactly 64 lowercase hex characters: delete it to mint a new token", path)
	}
	return token, nil
}

// mintedToken reports whether a token has the shape RelayToken writes.
func mintedToken(t string) bool {
	if len(t) != 64 || strings.ToLower(t) != t {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
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
