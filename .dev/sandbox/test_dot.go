// go run test_dot.go 222jpsafweqasfregthjgfbsdtgredgsdgwef24136547346452.chylink.xyz
package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func main() {
	domain := "222jpsafweqasfregthjgfbsdtgredgsdgwef24136547346452.chylink.xyz"
	if len(os.Args) > 1 {
		domain = os.Args[1]
	}

	// 连接 DoT 服务器
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp",
		"112.74.48.57:10853",
		&tls.Config{InsecureSkipVerify: true},
	)
	if err != nil {
		fmt.Printf("连接失败: %v\n", err)
		return
	}
	defer conn.Close()

	// 构建 DNS 查询
	var msg dnsmessage.Message
	msg.Header.ID = 1234
	msg.Header.RecursionDesired = true
	msg.Questions = []dnsmessage.Question{{
		Name:  dnsmessage.MustNewName(domain + "."),
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
	}}

	packed, _ := msg.Pack()

	// DNS over TLS 需要先发送长度
	length := make([]byte, 2)
	length[0] = byte(len(packed) >> 8)
	length[1] = byte(len(packed))
	conn.Write(length)
	conn.Write(packed)

	// 读取响应
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	respLen := make([]byte, 2)
	conn.Read(respLen)
	respSize := int(respLen[0])<<8 | int(respLen[1])
	resp := make([]byte, respSize)
	conn.Read(resp)

	// 解析响应
	var respMsg dnsmessage.Message
	respMsg.Unpack(resp)

	fmt.Printf("查询域名: %s\n", domain)
	fmt.Printf("DNS服务器: tls://112.74.48.57:10853\n")
	fmt.Println("结果:")
	for _, ans := range respMsg.Answers {
		if ans.Header.Type == dnsmessage.TypeA {
			ip := ans.Body.(*dnsmessage.AResource).A
			fmt.Printf("  A记录: %d.%d.%d.%d\n", ip[0], ip[1], ip[2], ip[3])
		}
	}
}
