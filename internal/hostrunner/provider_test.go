package hostrunner

import (
	"errors"
	"reflect"
	"testing"
)

type namedProvider string

func (p namedProvider) Name() string                             { return string(p) }
func (p namedProvider) Prepare(Task, string) (Invocation, error) { return Invocation{}, nil }

func TestRegistryIsAnAllowlist(t *testing.T) {
	registry, err := NewRegistry(namedProvider("zeta"), namedProvider("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := registry.Names(), []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if _, err := registry.Provider("missing"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown provider error = %v", err)
	}
	if _, err := NewRegistry(namedProvider("same"), namedProvider("same")); err == nil {
		t.Fatal("duplicate provider was accepted")
	}
	if _, err := NewRegistry(namedProvider("Unsafe Provider")); err == nil {
		t.Fatal("unsafe provider name was accepted")
	}
}
