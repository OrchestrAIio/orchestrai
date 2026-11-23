package cli

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// CheckFinding is one deterministic local readiness issue found by `orchestrai check`.
type CheckFinding struct {
	Severity   string `json:"severity"` // critical | high | medium | low
	Category   string `json:"category"` // dockerfile | terraform | kubernetes | github-actions | secrets
	Rule       string `json:"rule"`
	File       string `json:"file"`
	Line       int    `json:"line,omitempty"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

// CheckReport is the --json shape for `orchestrai check`.
type CheckReport struct {
	Path         string         `json:"path"`
	FilesScanned int            `json:"files_scanned"`
	Counts       map[string]int `json:"counts"`
	Findings     []CheckFinding `json:"findings"`
}

var checkSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".terraform":   true,
	".venv":        true,
	"venv":         true,
	"dist":         true,
	"build":        true,
	"__pycache__":  true,
	".orchestrai":  true,
	".next":        true,
}

const maxCheckFileBytes = 1 << 20

var (
	reDockerFrom      = regexp.MustCompile(`(?i)^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)
	reDockerUser      = regexp.MustCompile(`(?i)^\s*USER\s+(\S+)`)
	reCurlPipeSh      = regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^|\n]*\|\s*(?:sudo\s+)?(?:ba|da|z|a)?sh\b`)
	reTFOpenCIDR      = regexp.MustCompile(`["'](0\.0\.0\.0/0|::/0)["']`)
	reTFPublicAccess  = regexp.MustCompile(`(?i)\bpublicly_accessible\s*=\s*true\b`)
	reTFUnencrypted   = regexp.MustCompile(`(?i)\b(?:storage_)?encrypted\s*=\s*false\b`)
	reGHAUses         = regexp.MustCompile(`^\s*(?:-\s*)?uses\s*:\s*['"]?([^'"\s#]+)['"]?`)
	reGHAPermWriteAll = regexp.MustCompile(`(?i)^\s*permissions\s*:\s*write-all\b`)
	reGHACommitSHA    = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	reGHAMajorOrSemvr = regexp.MustCompile(`^v\d+(?:\.\d+){0,2}$`)
	reK8sPrivileged   = regexp.MustCompile(`(?i)^\s*privileged\s*:\s*true\b`)
	reSecretPatterns  = []struct {
		rule string
		re   *regexp.Regexp
		desc string
	}{
		{"hardcoded-aws-key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "Possible hardcoded AWS access key ID (AKIA...)"},
		{"hardcoded-github-token", regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}\b`), "Possible hardcoded GitHub personal access token (ghp_...)"},
		{"hardcoded-stripe-key", regexp.MustCompile(`\bsk_live_[A-Za-z0-9]{16,}\b`), "Possible hardcoded Stripe live secret key (sk_live_...)"},
		{"hardcoded-private-key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`), "Committed private key material"},
	}
)

func (a *App) cmdCheck(_ context.Context, args []string) error {
	fs := a.flags("check", "[path] [--json] [--exit-code]")
	asJSON := fs.Bool("json", false, "print JSON")
	exitCodeFlag := fs.Bool("exit-code", false, "exit 1 when findings are reported")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return usagef("check accepts at most one path")
	}
	target := a.Dir
	if fs.NArg() == 1 {
		p := fs.Arg(0)
		if !filepath.IsAbs(p) {
			p = filepath.Join(a.Dir, p)
		}
		target = filepath.Clean(p)
	}
	report, err := runLocalCheck(target)
	if err != nil {
		return err
	}
	if *asJSON {
		if err := a.printJSON(report); err != nil {
			return err
		}
	} else {
		a.printCheckReport(report)
	}
	if *exitCodeFlag && len(report.Findings) > 0 {
		return silentExit(ExitOutOfSync)
	}
	return nil
}

