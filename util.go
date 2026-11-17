package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
)

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...(已截断)"
}

func emit(cfg *Config, jsonVal any, text string) {
	if cfg.JSON {
		b, err := json.MarshalIndent(jsonVal, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误: JSON 序列化失败:", err)
			return
		}
		fmt.Println(string(b))
		return
	}
	if text != "" {
		fmt.Println(text)
	}
}

func table(headers []string, rows [][]string) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	_ = w.Flush()
	return strings.TrimRight(b.String(), "\n")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func isTTY(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// promptPassword 从终端读取密码；终端下尝试关闭回显。
// 非交互场景可直接用 VSHELL_PASS 或 --password。
func promptPassword() (string, error) {
	fmt.Fprint(os.Stderr, "Password: ")

	restore := disableEcho()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if restore != nil {
		restore()
	}
	fmt.Fprintln(os.Stderr)

	if err != nil && line == "" {
		return "", fmt.Errorf("读取密码失败: %w", err)
	}
	p := strings.TrimRight(line, "\r\n")
	if p == "" {
		return "", fmt.Errorf("密码为空（可用 --password 或 VSHELL_PASS）")
	}
	return p, nil
}

func disableEcho() func() {
	cmd := exec.Command("stty", "-echo")
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return nil
	}
	return func() {
		c := exec.Command("stty", "echo")
		c.Stdin = os.Stdin
		_ = c.Run()
	}
}
