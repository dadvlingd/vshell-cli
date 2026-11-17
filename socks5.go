package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// dialSOCKS5 实现最小 SOCKS5 客户端握手（无认证、CONNECT），
// 用于通过 VShell 隧道访问内网目标。
func dialSOCKS5(proxyAddr, targetAddr string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, err
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))

	// greeting: VER=5, NMETHODS=1, METHOD=0 (no auth)
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fail(err)
	}
	greet := make([]byte, 2)
	if _, err := io.ReadFull(conn, greet); err != nil {
		return fail(err)
	}
	if greet[0] != 0x05 || greet[1] != 0x00 {
		return fail(fmt.Errorf("SOCKS5 握手失败（version=%d method=%d）", greet[0], greet[1]))
	}

	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return fail(fmt.Errorf("目标地址需要 host:port: %w", err))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fail(fmt.Errorf("端口无效: %s", portStr))
	}

	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fail(errors.New("域名过长"))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))

	if _, err := conn.Write(req); err != nil {
		return fail(err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fail(err)
	}
	if head[0] != 0x05 {
		return fail(fmt.Errorf("SOCKS5 响应版本异常: %d", head[0]))
	}
	if head[1] != 0x00 {
		return fail(fmt.Errorf("SOCKS5 CONNECT 被拒绝，错误码 0x%02x", head[1]))
	}
	switch head[3] {
	case 0x01: // IPv4
		if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
			return fail(err)
		}
	case 0x04: // IPv6
		if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
			return fail(err)
		}
	case 0x03: // domain
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fail(err)
		}
		if _, err := io.ReadFull(conn, make([]byte, int(l[0])+2)); err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("SOCKS5 未知地址类型: %d", head[3]))
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (c *Client) socksHTTPClient(port int, timeout time.Duration) *http.Client {
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.Insecure},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialSOCKS5(proxyAddr, addr, timeout)
		},
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// TunnelHTTP 通过本地 SOCKS5 隧道端口发起一次 HTTP 请求。
func (c *Client) TunnelHTTP(port int, method, rawURL string, headers []string, data string, timeout time.Duration) (HTTPResult, error) {
	req, err := BuildRequest(method, rawURL, headers, data)
	if err != nil {
		return HTTPResult{Engine: "socks5", Error: err.Error()}, err
	}
	start := time.Now()
	resp, err := c.socksHTTPClient(port, timeout).Do(req)
	if err != nil {
		return HTTPResult{Engine: "socks5", Error: err.Error()}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return HTTPResult{Engine: "socks5", Error: err.Error()}, err
	}
	return HTTPResult{
		Engine: "socks5", OK: true, Status: resp.StatusCode,
		Elapsed: time.Since(start).Seconds(), Size: len(body), Body: string(body),
	}, nil
}
