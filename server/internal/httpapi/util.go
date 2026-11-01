package httpapi

import (
	"strconv"

	"exam.local/monaco-examd/internal/workspace"
)

func provisionWorkspace(root, studentNo string) (string, error) {
	return workspace.Provision(root, studentNo)
}

func parseInt(s string) (int, error) { return strconv.Atoi(s) }

func intText(n int) string { return strconv.Itoa(n) }
