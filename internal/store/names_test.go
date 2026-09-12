package store

import (
	"strings"
	"testing"
	"unicode"
)

func TestNameListsAreUniqueASCII(t *testing.T) {
	seen := func(label string, words []string) {
		t.Helper()
		got := map[string]bool{}
		for _, w := range words {
			if w == "" || strings.ContainsRune(w, '-') {
				t.Fatalf("%s %q is empty or hyphenated", label, w)
			}
			for _, r := range w {
				if r > unicode.MaxASCII || !unicode.IsLower(r) {
					t.Fatalf("%s %q is not lowercase ASCII", label, w)
				}
			}
			if got[w] {
				t.Fatalf("duplicate %s %q", label, w)
			}
			got[w] = true
		}
	}
	seen("adjective", adjectives)
	seen("animal", animals)
	if len(adjectives) < 30 || len(animals) < 30 {
		t.Fatalf("vocabulary too small: %d adjectives, %d animals", len(adjectives), len(animals))
	}
}

func TestFriendlyNameShape(t *testing.T) {
	name := friendlyName("sess-1")
	adj, animal, ok := strings.Cut(name, "-")
	if !ok || !contains(adjectives, adj) || !contains(animals, animal) {
		t.Fatalf("friendlyName = %q", name)
	}
}

func contains(words []string, want string) bool {
	for _, w := range words {
		if w == want {
			return true
		}
	}
	return false
}
