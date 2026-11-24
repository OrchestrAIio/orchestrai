package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/OrchestrAIio/orchestrai/cli/internal/api"
)

type localKeySpec struct {
	Provider string
	Label    string
	EnvVars  []string
}

var localKeySpecs = []localKeySpec{
	{Provider: "openai", Label: "OpenAI", EnvVars: []string{"OPENAI_API_KEY"}},
	{Provider: "anthropic", Label: "Anthropic", EnvVars: []string{"ANTHROPIC_API_KEY"}},
	{Provider: "google", Label: "Google Gemini", EnvVars: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	{Provider: "openrouter", Label: "OpenRouter", EnvVars: []string{"OPENROUTER_API_KEY"}},
	{Provider: "moonshot", Label: "Kimi (Moonshot)", EnvVars: []string{"MOONSHOT_API_KEY"}},
	{Provider: "z-ai", Label: "GLM (Z.ai)", EnvVars: []string{"ZAI_API_KEY", "ZHIPU_API_KEY"}},
	{Provider: "deepseek", Label: "DeepSeek", EnvVars: []string{"DEEPSEEK_API_KEY"}},
	{Provider: "qwen", Label: "Qwen (Alibaba)", EnvVars: []string{"DASHSCOPE_API_KEY", "QWEN_API_KEY"}},
}

// LocalDetectedKey is one locally set LLM provider key (secret never exposed).
type LocalDetectedKey struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
	EnvVar   string `json:"env_var"`
	Last4    string `json:"last4"`
}

func detectLocalLLMKeys(getenv func(string) string) []LocalDetectedKey {
	if getenv == nil {
		getenv = os.Getenv
	}
	var out []LocalDetectedKey
	for _, spec := range localKeySpecs {
		for _, envName := range spec.EnvVars {
			v := strings.TrimSpace(getenv(envName))
			if len(v) < 8 {
				continue
			}
			out = append(out, LocalDetectedKey{
				Provider: spec.Provider,
				Label:    spec.Label,
				EnvVar:   envName,
				Last4:    v[len(v)-4:],
			})
			break
		}
	}
	return out
}

func (a *App) cmdKeys(ctx context.Context, args []string) error {
	sub := "status"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "status", "list":
		return a.cmdKeysStatus(ctx, rest)
	case "open":
		return a.cmdKeysOpen(rest)
	default:
		return usagef("unknown keys subcommand %q (expected status, list or open)", sub)
	}
}

func (a *App) cmdKeysStatus(ctx context.Context, args []string) error {
	fs := a.flags("keys status", "[flags]")
	apiFlag := fs.String("api-url", "", "API base URL")
	localOnly := fs.Bool("local-only", false, "only inspect local environment variables (offline)")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("keys status takes no positional arguments")
	}

	localKeys := detectLocalLLMKeys(os.Getenv)
	var remote *api.LLMKeysResponse
	if !*localOnly {
		rc, err := a.repo()
		if err != nil {
			return err
		}
		u, err := a.apiURL(*apiFlag, rc.Cfg)
		if err != nil {
			return err
		}
		c, _, err := a.authedClient(u)
		if err != nil {
			return err
		}
		res, err := c.ListLLMKeys(ctx)
		if err != nil {
			return err
		}
		remote = res
	}

	if *jsonOut {
		payload := map[string]any{"local_env_keys": localKeys}
		if remote != nil {
			payload["remote_keys"] = remote.Keys
			payload["policy"] = remote.Policy
		}
		return a.printJSON(payload)
	}

	connected := map[string]api.LLMProviderKey{}
	if remote != nil {
		fmt.Fprintln(a.Stdout, "Connected BYOK keys:")
		if len(remote.Keys) == 0 {
			fmt.Fprintln(a.Stdout, "  (none)")
		}
		for _, k := range remote.Keys {
			connected[k.Provider] = k
			mask := "cloud-identity"
			if k.Last4 != "" {
				mask = "••••" + k.Last4
			}
			fmt.Fprintf(a.Stdout, "  %-16s %-8s %-10s %s (%d models)\n",
				k.Provider, k.OwnerType, k.Status, mask, len(k.ReachableModels))
		}
		if remote.Policy != nil && remote.Policy.DisallowPersonalKeys {
			fmt.Fprintln(a.Stdout, "  Note: your organization policy disallows personal BYOK keys.")
		}
		fmt.Fprintln(a.Stdout)
	}

	fmt.Fprintln(a.Stdout, "Local environment keys detected:")
	if len(localKeys) == 0 {
		fmt.Fprintln(a.Stdout, "  (none found in environment)")
		return nil
	}
	unconnected := 0
	for _, lk := range localKeys {
		state := "not connected"
		if rk, ok := connected[lk.Provider]; ok && rk.Status == "active" {
			state = "connected"
		} else {
			unconnected++
		}
		fmt.Fprintf(a.Stdout, "  %-16s %-20s ••••%s  [%s]\n",
			lk.Provider, lk.EnvVar, lk.Last4, state)
	}
	if unconnected > 0 {
		fmt.Fprintf(a.Stdout, "\nRun '%s keys open' to add keys in your browser (BYOK key writes require a verified browser session).\n", a.Prog)
	}
	return nil
}

func (a *App) cmdKeysOpen(args []string) error {
	fs := a.flags("keys open", "[flags]")
	noBrowser := fs.Bool("no-browser", false, "print the AI keys settings URL instead of opening a browser")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rc, err := a.repo()
	if err != nil {
		return err
	}
	target := strings.TrimRight(a.appURL(rc.Cfg), "/") + "/settings/ai-keys"
	if !*noBrowser && a.OpenURL != nil {
		if err := a.OpenURL(target); err == nil {
			fmt.Fprintf(a.Stdout, "Opened %s\n", target)
			return nil
		}
	}
	fmt.Fprintln(a.Stdout, target)
	return nil
}
