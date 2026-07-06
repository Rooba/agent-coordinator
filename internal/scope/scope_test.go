package scope

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveGitRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(root)
	if got := Resolve(sub); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveWorktreeMapsToMainRepo(t *testing.T) {
	tmp := t.TempDir()
	main := filepath.Join(tmp, "mainrepo")
	wt := filepath.Join(tmp, "wt1")
	// main repo with a registered worktree
	gitdir := filepath.Join(main, ".git", "worktrees", "wt1")
	write(t, filepath.Join(gitdir, "commondir"), "../..\n")
	if err := os.MkdirAll(filepath.Join(main, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	// linked worktree with a .git FILE
	write(t, filepath.Join(wt, ".git"), "gitdir: "+gitdir+"\n")
	want, _ := filepath.EvalSymlinks(main)
	if got := Resolve(wt); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveNonGit(t *testing.T) {
	d := t.TempDir()
	want, _ := filepath.EvalSymlinks(d)
	if got := Resolve(d); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