func runLocalCheck(root string) (*CheckReport, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	rep := &CheckReport{
		Path:     root,
		Counts:   map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0, "total": 0},
		Findings: []CheckFinding{},
	}
	scanOne := func(fullPath, rel string) error {
		base := filepath.Base(fullPath)
		lowerBase := strings.ToLower(base)
		rep.FilesScanned++

		if isCommittedEnvFile(lowerBase) {
			rep.Findings = append(rep.Findings, CheckFinding{
				Severity:   "critical",
				Category:   "secrets",
				Rule:       "committed-env-file",
				File:       rel,
				Line:       1,
				Message:    fmt.Sprintf("Committed environment file %s may contain secrets.", base),
				Suggestion: "Remove from git, add .env* to .gitignore, and keep only .env.example with placeholders.",
			})
		}

		info, err := os.Stat(fullPath)
		if err != nil || info.Size() > maxCheckFileBytes {
			return nil
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return nil
		}
		if looksBinaryBytes(data) {
			return nil
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")

		rep.Findings = append(rep.Findings, scanHardcodedSecrets(rel, text)...)

		switch {
		case isDockerfileName(base):
			rep.Findings = append(rep.Findings, scanDockerfile(rel, text)...)
		case strings.HasSuffix(lowerBase, ".tf"):
			rep.Findings = append(rep.Findings, scanTerraform(rel, text)...)
		case strings.HasSuffix(lowerBase, ".yml") || strings.HasSuffix(lowerBase, ".yaml"):
			if strings.HasPrefix(strings.ToLower(rel), ".github/workflows/") {
				rep.Findings = append(rep.Findings, scanGitHubWorkflow(rel, text)...)
			}
			if isKubernetesManifest(text) {
				rep.Findings = append(rep.Findings, scanKubernetesManifest(rel, text)...)
			}
		}
		return nil
	}

	if !fi.IsDir() {
		if err := scanOne(root, filepath.Base(root)); err != nil {
			return nil, err
		}
	} else {
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				if p != root && checkSkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return nil
			}
			return scanOne(p, filepath.ToSlash(rel))
		})
		if err != nil {
			return nil, err
		}
	}

	severityRank := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		ri, rj := severityRank[rep.Findings[i].Severity], severityRank[rep.Findings[j].Severity]
		if ri != rj {
			return ri < rj
		}
		if rep.Findings[i].File != rep.Findings[j].File {
			return rep.Findings[i].File < rep.Findings[j].File
		}
		return rep.Findings[i].Line < rep.Findings[j].Line
	})
	for _, f := range rep.Findings {
		rep.Counts[f.Severity]++
		rep.Counts["total"]++
	}
	return rep, nil
}

func looksBinaryBytes(b []byte) bool {
	limit := len(b)
	if limit > 8192 {
		limit = 8192
	}
	for i := 0; i < limit; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

func isCommittedEnvFile(lowerBase string) bool {
	if lowerBase == ".env" {
		return true
	}
	if !strings.HasPrefix(lowerBase, ".env.") {
		return false
	}
	for _, allowed := range []string{".env.example", ".env.template", ".env.sample", ".env.dist", ".env.defaults"} {
		if lowerBase == allowed {
			return false
		}
	}
	if strings.HasSuffix(lowerBase, ".example") || strings.HasSuffix(lowerBase, ".template") || strings.HasSuffix(lowerBase, ".sample") {
		return false
	}
	return true
}

func isDockerfileName(base string) bool {
	lower := strings.ToLower(base)
	return lower == "dockerfile" || strings.HasPrefix(lower, "dockerfile.") || strings.HasSuffix(lower, ".dockerfile")
}

func scanDockerfile(rel, text string) []CheckFinding {
	var out []CheckFinding
	lines := strings.Split(text, "\n")
	stages := map[string]bool{}
	hasFrom := false
	lastStageHasNonRootUser := false
	lastFromLine := 1

	for i, raw := range lines {
		lineNo := i + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if m := reDockerFrom.FindStringSubmatch(raw); m != nil {
			hasFrom = true
			lastFromLine = lineNo
			lastStageHasNonRootUser = false
			img := m[1]
			alias := strings.ToLower(m[2])
			if alias != "" {
				stages[alias] = true
			}
			lowerImg := strings.ToLower(img)
			if lowerImg != "scratch" && !stages[lowerImg] && !strings.HasPrefix(img, "$") {
				if strings.HasSuffix(lowerImg, ":latest") || (!strings.Contains(img, ":") && !strings.Contains(img, "@sha256:")) {
					out = append(out, CheckFinding{
						Severity:   "medium",
						Category:   "dockerfile",
						Rule:       "dockerfile-latest-tag",
						File:       rel,
						Line:       lineNo,
						Message:    fmt.Sprintf("Base image %q uses :latest or an unpinned tag.", img),
						Suggestion: "Pin the base image to an explicit version tag or digest (@sha256:...).",
					})
				}
			}
			continue
		}
		if m := reDockerUser.FindStringSubmatch(raw); m != nil {
			u := strings.ToLower(strings.TrimSpace(m[1]))
			if u != "root" && u != "0" && !strings.HasPrefix(u, "root:") && !strings.HasPrefix(u, "0:") {
				lastStageHasNonRootUser = true
			} else {
				lastStageHasNonRootUser = false
			}
		}
		if reCurlPipeSh.MatchString(raw) {
			out = append(out, CheckFinding{
				Severity:   "high",
				Category:   "dockerfile",
				Rule:       "dockerfile-curl-pipe-sh",
				File:       rel,
				Line:       lineNo,
				Message:    "Piping curl/wget directly into a shell executes unverified remote code.",
				Suggestion: "Download the artifact, verify its SHA-256 checksum, then execute it.",
			})
		}
	}
	if hasFrom && !lastStageHasNonRootUser {
		out = append(out, CheckFinding{
			Severity:   "high",
			Category:   "dockerfile",
			Rule:       "dockerfile-missing-user",
			File:       rel,
			Line:       lastFromLine,
			Message:    "Container runs as root (no non-root USER instruction in the final stage).",
			Suggestion: "Add a dedicated non-root user and switch to it with `USER <nonroot>`.",
		})
	}
	return out
}

func scanTerraform(rel, text string) []CheckFinding {
	var out []CheckFinding
	lines := strings.Split(text, "\n")
	inIngress := false
	ingressDepth := 0
	for i, raw := range lines {
		lineNo := i + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "ingress") && strings.Contains(lower, "{") {
			inIngress = true
			ingressDepth = strings.Count(raw, "{") - strings.Count(raw, "}")
		} else if inIngress {
			ingressDepth += strings.Count(raw, "{") - strings.Count(raw, "}")
			if ingressDepth <= 0 {
				inIngress = false
			}
		}
		if reTFOpenCIDR.MatchString(raw) && (inIngress || strings.Contains(lower, "ingress") || strings.Contains(lower, "cidr_blocks") || strings.Contains(lower, "source_ranges")) {
			out = append(out, CheckFinding{
				Severity:   "high",
				Category:   "terraform",
				Rule:       "tf-open-ingress",
				File:       rel,
				Line:       lineNo,
				Message:    "Ingress rule allows traffic from 0.0.0.0/0 (entire internet).",
				Suggestion: "Restrict CIDR blocks to known VPN, office, or load-balancer ranges.",
			})
		}
		if reTFPublicAccess.MatchString(raw) {
			out = append(out, CheckFinding{
				Severity:   "high",
				Category:   "terraform",
				Rule:       "tf-publicly-accessible",
				File:       rel,
				Line:       lineNo,
				Message:    "Resource sets publicly_accessible = true.",
				Suggestion: "Set publicly_accessible = false and route access through a private subnet or bastion.",
			})
		}
		if reTFUnencrypted.MatchString(raw) {
			out = append(out, CheckFinding{
				Severity:   "high",
				Category:   "terraform",
				Rule:       "tf-unencrypted",
				File:       rel,
				Line:       lineNo,
				Message:    "Resource disables encryption at rest (encrypted/storage_encrypted = false).",
				Suggestion: "Enable encryption at rest (encrypted = true / storage_encrypted = true).",
			})
		}
	}
	return out
}

