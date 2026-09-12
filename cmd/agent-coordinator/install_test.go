package main

import (
	"errors"
	"flag"
	"reflect"
	"testing"
)

func TestSplitEyesFlag(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		eyes bool
		rest []string
	}{
		{nil, false, []string{}},
		{[]string{"--uninstall"}, false, []string{"--uninstall"}},
		{[]string{"--eyes"}, true, []string{}},
		{[]string{"--eyes", "--dry-run"}, true, []string{"--dry-run"}},
		{[]string{"--eyes", "--uninstall"}, true, []string{"--uninstall"}},
		{[]string{"--uninstall", "--eyes"}, true, []string{"--uninstall"}},
	} {
		eyes, rest := splitEyesFlag(tc.in)
		if eyes != tc.eyes || !reflect.DeepEqual(rest, tc.rest) {
			t.Fatalf("splitEyesFlag(%q) = (%v, %q), want (%v, %q)", tc.in, eyes, rest, tc.eyes, tc.rest)
		}
	}
}

func TestParseInstallArgsPreflight(t *testing.T) {
	in, err := parseInstallArgs(nil)
	if err != nil || in.uninstall || in.dryRun || in.setup != nil {
		t.Fatalf("empty: %+v %v", in, err)
	}
	in, err = parseInstallArgs([]string{"--uninstall"})
	if err != nil || !in.uninstall || in.setup != nil {
		t.Fatalf("uninstall: %+v %v", in, err)
	}
	in, err = parseInstallArgs([]string{"--eyes", "--dry-run"})
	if err != nil || in.setup == nil || !in.dryRun || in.uninstall {
		t.Fatalf("dry-run: %+v %v", in, err)
	}
	_, err = parseInstallArgs([]string{"--eyes", "--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help err=%v", err)
	}
	_, err = parseInstallArgs([]string{"--eyes", "--not-a-flag"})
	if err == nil {
		t.Fatal("invalid flag accepted")
	}
	_, err = parseInstallArgs([]string{"--eyes", "positional"})
	if err == nil {
		t.Fatal("positional accepted")
	}
	for _, args := range [][]string{
		{"--eyes", "--uninstall"},
		{"--eyes", "--uninstall", "--dry-run"},
		{"--uninstall", "--eyes"},
		{"--uninstall", "--dry-run"},
		{"--uninstall", "extra"},
	} {
		in, err = parseInstallArgs(args)
		if err == nil || in.uninstall || in.setup != nil {
			t.Fatalf("%q must not uninstall: %+v %v", args, in, err)
		}
	}
}
