// cd c:\Users\yuanyuan\Documents\workspace\CLIProxyAPI\_ai_sandbox
// go run test_ss_dot.go

package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"time"

	sscore "github.com/shadowsocks/go-shadowsocks2/core"
	"golang.org/x/net/dns/dnsmessage"
)

const (
	ssURL   = "ss://chacha20-ietf-poly1305:aSiTIFGjKU7RYoH5@asfgrhtyjergwerfwqrdqewfg124325t12354.chylink.xyz:25116"
	dotDNS  = "112.74.48.57:10853"
	testURL = "https://api.ipify.org?format=text"
)

func main() {
	fmt.Println("=== SS + DoT DNS 测试 ===")
	fmt.Printf("SS URL: %s\n", ssURL)
	fmt.Printf("DoT DNS: %s\n\n", dotDNS)

	// Step 1: 测试 DoT DNS 解析
	fmt.Println("Step 1: 测试 DoT DNS 解析 SS 服务器域名...")
	ssHost := "asfgrhtyjergwerfwqrdqewfg124325t12354.chylink.xyz"
	resolvedIP, err := resolveWithDoT(ssHost, dotDNS)
	if err != nil {
		fmt.Printf("❌ DoT DNS 解析失败: %v\n", err)
		return
	}
	fmt.Printf("✅ DoT DNS 解析成功: %s -> %s\n\n", ssHost, resolvedIP)

	// Step 2: 测试连接 SS 服务器
	ssServer := resolvedIP + ":25116"
	fmt.Printf("Step 2: 测试连接 SS 服务器 (%s)...\n", ssServer)

	cipher, err := sscore.PickCipher("chacha20-ietf-poly1305", nil, "aSiTIFGjKU7RYoH5")
	if err != nil {
		fmt.Printf("❌ 创建 cipher 失败: %v\n", err)
		return
	}

	// 创建 HTTP 客户端使用 SS 代理
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			rawConn, err := net.DialTimeout("tcp", ssServer, 10*time.Second)
			if err != nil {
				return nil, fmt.Errorf("connect to SS server: %w", err)
			}
			ssConn := cipher.StreamConn(rawConn)
			if err := writeSSTargetAddr(ssConn, addr); err != nil {
				rawConn.Close()
				return nil, fmt.Errorf("write target addr: %w", err)
			}
			return ssConn, nil
		},
	}

	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	fmt.Printf("Step 3: 通过 SS 代理访问 %s...\n", testURL)
	resp, err := client.Get(testURL)
	if err != nil {
		fmt.Printf("❌ HTTP 请求失败: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body := make([]byte, 1024)
	n, _ := resp.Body.Read(body)
	fmt.Printf("✅ 请求成功! 出口 IP: %s\n", string(body[:n]))
}

func resolveWithDoT(domain, dnsServer string) (string, error) {
	fmt.Printf("   连接 DoT 服务器 %s...\n", dnsServer)
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp",
		dnsServer,
		&tls.Config{InsecureSkipVerify: true},
	)
	if err != nil {
		return "", fmt.Errorf("connect to DoT: %w", err)
	}
	defer conn.Close()
	fmt.Println("   DoT TLS 连接成功")

	// 构建 DNS 查询
	var msg dnsmessage.Message
	msg.Header.ID = uint16(time.Now().UnixNano() & 0xFFFF)
	msg.Header.RecursionDesired = true
	msg.Questions = []dnsmessage.Question{{
		Name:  dnsmessage.MustNewName(domain + "."),
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
	}}

	packed, err := msg.Pack()
	if err != nil {
		return "", fmt.Errorf("pack DNS query: %w", err)
	}

	// 发送查询 (length prefix + data)
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(packed)))
	conn.Write(length)
	conn.Write(packed)
	fmt.Println("   DNS 查询已发送")

	// 读取响应
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	respLen := make([]byte, 2)
	if _, err := conn.Read(respLen); err != nil {
		return "", fmt.Errorf("read response length: %w", err)
	}
	respSize := int(binary.BigEndian.Uint16(respLen))
	resp := make([]byte, respSize)
	if _, err := conn.Read(resp); err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	// 解析响应
	var respMsg dnsmessage.Message
	if err := respMsg.Unpack(resp); err != nil {
		return "", fmt.Errorf("unpack DNS response: %w", err)
	}

	for _, ans := range respMsg.Answers {
		if ans.Header.Type == dnsmessage.TypeA {
			aRecord := ans.Body.(*dnsmessage.AResource)
			return fmt.Sprintf("%d.%d.%d.%d", aRecord.A[0], aRecord.A[1], aRecord.A[2], aRecord.A[3]), nil
		}
	}

	return "", fmt.Errorf("no A record found")
}

func writeSSTargetAddr(conn net.Conn, addr string) error {
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)

	var buf []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			buf = make([]byte, 1+4+2)
			buf[0] = 0x01
			copy(buf[1:5], ip4)
		} else {
			buf = make([]byte, 1+16+2)
			buf[0] = 0x04
			copy(buf[1:17], ip.To16())
		}
	} else {
		buf = make([]byte, 1+1+len(host)+2)
		buf[0] = 0x03
		buf[1] = byte(len(host))
		copy(buf[2:], host)
	}
	binary.BigEndian.PutUint16(buf[len(buf)-2:], uint16(port))
	_, err := conn.Write(buf)
	return err
}
