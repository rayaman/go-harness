package tools

import (
	"ai-harness/harness/utils"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func CreateFiles(files []string, path string) {
	for _, file := range files {
		f, _ := os.Create(filepath.Join(path, file))
		f.Close()
	}
}

func TestListFiles(t *testing.T) {
	dir := t.TempDir()
	ignore := filepath.Join(dir, ".myignore")
	t.Setenv("IGNORE_FILE", ignore)
	file, _ := os.Create(ignore)
	defer file.Close()
	file.WriteString(`
.*
me.txt`)
	CreateFiles([]string{"test1", "test2", ".haha", "me.txt"}, dir)
	lft := ListFilesTool{BaseDir: dir}
	data := []byte(`{
		"path": ".",
		"recursive": true
	}`)
	res, err := lft.Execute(data)
	assert.Nil(t, err)
	fs := &[]string{}
	err = json.Unmarshal([]byte(res), fs)
	assert.Nil(t, err)
	fmt.Println(res)

	for _, f := range *fs {
		b, _ := utils.MatchPatterns(f)
		fmt.Println(f, b)
	}
}
