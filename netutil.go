package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// runCurl 通过系统 curl 发请求，返回 (原始响应, 状态码, 字节数, error)。
func runCurl(method, rawURL string, headers []string, body string, timeout time.Duration) (string, int, int, error) {
	if _, err := exec.LookPath("curl"); err != nil {
		return "", 0, 0, errors.New("curl 未安装")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()

	args := []string{"-s", "-S", "-i", "-X", method, "--max-time", strconv.Itoa(int(timeout.Seconds()))}
	for _, h := range headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return "", 0, 0, fmt.Errorf("请求头格式应为 'Key: Value': %s", h)
		}
		args = append(args, "-H", strings.TrimSpace(k)+": "+strings.TrimSpace(v))
	}
	switch strings.ToUpper(method) {
	case "POST", "PUT", "PATCH":
		if body != "" {
			args = append(args, "--data", body)
		}
	}
	args = append(args, rawURL)

	cmd := exec.CommandContext(ctx, "curl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", 0, 0, errors.New("curl 超时")
	}
	out := stdout.String()
	if err != nil && out == "" {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", 0, 0, fmt.Errorf("curl 执行失败: %s", msg)
	}

	status := 0
	if line, _, ok := strings.Cut(out, "\n"); ok || line != "" {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			status, _ = strconv.Atoi(fields[1])
		}
	}
	return out, status, len(out), nil
}

func curlVersion() (string, error) {
	if _, err := exec.LookPath("curl"); err != nil {
		return "", errors.New("未安装")
	}
	out, err := exec.Command("curl", "--version").Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line), nil
}

// splitCurlResponse 把 curl -i 的输出拆成响应头和响应体。
func splitCurlResponse(raw string) (map[string]string, string) {
	sep := "\r\n\r\n"
	i := strings.Index(raw, sep)
	if i < 0 {
		sep = "\n\n"
		i = strings.Index(raw, sep)
	}
	if i < 0 {
		return map[string]string{}, raw
	}
	head, body := raw[:i], raw[i+len(sep):]
	hdrs := map[string]string{}
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "HTTP/") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			hdrs[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return hdrs, body
}
