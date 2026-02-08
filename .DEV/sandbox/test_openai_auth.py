#!/usr/bin/env python3
"""
测试到 auth.openai.com 的连通性，诊断 codex token refresh 失败原因
Usage: python3 test_openai_auth.py
"""
import socket
import ssl
import json
import time
import urllib.request
import urllib.error
from datetime import datetime

AUTH_HOST = "auth.openai.com"
TOKEN_URL = "https://auth.openai.com/oauth/token"

def step_dns():
    print("--- Step 1: DNS 解析 auth.openai.com ---")
    try:
        results = socket.getaddrinfo(AUTH_HOST, 443, socket.AF_INET)
        ips = list(set(r[4][0] for r in results))
        print(f"  解析结果: {ips}")
        for ip in ips:
            if ip.startswith("198.18.") or ip.startswith("198.19."):
                print(f"  ⚠ {ip} 是 Fake IP (Clash/Surge 透明代理)")
            elif ip.startswith("10.") or ip.startswith("192.168.") or ip.startswith("172."):
                print(f"  ⚠ {ip} 是内网 IP")
            else:
                print(f"  ✓ {ip} 看起来是正常的公网 IP")
        return ips
    except Exception as e:
        print(f"  DNS 解析失败: {e}")
        return []

def step_tcp():
    print("\n--- Step 2: TCP 连接测试 auth.openai.com:443 ---")
    start = time.time()
    try:
        sock = socket.create_connection((AUTH_HOST, 443), timeout=10)
        duration = time.time() - start
        print(f"  TCP 连接成功 (耗时 {duration:.3f}s)")
        sock.close()
        return True
    except Exception as e:
        duration = time.time() - start
        print(f"  TCP 连接失败 ({duration:.3f}s): {e}")
        return False

def step_tls():
    print("\n--- Step 3: TLS 握手测试 ---")
    start = time.time()
    try:
        ctx = ssl.create_default_context()
        sock = socket.create_connection((AUTH_HOST, 443), timeout=10)
        tls_sock = ctx.wrap_socket(sock, server_hostname=AUTH_HOST)
        duration = time.time() - start
        
        cert = tls_sock.getpeercert()
        print(f"  TLS 握手成功 (耗时 {duration:.3f}s)")
        print(f"  TLS 版本: {tls_sock.version()}")
        
        # 证书信息
        subject = dict(x[0] for x in cert.get('subject', ()))
        print(f"  证书 CN: {subject.get('commonName', 'N/A')}")
        sans = [entry[1] for entry in cert.get('subjectAltName', ()) if entry[0] == 'DNS']
        if sans:
            print(f"  证书 SAN: {sans[:5]}{'...' if len(sans) > 5 else ''}")
        print(f"  证书有效期: {cert.get('notBefore', 'N/A')} ~ {cert.get('notAfter', 'N/A')}")
        
        tls_sock.close()
        return True
    except Exception as e:
        duration = time.time() - start
        print(f"  TLS 握手失败 ({duration:.3f}s): {e}")
        return False

def step_http_token():
    print("\n--- Step 4: HTTP POST 测试 (模拟 token refresh) ---")
    body = json.dumps({
        "client_id": "app_EMoamEEZ73f0CkXaXp7hrann",
        "grant_type": "refresh_token",
        "refresh_token": "fake_test_token_for_connectivity_check"
    }).encode()
    
    req = urllib.request.Request(
        TOKEN_URL,
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST"
    )
    
    start = time.time()
    try:
        resp = urllib.request.urlopen(req, timeout=15)
        duration = time.time() - start
        resp_body = resp.read().decode()
        print(f"  HTTP 响应状态: {resp.status} (耗时 {duration:.3f}s)")
        print(f"  响应 body: {resp_body[:500]}")
        print("  ✓ 连通性正常!")
    except urllib.error.HTTPError as e:
        duration = time.time() - start
        resp_body = e.read().decode()
        print(f"  HTTP 响应状态: {e.code} (耗时 {duration:.3f}s)")
        print(f"  响应 body: {resp_body[:500]}")
        
        if e.code in (400, 401):
            if "unsupported_country_region_territory" in resp_body:
                print("  ✗ 地域限制! 当前出口 IP 被 OpenAI 拒绝")
            else:
                print("  ✓ 连通性正常! OpenAI 正常响应了请求 (凭据无效是预期的)")
        elif e.code == 403:
            if "unsupported_country_region_territory" in resp_body:
                print("  ✗ 地域限制! 当前出口 IP 被 OpenAI 拒绝")
            else:
                print(f"  ? 收到 403，请检查具体内容")
        else:
            print(f"  ? 收到 {e.code}，请检查具体内容")
    except Exception as e:
        duration = time.time() - start
        print(f"  HTTP 请求失败 ({duration:.3f}s): {e}")

def step_exit_ip():
    print("\n--- Step 5: 检查出口 IP ---")
    try:
        resp = urllib.request.urlopen("https://api.ipify.org?format=text", timeout=10)
        ip = resp.read().decode().strip()
        print(f"  出口 IP: {ip}")
    except Exception as e:
        print(f"  获取出口 IP 失败: {e}")
        # 备用
        try:
            resp = urllib.request.urlopen("https://ifconfig.me", timeout=10)
            ip = resp.read().decode().strip()
            print(f"  出口 IP (备用): {ip}")
        except Exception as e2:
            print(f"  备用也失败: {e2}")

def step_google_oauth():
    print("\n--- Step 6: Google OAuth 连通性 (antigravity 用) ---")
    start = time.time()
    try:
        sock = socket.create_connection(("oauth2.googleapis.com", 443), timeout=10)
        duration = time.time() - start
        print(f"  TCP 到 oauth2.googleapis.com:443 成功 (耗时 {duration:.3f}s)")
        sock.close()
    except Exception as e:
        duration = time.time() - start
        print(f"  TCP 到 oauth2.googleapis.com:443 失败 ({duration:.3f}s): {e}")

if __name__ == "__main__":
    print("=== OpenAI Auth 连通性诊断 ===")
    print(f"时间: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}\n")
    
    step_dns()
    step_tcp()
    step_tls()
    step_http_token()
    step_exit_ip()
    step_google_oauth()
    
    print("\n=== 诊断完成 ===")
