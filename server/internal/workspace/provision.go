package workspace

import (
	"os"
	"path/filepath"
)

const defaultMain = `package main

import (
	"bufio"
	"fmt"
	"os"
)

func main() {
	in := bufio.NewReader(os.Stdin)
	_ = in
	fmt.Println("hello, exam")
}
`

const defaultMod = "module exam\n\ngo 1.26\n"

const defaultReadme = `# 考场工作区

- 在 main.go 里完成题目，从标准输入读入，向标准输出打印结果。
- 保存后服务端每 15 秒自动快照一次。
- 断线时可以继续编辑，恢复连接后会自动同步。
`

// vscode 会主动探测这些文件；预先放好空对象，避免大量 404。
var defaultVscodeFiles = map[string]string{
	".vscode/settings.json": "{}",
	".vscode/tasks.json":    "{}",
	".vscode/launch.json":   "{}",
	".vscode/mcp.json":      "{}",
}

// Provision 创建（或补齐）考生工作区，返回工作区目录。
func Provision(root, studentNo string) (string, error) {
	dir := filepath.Join(root, studentNo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	files := map[string]string{
		"main.go":   defaultMain,
		"go.mod":    defaultMod,
		"README.md": defaultReadme,
	}
	for name, content := range defaultVscodeFiles {
		files[name] = content
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}
