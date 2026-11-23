package project

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RepoIdentity names the repository at root for sync reports (the Free plan
// counts distinct synced repos per team space): the normalized URL of the
// "origin" remote (else the first remote), or the repo root's directory name
// when there is no remote. Credentials in remote URLs are never included.
func RepoIdentity(root string) string {
	if url := originURL(root); url != "" {
		if n := NormalizeRemoteURL(url); n != "" {
			return n
		}
	}
	return filepath.Base(filepath.Clean(root))
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// NormalizeRemoteURL turns https://user:tok@GitHub.com/a/b.git,
// git@github.com:a/b.git and ssh://git@github.com:22/a/b into
// "github.com/a/b". Local paths and file:// URLs return "".
func NormalizeRemoteURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" || strings.HasPrefix(u, "file://") || strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") {
		return ""
	}
	var host, path string
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
			rest = rest[at+1:]
		}
		host, path, _ = strings.Cut(rest, "/")
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h // drop the port
		}
	} else if m := scpLike.FindStringSubmatch(u); m != nil {
		host, path = m[1], m[2]
	} else {
		return ""
	}
	host = strings.ToLower(host)
	path = strings.Trim(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if host == "" || path == "" {
		return ""
	}
	return host + "/" + path
}

// gitConfigPath finds the repo's config, following worktree .git files.
func gitConfigPath(root string) string {
	dotgit := filepath.Join(root, ".git")
	info, err := os.Stat(dotgit)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return filepath.Join(dotgit, "config")
	}
	data, err := os.ReadFile(dotgit)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	if common, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		c := strings.TrimSpace(string(common))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitdir, c)
		}
		return filepath.Join(c, "config")
	}
	return filepath.Join(gitdir, "config")
}

// originURL reads [remote "origin"] url (or the first remote's url).
func originURL(root string) string {
	path := gitConfigPath(root)
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var section, first, origin string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "url" || !strings.HasPrefix(section, `[remote "`) {
			continue
		}
		v = strings.TrimSpace(v)
		if first == "" {
			first = v
		}
		if section == `[remote "origin"]` {
			origin = v
		}
	}
	if origin != "" {
		return origin
	}
	return first
}
