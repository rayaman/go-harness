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
	IgnoreFile string
	HotReload  bool
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
}