func scanGitHubWorkflow(rel, text string) []CheckFinding {
	var out []CheckFinding
	lines := strings.Split(text, "\n")
	for i, raw := range lines {
		lineNo := i + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if reGHAPermWriteAll.MatchString(raw) {
			out = append(out, CheckFinding{
				Severity:   "high",
				Category:   "github-actions",
				Rule:       "gha-write-all-permissions",
				File:       rel,
				Line:       lineNo,
				Message:    "Workflow grants `permissions: write-all` to GITHUB_TOKEN.",
				Suggestion: "Follow least privilege and declare only the specific permissions needed (e.g. `contents: read`).",
			})
		}
		if m := reGHAUses.FindStringSubmatch(raw); m != nil {
			target := m[1]
			if strings.HasPrefix(target, "./") || strings.HasPrefix(target, "docker://") {
				continue
			}
			_, ref, hasAt := strings.Cut(target, "@")
			if !hasAt || (!reGHACommitSHA.MatchString(ref) && !reGHAMajorOrSemvr.MatchString(ref)) {
				out = append(out, CheckFinding{
					Severity:   "medium",
					Category:   "github-actions",
					Rule:       "gha-unpinned-action",
					File:       rel,
					Line:       lineNo,
					Message:    fmt.Sprintf("GitHub Action %q is not pinned to a commit SHA or major version tag.", target),
					Suggestion: "Pin actions to a full 40-char commit SHA (or at minimum a major version like @v4).",
				})
			}
		}
	}
	return out
}

func isKubernetesManifest(text string) bool {
	hasAPI := false
	hasKind := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "apiVersion:") {
			hasAPI = true
		} else if strings.HasPrefix(t, "kind:") {
			hasKind = true
		}
		if hasAPI && hasKind {
			return true
		}
	}
	return false
}

