package store

import "hash/fnv"

var adjectives = []string{"amber", "brisk", "calm", "deft", "eager", "frank", "hardy", "keen",
	"lucid", "mellow", "nimble", "proud", "quick", "solid", "tidy", "vivid", "wry", "bold"}
var animals = []string{"fox", "owl", "lynx", "otter", "hawk", "wolf", "crane", "badger",
	"heron", "mole", "stoat", "raven", "ibis", "pika", "newt", "tern", "vole", "orca"}

// friendlyName derives a stable candidate from the session id; the caller
// appends -2, -3... on collision within the scope.
func friendlyName(sessionID string) string {
	h := fnv.New32a()
	h.Write([]byte(sessionID))
	v := h.Sum32()
	return adjectives[v%uint32(len(adjectives))] + "-" + animals[(v/31)%uint32(len(animals))]
}
