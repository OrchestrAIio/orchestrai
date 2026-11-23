package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeRemoteURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/api.git":             "github.com/acme/api",
		"https://user:ghp_secret@GitHub.com/acme/api": "github.com/acme/api",
		"git@github.com:acme/api.git":                 "github.com/acme/api",
		"ssh://git@gitlab.example.com:2222/g/sub/r":   "gitlab.example.com/g/sub/r",
		"https://dev.azure.com/o/p/_git/r/":           "dev.azure.com/o/p/_git/r",
		"/srv/git/repo.git":                           "",
		"file:///srv/git/repo.git":                    "",
		"../other":                                    "",
		"":                                            "",
	}
	for in, want := range cases {
		if got := NormalizeRemoteURL(in); got != want {
			t.Errorf("NormalizeRemoteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoIdentity(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "my-service")
	writeFile(t, filepath.Join(repo, ".git", "config"), "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = git@github.com:up/x.git\n[remote \"origin\"]\n\turl = https://tok@github.com/acme/my-service.git\n")
	if got := RepoIdentity(repo); got != "github.com/acme/my-service" {
		t.Fatalf("origin = %q", got)
	}

	// Worktree: .git is a file pointing at the main repo's worktrees dir.
	main := filepath.Join(dir, "main")
	writeFile(t, filepath.Join(main, ".git", "config"), "[remote \"fork\"]\n\turl = git@gitlab.com:me/fork.git\n")
	wtGitdir := filepath.Join(main, ".git", "worktrees", "wt")
	writeFile(t, filepath.Join(wtGitdir, "commondir"), "../..\n")
	wt := filepath.Join(dir, "wt")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+wtGitdir+"\n")
	if got := RepoIdentity(wt); got != "gitlab.com/me/fork" {
		t.Fatalf("worktree = %q", got)
	}

	// No git: the directory name.
	plain := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := RepoIdentity(plain); got != "scratch" {
		t.Fatalf("no remote = %q", got)
	}
}
