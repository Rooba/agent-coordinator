package scope

import (
	"os"
	"path/filepath"
	"strings"
)

// Effective returns the scope a command should use: AC_SCOPE when set,
// otherwise the workspace containing cwd.
func Effective(cwd string) string {
	if v := strings.TrimSpace(os.Getenv("AC_SCOPE")); v != "" {
		return v
	}
	return Resolve(cwd)
}

// Here is Effective for the process working directory.
func Here() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return Effective(cwd)
}

// Resolve maps a working directory to its workspace scope key.
func Resolve(cwd string) string {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return filepath.Clean(cwd)
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	for d := dir; ; {
		gitPath := filepath.Join(d, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			if fi.IsDir() {
				return d
			}
			if main := mainRepoRoot(gitPath); main != "" {
				return main
			}
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// mainRepoRoot resolves a .git FILE (linked worktree) to the main repo root.
func mainRepoRoot(gitFile string) string {
	b, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitFile), gitdir)
	}
	if cb, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		cd := strings.TrimSpace(string(cb))
		if !filepath.IsAbs(cd) {
			cd = filepath.Join(gitdir, cd)
		}
		gitdir = filepath.Clean(cd)
	}
	if filepath.Base(gitdir) == ".git" {
		root := filepath.Dir(gitdir)
		if r, err := filepath.EvalSymlinks(root); err == nil {
			return r
		}
		return root
	}
	return ""
}
