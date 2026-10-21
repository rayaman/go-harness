package config

import (
	"os"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

var (
	config *Config
)

type Config struct {
	IgnoreFile    string
	HotReload     bool
	CustomToolDir string

	// ToolPermissions gates tools by name (roadmap 1.2): each list names
	// tools whose policy is allow / ask / deny. On a name collision
	// deny beats ask beats allow. This package deliberately does NOT
	// import harness (harness -> harness/utils -> harness/config would
	// be an import cycle), so callers apply the lists themselves via
	// harness.Registry.ApplyPolicies. Hot-reload repopulates them in
	// place via the existing OnConfigChange hook.
	ToolPermissions struct {
		Allow []string
		Ask   []string
		Deny  []string
	}

	// MCPServers declares external MCP tool servers to spawn over stdio
	// (roadmap 2.2), from the `mcp-servers` list-of-maps key. Like the
	// permission lists, this package does not import harness/mcp; the
	// REPL and the workflow runner start the servers themselves.
	// Hot-reload repopulates the slice in place, but already-spawned
	// servers are not re-killed/re-spawned by a config edit alone.
	MCPServers []MCPServerConfig

	// MemoryFile is the persistent memory file (roadmap 2.4), from the
	// `memory-file` key. This package does not import harness (import
	// cycle), so it is just a path string; main.go turns it into a
	// *harness.Memory. Empty means memory is DISABLED at the harness
	// level (the zero value keeps embedders/workflows unchanged); the
	// REPL defaults it to agent/memory.md.
	MemoryFile string

	// LogDir is the JSONL transcript log directory (roadmap 3.1), from
	// the `log-dir` key. Like MemoryFile this package does not import
	// harness; callers copy it onto harness.Config.LogDir. Empty means
	// transcript logging stays DISABLED (byte-for-byte default
	// behavior); the REPL does not default it, matching the roadmap.
	LogDir string
}

// MCPServerConfig is one `mcp-servers` entry: the executable to spawn
// and its argv. Command may be a Windows .cmd/.bat shim; exec.Command
// resolves those via cmd.exe.
type MCPServerConfig struct {
	Name    string
	Command string
	Args    []string
}

func GetConfig() *Config {
	return config
}

func init() {
	viper.SetConfigName("config")
	viper.AddConfigPath(".")
	viper.AddConfigPath("agent")
	viper.AddConfigPath(os.Getenv("AGENT_CONFIG_PATH"))
	viper.WatchConfig()
	loadConfig()
	viper.OnConfigChange(func(in fsnotify.Event) {
		loadConfig()
	})
}

func loadConfig() {
	err := viper.ReadInConfig()
	if err != nil {
		panic(err)
	}

	if config == nil {
		config = &Config{}
	}

	config.IgnoreFile = viper.GetString("ignore-file")
	config.HotReload = viper.GetBool("hot-reload")
	config.CustomToolDir = viper.GetString("custom-tool-dir")
	config.ToolPermissions.Allow = viper.GetStringSlice("tool-permissions.allow")
	config.ToolPermissions.Ask = viper.GetStringSlice("tool-permissions.ask")
	config.ToolPermissions.Deny = viper.GetStringSlice("tool-permissions.deny")
	// mcp-servers is a list of maps; viper.UnmarshalKey decodes it into
	// the typed slice in the same hot-reload pass as the scalar getters.
	var mcpServers []MCPServerConfig
	if err := viper.UnmarshalKey("mcp-servers", &mcpServers); err != nil {
		mcpServers = nil
	}
	config.MCPServers = mcpServers
	config.MemoryFile = viper.GetString("memory-file")
	config.LogDir = viper.GetString("log-dir")
}
