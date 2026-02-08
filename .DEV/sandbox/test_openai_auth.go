// go run test_openai_auth.go
// 测试到 auth.openai.com 的连通性，诊断 codex token refresh 失败原因
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	authHost = "auth.openai.com"
	tokenURL = "https://auth.openai.com/oauth/token"
)

func main() {
	fmt.Println("=== OpenAI Auth 连通性诊断 ===")
	fmt.Printf("时间: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	// Step 1: DNS 解析
	fmt.Println("--- Step 1: DNS 解析 auth.openai.com ---")
	ips, err := net.LookupHost(authHost)
	if err != nil {
		fmt.Printf("  DNS 解析失败: %v\n", err)
	} else {
		fmt.Printf("  解析结果: %v\n", ips)
		for _, ip := range ips {
			parsed := net.ParseIP(ip)
			if parsed != nil {
				// 检查是否是 Fake IP (198.18.x.x 或 198.19.x.x)
				if strings.HasPrefix(ip, "198.18.") || strings.HasPrefix(ip, "198.19.") {
					fmt.Printf("  ⚠ %s 是 Fake IP (Clash/Surge 透明代理)\n", ip)
				} else if parsed.IsPrivate() {
					fmt.Printf("  ⚠ %s 是内网 IP\n", ip)
				} else {
					fmt.Printf("  ✓ %s 看起来是正常的公网 IP\n", ip)
				}
			}
		}
	}
	fmt.Println()

	// Step 2: TCP 连接测试
	fmt.Println("--- Step 2: TCP 连接测试 auth.openai.com:443 ---")
	tcpStart := time.Now()
	conn, err := net.DialTimeout("tcp", authHost+":443", 10*time.Second)
	tcpDuration := time.Since(tcpStart)
	if err != nil {
		fmt.Printf("  TCP 连接失败 (%v): %v\n", tcpDuration, err)
	} else {
		fmt.Printf("  TCP 连接成功 (耗时 %v)\n", tcpDuration)
		conn.Close()
	}
	fmt.Println()

	// Step 3: TLS 握手测试
	fmt.Println("--- Step 3: TLS 握手测试 ---")
	tlsStart := time.Now()
	tlsConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp",
		authHost+":443",
		&tls.Config{ServerName: authHost},
	)
	tlsDuration := time.Since(tlsStart)
	if err != nil {
		fmt.Printf("  TLS 握手失败 (%v): %v\n", tlsDuration, err)
	} else {
		state := tlsConn.ConnectionState()
		fmt.Printf("  TLS 握手成功 (耗时 %v)\n", tlsDuration)
		fmt.Printf("  TLS 版本: %x, 协商协议: %s\n", state.Version, state.NegotiatedProtocol)
		if len(state.PeerCertificates) > 0 {
			cert := state.PeerCertificates[0]
			fmt.Printf("  证书 CN: %s\n", cert.Subject.CommonName)
			fmt.Printf("  证书 SAN: %v\n", cert.DNSNames)
			fmt.Printf("  证书有效期: %s ~ %s\n", cert.NotBefore.Format("2006-01-02"), cert.NotAfter.Format("2006-01-02"))
		}
		tlsConn.Close()
	}
	fmt.Println()

	// Step 4: HTTP 请求测试 (模拟 token refresh，用空 body 触发错误看返回)
	fmt.Println("--- Step 4: HTTP POST 测试 (模拟 token refresh) ---")
	client := &http.Client{Timeout: 15 * time.Second}

	// 发送一个格式正确但凭据无效的 token refresh 请求
	body := `{"client_id":"app_EMoamEEZ73f0CkXaXp7hrann","grant_type":"refresh_token","refresh_token":"fake_test_token_for_connectivity_check"}`
	req, _ := http.NewRequest("POST", tokenURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	httpStart := time.Now()
	resp, err := client.Do(req)
	httpDuration := time.Since(httpStart)
	if err != nil {
		fmt.Printf("  HTTP 请求失败 (%v): %v\n", httpDuration, err)
	} else {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Printf("  HTTP 响应状态: %d (耗时 %v)\n", resp.StatusCode, httpDuration)
		fmt.Printf("  响应 body: %s\n", string(respBody))

		// 分析
		if resp.StatusCode == 401 || resp.StatusCode == 400 || resp.StatusCode == 403 {
			respStr := string(respBody)
			if strings.Contains(respStr, "invalid_grant") || strings.Contains(respStr, "invalid_client") || strings.Contains(respStr, "refresh_token") {
				fmt.Println("  ✓ 连通性正常! OpenAI 正常响应了 token 请求 (只是凭据无效)")
			} else if strings.Contains(respStr, "unsupported_country_region_territory") {
				fmt.Println("  ✗ 地域限制! 当前出口 IP 被 OpenAI 拒绝")
			} else {
				fmt.Printf("  ? 收到 %d 响应，请检查具体内容\n", resp.StatusCode)
			}
		} else if resp.StatusCode == 200 {
			fmt.Println("  ✓ 连通性正常!")
		}
	}
	fmt.Println()

	// Step 5: 检查出口 IP
	fmt.Println("--- Step 5: 检查出口 IP ---")
	ipResp, err := client.Get("https://api.ipify.org?format=text")
	if err != nil {
		fmt.Printf("  获取出口 IP 失败: %v\n", err)
	} else {
		defer ipResp.Body.Close()
		ipBody, _ := io.ReadAll(ipResp.Body)
		fmt.Printf("  出口 IP: %s\n", strings.TrimSpace(string(ipBody)))
	}

	fmt.Println("\n=== 诊断完成 ===")
}
