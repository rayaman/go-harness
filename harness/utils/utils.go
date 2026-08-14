package utils

import (
	"ai-harness/harness/config"
	"path/filepath"

	ignore "github.com/sabhiram/go-gitignore"
)

var conf = config.GetConfig()
var ignoreFile *ignore.GitIgnore

func MatchPatterns(file string) (bool, error) {
	err := compileIgnore()
	if err != nil {
		return false, err
	}
	slashPath := filepath.ToSlash(file)
	return ignoreFile.MatchesPath(slashPath), nil
}

func compileIgnore() error {
	var err error
	if conf.HotReload || ignoreFile == nil {
		ignoreFile, err = ignore.CompileIgnoreFile(conf.IgnoreFile)
		if err != nil {
			return err
		}
	}
	return nil
}