func scanKubernetesManifest(rel, text string) []CheckFinding {
	var out []CheckFinding
	lines := strings.Split(text, "\n")

	// Check privileged: true anywhere in the manifest.
	for i, raw := range lines {
		if reK8sPrivileged.MatchString(raw) {
			out = append(out, CheckFinding{
				Severity:   "critical",
				Category:   "kubernetes",
				Rule:       "k8s-privileged-container",
				File:       rel,
				Line:       i + 1,
				Message:    "Kubernetes container sets `privileged: true`.",
				Suggestion: "Remove `privileged: true` and grant only specific Linux capabilities if required.",
			})
		}
	}

	// Track containers: blocks and verify each container declares resources.limits.
	inContainers := false
	containersIndent := -1
	containerStartLine := -1
	containerIndent := -1
	containerName := ""
	hasResources := false
	hasLimits := false

	flushContainer := func() {
		if containerStartLine > 0 && (!hasResources || !hasLimits) {
			label := "Kubernetes container"
			if containerName != "" {
				label = fmt.Sprintf("Kubernetes container %q", containerName)
			}
			out = append(out, CheckFinding{
				Severity:   "medium",
				Category:   "kubernetes",
				Rule:       "k8s-missing-resource-limits",
				File:       rel,
				Line:       containerStartLine,
				Message:    label + " is missing `resources.limits`.",
				Suggestion: "Define `resources.limits` (cpu and memory) to prevent noisy-neighbor outages.",
			})
		}
		containerStartLine = -1
		containerIndent = -1
		containerName = ""
		hasResources = false
		hasLimits = false
	}

	for i, raw := range lines {
		lineNo := i + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "---" {
			flushContainer()
			inContainers = false
			containersIndent = -1
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		if inContainers && indent <= containersIndent && !strings.HasPrefix(trimmed, "-") {
			flushContainer()
			inContainers = false
			containersIndent = -1
		}
		if trimmed == "containers:" || trimmed == "initContainers:" {
			flushContainer()
			inContainers = true
			containersIndent = indent
			continue
		}
		if !inContainers {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") && (containerIndent < 0 || indent == containerIndent) {
			flushContainer()
			containerStartLine = lineNo
			containerIndent = indent
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			if strings.HasPrefix(rest, "name:") {
				containerName = strings.Trim(strings.TrimSpace(strings.TrimPrefix(rest, "name:")), `"'`)
			}
			continue
		}
		if containerStartLine > 0 {
			if strings.HasPrefix(trimmed, "name:") && containerName == "" {
				containerName = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "name:")), `"'`)
			}
			if trimmed == "resources:" {
				hasResources = true
			}
			if hasResources && trimmed == "limits:" {
				hasLimits = true
			}
		}
	}
	flushContainer()
	return out
}

func scanHardcodedSecrets(rel, text string) []CheckFinding {
	var out []CheckFinding
	lines := strings.Split(text, "\n")
	for i, raw := range lines {
		if strings.Contains(raw, "orchestrai:allow-secret") {
			continue
		}
		for _, p := range reSecretPatterns {
			if p.re.MatchString(raw) {
				out = append(out, CheckFinding{
					Severity:   "critical",
					Category:   "secrets",
					Rule:       p.rule,
					File:       rel,
					Line:       i + 1,
					Message:    p.desc + ".",
					Suggestion: "Move this secret to a secret manager or environment variable and rotate the exposed credential.",
				})
				break
			}
		}
	}
	return out
}

func (a *App) printCheckReport(rep *CheckReport) {
	w := a.Stdout
	if len(rep.Findings) == 0 {
		fmt.Fprintf(w, "No readiness issues found across %d file(s).\n", rep.FilesScanned)
		return
	}
	fmt.Fprintf(w, "OrchestrAI Readiness Check — %d issue(s) across %d file(s) (critical: %d, high: %d, medium: %d, low: %d)\n",
		rep.Counts["total"], rep.FilesScanned, rep.Counts["critical"], rep.Counts["high"], rep.Counts["medium"], rep.Counts["low"])
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if rep.Counts[sev] == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%d)\n", strings.ToUpper(sev), rep.Counts[sev])
		for _, f := range rep.Findings {
			if f.Severity != sev {
				continue
			}
			loc := f.File
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Fprintf(w, "  %s  [%s] %s\n", loc, f.Rule, f.Message)
			if f.Suggestion != "" {
				fmt.Fprintf(w, "    Fix: %s\n", f.Suggestion)
			}
		}
	}
	fmt.Fprintf(w, "\nHave OrchestrAI fix these and open a PR (from $5 = 100 credits, or Pro $29/mo): %s ask \"Fix the readiness issues found by orchestrai check\"\n", a.Prog)
}
