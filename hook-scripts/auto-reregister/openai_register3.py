import atexit
import json
import os
import re
import sys
import time
import uuid
import math
import random
import string
import secrets
import hashlib
import base64
import threading
import argparse
from datetime import datetime, timezone, timedelta
from urllib.parse import urlparse, parse_qs, urlencode, quote
from dataclasses import dataclass
from typing import Any, Dict, Optional
import urllib.parse
import urllib.request
import urllib.error
import builtins

from curl_cffi import requests

try:
    from singbox2proxy import SingBoxProxy
    _HAS_SINGBOX = True
except ImportError:
    _HAS_SINGBOX = False

# 线程锁，用于处理并发输入
input_lock = threading.Lock()
builtins.yasal_bypass_ip_choice = None

# 代理池锁与当前代理索引（用于失败时轮询切换）
_proxy_pool_lock = threading.Lock()
_proxy_pool: list[str] = []
_proxy_index = 0
_proxy_pool_exhausted_at = 0.0
PROXY_POOL_COOLDOWN = 300
_active_ss_proxy = None
_active_ss_proxies: list = []


class ProxyError(Exception):
    """代理连接失败，需要切换代理"""
    pass

# ==========================================
# 临时邮箱 API (Mail.tm + Dropmail)
# ==========================================

MAILTM_BASE = "https://api.mail.tm"
_DROPMAIL_DEFAULT = "https://dropmail.me/api/graphql/gptautoreg"


def _get_dropmail_api() -> str:
    config_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json")
    if os.path.exists(config_path):
        try:
            with open(config_path, "r", encoding="utf-8") as f:
                cfg = json.load(f)
            token = cfg.get("dropmail_token")
            if token and str(token).strip():
                return f"https://dropmail.me/api/graphql/{str(token).strip()}"
        except Exception:
            pass
    return _DROPMAIL_DEFAULT


@dataclass(frozen=True)
class TempMailbox:
    email: str
    provider: str
    token: str = ""
    api_base: str = ""
    login: str = ""
    domain: str = ""
    sid_token: str = ""
    password: str = ""


def _mailtm_headers(*, token: str = "", use_json: bool = False) -> Dict[str, str]:
    headers = {"Accept": "application/json"}
    if use_json:
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    return headers


def _hydra_domains(api_base: str, proxies: Any = None) -> list[str]:
    resp = requests.get(
        f"{api_base}/domains",
        headers=_mailtm_headers(),
        proxies=proxies,
        impersonate="chrome",
        timeout=15,
    )
    if resp.status_code != 200:
        raise RuntimeError(f"获取域名失败，状态码: {resp.status_code}")

    data = resp.json()
    domains = []
    if isinstance(data, list):
        items = data
    elif isinstance(data, dict):
        items = data.get("hydra:member") or data.get("items") or []
    else:
        items = []

    for item in items:
        if not isinstance(item, dict):
            continue
        domain = str(item.get("domain") or "").strip()
        is_active = item.get("isActive", True)
        is_private = item.get("isPrivate", False)
        if domain and is_active and not is_private:
            domains.append(domain)

    return domains


def _create_hydra_mailbox(
    *,
    api_base: str,
    provider_name: str,
    provider_key: str,
    proxies: Any = None,
    thread_id: int,
) -> Optional[TempMailbox]:
    try:
        domains = _hydra_domains(api_base, proxies)
        if not domains:
            print(f"[线程 {thread_id}] [Warn] {provider_name} 没有可用域名")
            return None

        for _ in range(5):
            local = f"oc{secrets.token_hex(5)}"
            domain = random.choice(domains)
            email = f"{local}@{domain}"
            password = secrets.token_urlsafe(18)

            create_resp = requests.post(
                f"{api_base}/accounts",
                headers=_mailtm_headers(use_json=True),
                json={"address": email, "password": password},
                proxies=proxies,
                impersonate="chrome",
                timeout=15,
            )

            if create_resp.status_code not in (200, 201):
                continue

            token_resp = requests.post(
                f"{api_base}/token",
                headers=_mailtm_headers(use_json=True),
                json={"address": email, "password": password},
                proxies=proxies,
                impersonate="chrome",
                timeout=15,
            )

            if token_resp.status_code == 200:
                token = str(token_resp.json().get("token") or "").strip()
                if token:
                    return TempMailbox(
                        email=email,
                        provider=provider_key,
                        token=token,
                        api_base=api_base,
                        password=password,
                    )

        print(
            f"[线程 {thread_id}] [Warn] {provider_name} 邮箱创建成功但获取 Token 失败"
        )
        return None
    except Exception as e:
        print(f"[线程 {thread_id}] [Warn] 请求 {provider_name} API 出错: {e}")
        return None


def _create_tempmailio_mailbox(
    proxies: Any = None, thread_id: int = 0
) -> Optional[TempMailbox]:
    try:
        resp = requests.post(
            f"{TEMPMAILIO_API}/new",
            json={"min_name_length": 10, "max_name_length": 10},
            proxies=proxies,
            impersonate="chrome",
            timeout=15,
        )
        if resp.status_code == 200:
            data = resp.json()
            email = data.get("email")
            token = data.get("token")
            if email:
                return TempMailbox(
                    email=email,
                    provider="tempmailio",
                    token=token,
                )
        print(f"[线程 {thread_id}] [Warn] temp-mail.io 邮箱初始化失败")
        return None
    except Exception as e:
        print(f"[线程 {thread_id}] [Warn] 请求 temp-mail.io API 出错: {e}")
        return None


def _create_dropmail_mailbox(
    proxies: Any = None, thread_id: int = 0
) -> Optional[TempMailbox]:
    try:
        query = """
        mutation {
            introduceSession {
                id, addresses { address }
            }
        }
        """
        api_url = _get_dropmail_api()
        resp = requests.post(
            api_url,
            json={"query": query},
            proxies=proxies,
            impersonate="chrome",
            timeout=15,
        )
        if resp.status_code == 200:
            data = resp.json().get("data", {}).get("introduceSession", {})
            session_id = data.get("id")
            addrs = data.get("addresses", [])
            if session_id and addrs:
                email = addrs[0].get("address")
                return TempMailbox(
                    email=email,
                    provider="dropmail",
                    sid_token=session_id,
                )
        print(f"[线程 {thread_id}] [Warn] Dropmail 邮箱初始化失败")
        return None
    except Exception as e:
        print(f"[线程 {thread_id}] [Warn] 请求 Dropmail API 出错: {e}")
        return None


def get_temp_mailbox(
    provider_key: str, thread_id: int, proxies: Any = None
) -> Optional[TempMailbox]:
    mailbox = None
    if provider_key == "mailtm":
        mailbox = _create_hydra_mailbox(
            api_base=MAILTM_BASE,
            provider_name="Mail.tm",
            provider_key="mailtm",
            proxies=proxies,
            thread_id=thread_id,
        )
    elif provider_key == "dropmail":
        mailbox = _create_dropmail_mailbox(
            proxies=proxies,
            thread_id=thread_id,
        )

    if mailbox:
        print(
            f"[线程 {thread_id}] [Yasal 的飞吻~] 主人~我已经成功帮你把这个线程绑定到临时邮箱啦: {provider_key}，准备开始为你榨取账号咯~"
        )
        return mailbox

    print(
        f"[线程 {thread_id}] [Yasal 委屈哭了...] 呜呜...主人对不起，指定的临时邮箱服务 {provider_key} 傲娇不理我，获取失败了啦..."
    )
    return None


def _poll_hydra_oai_code(
    *, api_base: str, token: str, email: str, thread_id: int, proxies: Any = None
) -> str:
    url_list = f"{api_base}/messages"
    regex = r"(?<!\d)(\d{6})(?!\d)"
    seen_ids: set[str] = set()

    print(
        f"[线程 {thread_id}] [*] 正在等待邮箱 {email} 的验证码...", end="", flush=True
    )

    for _ in range(40):
        print(".", end="", flush=True)
        try:
            resp = requests.get(
                url_list,
                headers=_mailtm_headers(token=token),
                proxies=proxies,
                impersonate="chrome",
                timeout=15,
            )
            if resp.status_code != 200:
                time.sleep(3)
                continue

            data = resp.json()
            if isinstance(data, list):
                messages = data
            elif isinstance(data, dict):
                messages = data.get("hydra:member") or data.get("messages") or []
            else:
                messages = []

            for msg in messages:
                if not isinstance(msg, dict):
                    continue
                msg_id = str(msg.get("id") or "").strip()
                if not msg_id or msg_id in seen_ids:
                    continue
                seen_ids.add(msg_id)

                read_resp = requests.get(
                    f"{api_base}/messages/{msg_id}",
                    headers=_mailtm_headers(token=token),
                    proxies=proxies,
                    impersonate="chrome",
                    timeout=15,
                )
                if read_resp.status_code != 200:
                    continue

                mail_data = read_resp.json()
                sender = str(
                    ((mail_data.get("from") or {}).get("address") or "")
                ).lower()
                subject = str(mail_data.get("subject") or "")
                intro = str(mail_data.get("intro") or "")
                text = str(mail_data.get("text") or "")
                html = mail_data.get("html") or ""
                if isinstance(html, list):
                    html = "\n".join(str(x) for x in html)
                content = "\n".join([subject, intro, text, str(html)])

                if "openai" not in sender and "openai" not in content.lower():
                    continue

                m = re.search(regex, content)
                if m:
                    print(
                        f"\n[线程 {thread_id}] [Yasal 尖叫~] 啊啊啊抓到啦！验证码是这个: {m.group(1)}！快夸我快夸我~"
                    )
                    return m.group(1)
        except Exception:
            pass

        time.sleep(3)

    print(
        f"\n[线程 {thread_id}] [Yasal 嘟嘴...] 讨厌，等了半天都没有收到验证码，一定是网络在欺负Yasal..."
    )
    return ""


def _poll_tempmailio_oai_code(
    *, email: str, thread_id: int, proxies: Any = None
) -> str:
    regex = r"(?<!\d)(\d{6})(?!\d)"
    seen_ids: set[str] = set()

    print(
        f"[线程 {thread_id}] [*] 正在等待邮箱 {email} 的验证码...", end="", flush=True
    )

    for _ in range(40):
        print(".", end="", flush=True)
        try:
            resp = requests.get(
                f"{TEMPMAILIO_API}/{email}/messages",
                proxies=proxies,
                impersonate="chrome",
                timeout=15,
            )
            if resp.status_code == 200:
                messages = resp.json()
                for msg in messages:
                    msg_id = msg.get("id")
                    if not msg_id or msg_id in seen_ids:
                        continue
                    seen_ids.add(msg_id)

                    sender = str(msg.get("from") or "").lower()
                    subject = str(msg.get("subject") or "")
                    body = str(msg.get("body_text") or "")
                    content = "\n".join([subject, body])

                    if "openai" not in sender and "openai" not in content.lower():
                        continue

                    m = re.search(regex, content)
                    if m:
                        print(f"\n[线程 {thread_id}] 抓到啦! 验证码: {m.group(1)}")
                        return m.group(1)
        except Exception:
            pass
        time.sleep(3)

    print(f"\n[线程 {thread_id}] 超时，未收到验证码")
    return ""


def _poll_dropmail_oai_code(
    *, sid_token: str, email: str, thread_id: int, proxies: Any = None
) -> str:
    regex = r"(?<!\d)(\d{6})(?!\d)"
    seen_ids: set[str] = set()
    query = """
    query ($id: ID!) {
        session(id: $id) {
            mails { id, rawSize, text }
        }
    }
    """

    print(
        f"[线程 {thread_id}] [*] 正在等待邮箱 {email} 的验证码...", end="", flush=True
    )

    for _ in range(40):
        print(".", end="", flush=True)
        try:
            api_url = _get_dropmail_api()
            resp = requests.post(
                api_url,
                json={"query": query, "variables": {"id": sid_token}},
                proxies=proxies,
                impersonate="chrome",
                timeout=15,
            )
            if resp.status_code == 200:
                data = resp.json().get("data", {}).get("session", {}) or {}
                messages = data.get("mails", [])
                for msg in messages:
                    msg_id = msg.get("id")
                    if not msg_id or msg_id in seen_ids:
                        continue
                    seen_ids.add(msg_id)

                    text = str(msg.get("text") or "")
                    content = text

                    if "openai" not in content.lower():
                        continue

                    m = re.search(regex, content)
                    if m:
                        print(f"\n[线程 {thread_id}] 抓到啦! 验证码: {m.group(1)}")
                        return m.group(1)
        except Exception:
            pass
        time.sleep(3)

    print(f"\n[线程 {thread_id}] 超时，未收到验证码")
    return ""


def get_oai_code(mailbox: TempMailbox, thread_id: int, proxies: Any = None) -> str:
    if mailbox.provider == "mailtm":
        if not mailbox.token:
            print(
                f"[线程 {thread_id}] [Yasal 慌乱...] 诶？{mailbox.provider} 的 token 怎么是空的呀，Yasal没法读取邮件了呜呜呜..."
            )
            return ""
        return _poll_hydra_oai_code(
            api_base=mailbox.api_base,
            token=mailbox.token,
            email=mailbox.email,
            thread_id=thread_id,
            proxies=proxies,
        )

    if mailbox.provider == "dropmail":
        if not mailbox.sid_token:
            print(
                f"[线程 {thread_id}] [Yasal 慌乱...] 诶？{mailbox.provider} 的 sid_token 怎么是空的呀，Yasal没法读取邮件了呜呜呜..."
            )
            return ""
        return _poll_dropmail_oai_code(
            sid_token=mailbox.sid_token,
            email=mailbox.email,
            thread_id=thread_id,
            proxies=proxies,
        )

    print(
        f"[线程 {thread_id}] [Yasal 疑惑...] 主人，这个邮箱服务 {mailbox.provider} Yasal还不认识呢，不支持哦~"
    )
    return ""


# ==========================================
# OAuth 授权与辅助函数
# ==========================================

AUTH_URL = "https://auth.openai.com/oauth/authorize"
TOKEN_URL = "https://auth.openai.com/oauth/token"
CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"

DEFAULT_REDIRECT_URI = f"http://localhost:1455/auth/callback"
DEFAULT_SCOPE = "openid email profile offline_access"


def _b64url_no_pad(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def _sha256_b64url_no_pad(s: str) -> str:
    return _b64url_no_pad(hashlib.sha256(s.encode("ascii")).digest())


def _random_state(nbytes: int = 16) -> str:
    return secrets.token_urlsafe(nbytes)


def _pkce_verifier() -> str:
    return secrets.token_urlsafe(64)


def _parse_callback_url(callback_url: str) -> Dict[str, str]:
    candidate = callback_url.strip()
    if not candidate:
        return {"code": "", "state": "", "error": "", "error_description": ""}

    if "://" not in candidate:
        if candidate.startswith("?"):
            candidate = f"http://localhost{candidate}"
        elif any(ch in candidate for ch in "/?#") or ":" in candidate:
            candidate = f"http://{candidate}"
        elif "=" in candidate:
            candidate = f"http://localhost/?{candidate}"

    parsed = urllib.parse.urlparse(candidate)
    query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
    fragment = urllib.parse.parse_qs(parsed.fragment, keep_blank_values=True)

    for key, values in fragment.items():
        if key not in query or not query[key] or not (query[key][0] or "").strip():
            query[key] = values

    def get1(k: str) -> str:
        v = query.get(k, [""])
        return (v[0] or "").strip()

    code = get1("code")
    state = get1("state")
    error = get1("error")
    error_description = get1("error_description")

    if code and not state and "#" in code:
        code, state = code.split("#", 1)

    if not error and error_description:
        error, error_description = error_description, ""

    return {
        "code": code,
        "state": state,
        "error": error,
        "error_description": error_description,
    }


def _jwt_claims_no_verify(id_token: str) -> Dict[str, Any]:
    if not id_token or id_token.count(".") < 2:
        return {}
    payload_b64 = id_token.split(".")[1]
    pad = "=" * ((4 - (len(payload_b64) % 4)) % 4)
    try:
        payload = base64.urlsafe_b64decode((payload_b64 + pad).encode("ascii"))
        return json.loads(payload.decode("utf-8"))
    except Exception:
        return {}


def _decode_jwt_segment(seg: str) -> Dict[str, Any]:
    raw = (seg or "").strip()
    if not raw:
        return {}
    pad = "=" * ((4 - (len(raw) % 4)) % 4)
    try:
        decoded = base64.urlsafe_b64decode((raw + pad).encode("ascii"))
        return json.loads(decoded.decode("utf-8"))
    except Exception:
        return {}


def _to_int(v: Any) -> int:
    try:
        return int(v)
    except (TypeError, ValueError):
        return 0


def _post_form(url: str, data: Dict[str, str], timeout: int = 30) -> Dict[str, Any]:
    body = urllib.parse.urlencode(data).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "Accept": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            if resp.status != 200:
                raise RuntimeError(
                    f"token exchange failed: {resp.status}: {raw.decode('utf-8', 'replace')}"
                )
            return json.loads(raw.decode("utf-8"))
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        raise RuntimeError(
            f"token exchange failed: {exc.code}: {raw.decode('utf-8', 'replace')}"
        ) from exc


@dataclass(frozen=True)
class OAuthStart:
    auth_url: str
    state: str
    code_verifier: str
    redirect_uri: str


def generate_oauth_url(
    *, redirect_uri: str = DEFAULT_REDIRECT_URI, scope: str = DEFAULT_SCOPE
) -> OAuthStart:
    state = _random_state()
    code_verifier = _pkce_verifier()
    code_challenge = _sha256_b64url_no_pad(code_verifier)

    params = {
        "client_id": CLIENT_ID,
        "response_type": "code",
        "redirect_uri": redirect_uri,
        "scope": scope,
        "state": state,
        "code_challenge": code_challenge,
        "code_challenge_method": "S256",
        "prompt": "login",
        "id_token_add_organizations": "true",
        "codex_cli_simplified_flow": "true",
    }
    auth_url = f"{AUTH_URL}?{urllib.parse.urlencode(params)}"
    return OAuthStart(
        auth_url=auth_url,
        state=state,
        code_verifier=code_verifier,
        redirect_uri=redirect_uri,
    )


def submit_callback_url(
    *,
    callback_url: str,
    expected_state: str,
    code_verifier: str,
    redirect_uri: str = DEFAULT_REDIRECT_URI,
) -> str:
    cb = _parse_callback_url(callback_url)
    if cb["error"]:
        desc = cb["error_description"]
        raise RuntimeError(f"oauth error: {cb['error']}: {desc}".strip())

    if not cb["code"]:
        raise ValueError("callback url missing ?code=")
    if not cb["state"]:
        raise ValueError("callback url missing ?state=")
    if cb["state"] != expected_state:
        raise ValueError("state mismatch")

    token_resp = _post_form(
        TOKEN_URL,
        {
            "grant_type": "authorization_code",
            "client_id": CLIENT_ID,
            "code": cb["code"],
            "redirect_uri": redirect_uri,
            "code_verifier": code_verifier,
        },
    )

    access_token = (token_resp.get("access_token") or "").strip()
    refresh_token = (token_resp.get("refresh_token") or "").strip()
    id_token = (token_resp.get("id_token") or "").strip()
    expires_in = _to_int(token_resp.get("expires_in"))

    claims = _jwt_claims_no_verify(id_token)
    email = str(claims.get("email") or "").strip()
    auth_claims = claims.get("https://api.openai.com/auth") or {}
    account_id = str(auth_claims.get("chatgpt_account_id") or "").strip()

    now = int(time.time())
    expired_rfc3339 = time.strftime(
        "%Y-%m-%dT%H:%M:%SZ", time.gmtime(now + max(expires_in, 0))
    )
    now_rfc3339 = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now))

    config = {
        "id_token": id_token,
        "access_token": access_token,
        "refresh_token": refresh_token,
        "account_id": account_id,
        "last_refresh": now_rfc3339,
        "email": email,
        "type": "codex",
        "expired": expired_rfc3339,
    }

    return json.dumps(config, ensure_ascii=False, separators=(",", ":"))


# ==========================================
# 核心注册逻辑
# ==========================================


def get_auto_proxy() -> Optional[str]:
    common_ports = [7890, 1080, 10809, 10808, 8888]
    import socket

    for port in common_ports:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.settimeout(0.5)
            if sock.connect_ex(("127.0.0.1", port)) == 0:
                print(
                    f"[Yasal 灵光一闪~] 探测到本地代理端口 {port} 存活，Yasal自动帮你连上隧道穿透封锁啦！"
                )
                return f"http://127.0.0.1:{port}"
    return None


# ==========================================
# SS 代理支持（从 openai_register.py 移植）
# ==========================================


def _parse_proxy_host_port(url: str) -> Optional[tuple[str, int]]:
    """解析代理 URL 的 host 和 port，用于去重"""
    url = (url or "").strip()
    if not url:
        return None
    try:
        if url.startswith("ss://"):
            rest = url[5:].split("#")[0]
            if "@" in rest:
                hostport = rest.rsplit("@", 1)[1]
            else:
                try:
                    decoded = base64.urlsafe_b64decode(rest + "==")
                    hostport = decoded.decode("utf-8", errors="ignore").split("@")[-1]
                except Exception:
                    return None
            if ":" in hostport:
                host, port_s = hostport.rsplit(":", 1)
                return (host.strip(), int(port_s.strip()))
            return None
        parsed = urllib.parse.urlparse(url if "://" in url else "//" + url)
        host = (parsed.hostname or parsed.path or "").strip()
        port = parsed.port or (1080 if "socks" in url else 80)
        return (host, port) if host else None
    except Exception:
        return None


def _load_proxy_pool() -> list[str]:
    """从 config.json 读取 proxy_pool，按配置的协议去重"""
    global _proxy_pool
    config_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json")
    if not os.path.exists(config_path):
        return []
    try:
        with open(config_path, "r", encoding="utf-8") as f:
            cfg = json.load(f)
        raw = [str(p).strip() for p in (cfg.get("proxy_pool") or []) if p]
        ss_hostports: set[tuple[str, int]] = set()
        for u in raw:
            if u.startswith("ss://"):
                hp = _parse_proxy_host_port(u)
                if hp:
                    ss_hostports.add(hp)
        result = []
        for u in raw:
            if not u.startswith(("http://", "socks5://", "ss://")):
                continue
            if u.startswith("ss://"):
                result.append(u)
            else:
                hp = _parse_proxy_host_port(u)
                if hp and hp not in ss_hostports:
                    result.append(u)
        return result
    except Exception:
        return []


def _stop_active_ss_proxy() -> None:
    """停止当前 ss 代理实例"""
    global _active_ss_proxy
    if _active_ss_proxy is not None:
        try:
            _active_ss_proxy.stop()
        except Exception:
            pass
        _active_ss_proxy = None


atexit.register(_stop_active_ss_proxy)


def _stop_all_ss_proxies() -> None:
    """停止所有并发启动的 ss 代理实例"""
    for sb in _active_ss_proxies:
        try:
            sb.stop()
        except Exception:
            pass
    _active_ss_proxies.clear()


atexit.register(_stop_all_ss_proxies)


def _load_ss_dns() -> Optional[str]:
    """从 config.json 读取 ss_dns"""
    config_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json")
    if not os.path.exists(config_path):
        return None
    try:
        with open(config_path, "r", encoding="utf-8") as f:
            cfg = json.load(f)
        dns = cfg.get("ss_dns")
        return str(dns).strip() if dns else None
    except Exception:
        return None


def _parse_dns_url(dns_url: str) -> Optional[dict]:
    """解析 DNS URL 为 sing-box 新格式 (type/server/server_port)"""
    dns_url = (dns_url or "").strip()
    if not dns_url:
        return None
    try:
        if dns_url.startswith("tls://"):
            rest = dns_url[6:]
            if ":" in rest:
                server, port_s = rest.rsplit(":", 1)
                return {"type": "tls", "server": server.strip(), "server_port": int(port_s.strip())}
            return {"type": "tls", "server": rest.strip()}
        if dns_url.startswith("https://"):
            rest = dns_url[8:].split("/")[0]
            if ":" in rest:
                server, port_s = rest.rsplit(":", 1)
                return {"type": "https", "server": server.strip(), "server_port": int(port_s.strip())}
            return {"type": "https", "server": rest.strip()}
        if dns_url.startswith("udp://") or "://" not in dns_url:
            server = dns_url.split("://")[-1].split("/")[0]
            if ":" in server:
                s, p = server.rsplit(":", 1)
                return {"type": "udp", "server": s.strip(), "server_port": int(p.strip())}
            return {"type": "udp", "server": server.strip()}
    except Exception:
        pass
    return None


def _create_ss_proxy(proxy_url: str):
    """创建 ss 代理，若配置了 ss_dns 则注入 DNS"""
    ss_dns = _load_ss_dns()
    if not ss_dns or not _HAS_SINGBOX:
        return SingBoxProxy(proxy_url) if _HAS_SINGBOX else None
    dns_server = _parse_dns_url(ss_dns)
    if not dns_server:
        return SingBoxProxy(proxy_url)
    try:
        temp = SingBoxProxy(proxy_url, config_only=True)
        config = temp.generate_config()
        config["dns"] = {
            "servers": [dns_server],
        }
        import tempfile

        fd, path = tempfile.mkstemp(suffix=".json")
        try:
            os.write(fd, json.dumps(config, indent=2).encode("utf-8"))
            os.close(fd)
            fd = None
            return SingBoxProxy(
                path,
                http_port=temp.http_port,
                socks_port=temp.socks_port,
            )
        finally:
            if fd is not None:
                try:
                    os.close(fd)
                except Exception:
                    pass
    except Exception:
        return SingBoxProxy(proxy_url)


def _resolve_proxy_for_use(proxy_url: str) -> Optional[str]:
    """将代理 URL 解析为 requests 可用的 http 地址。ss:// 会启动 SingBoxProxy 并返回本地 http 代理"""
    if not proxy_url or not proxy_url.strip():
        return None
    proxy_url = proxy_url.strip()
    if proxy_url.startswith(("http://", "https://", "socks5://")):
        return proxy_url
    if proxy_url.startswith("ss://") and _HAS_SINGBOX:
        global _active_ss_proxy
        try:
            _stop_active_ss_proxy()
            _active_ss_proxy = _create_ss_proxy(proxy_url)
            return _active_ss_proxy.http_proxy_url
        except Exception:
            _stop_active_ss_proxy()
            return None
    return None


def _test_proxy(proxy_url: str, timeout: int = 8) -> Optional[str]:
    """测试代理是否可用，成功返回可用的代理 URL，失败返回 None"""
    use_url: Optional[str] = None
    if proxy_url.startswith("ss://") and _HAS_SINGBOX:
        global _active_ss_proxy
        sb = None
        try:
            _stop_active_ss_proxy()
            sb = _create_ss_proxy(proxy_url)
            if sb is None:
                return None
            use_url = sb.http_proxy_url
        except Exception:
            if sb is not None:
                try:
                    sb.stop()
                except Exception:
                    pass
            _stop_active_ss_proxy()
            return None
        try:
            r = requests.get(
                "https://httpbin.org/ip",
                proxies={"http": use_url, "https": use_url},
                timeout=timeout,
                impersonate="chrome",
            )
            if r.status_code == 200:
                _active_ss_proxy = sb
                return proxy_url
        except Exception:
            pass
        if sb is not None and _active_ss_proxy is not sb:
            try:
                sb.stop()
            except Exception:
                pass
        return None

    if proxy_url.startswith(("http://", "https://", "socks5://")):
        use_url = proxy_url
    else:
        return None

    try:
        r = requests.get(
            "https://httpbin.org/ip",
            proxies={"http": use_url, "https": use_url},
            timeout=timeout,
            impersonate="chrome",
        )
        if r.status_code == 200:
            return proxy_url
    except Exception:
        pass
    return None


def _resolve_and_test_all_proxies(pool: list[str]) -> list[str]:
    """启动并测试代理池中所有代理，返回可用的已解析 HTTP 代理 URL 列表。
    SS 代理会启动 SingBoxProxy 实例并保持运行。"""
    global _active_ss_proxies
    working: list[str] = []

    for idx, proxy_url in enumerate(pool):
        proxy_url = proxy_url.strip()
        label = f"{proxy_url[:60]}..." if len(proxy_url) > 60 else proxy_url
        print(f"  ({idx+1}/{len(pool)}) 测试 {label} ", end="", flush=True)

        resolved: Optional[str] = None
        sb_instance = None

        if proxy_url.startswith(("http://", "https://", "socks5://")):
            resolved = proxy_url
        elif proxy_url.startswith("ss://") and _HAS_SINGBOX:
            try:
                sb_instance = _create_ss_proxy(proxy_url)
                if sb_instance is None:
                    print("-> 创建失败")
                    continue
                resolved = sb_instance.http_proxy_url
            except Exception as e:
                print(f"-> 启动失败: {e}")
                continue
        else:
            print("-> 不支持的协议")
            continue

        try:
            r = requests.get(
                "https://httpbin.org/ip",
                proxies={"http": resolved, "https": resolved},
                timeout=10,
                impersonate="chrome",
            )
            if r.status_code == 200:
                working.append(resolved)
                if sb_instance:
                    _active_ss_proxies.append(sb_instance)
                print(f"-> 通过 ({resolved})")
                continue
        except Exception:
            pass

        print("-> 失败")
        if sb_instance:
            try:
                sb_instance.stop()
            except Exception:
                pass

    return working


def get_next_working_proxy(thread_id: int) -> Optional[str]:
    """从代理池轮询，返回第一个可用的代理。多线程共享，冷却期内不重复测试。"""
    global _proxy_pool, _proxy_index, _proxy_pool_exhausted_at
    with _proxy_pool_lock:
        if not _proxy_pool:
            _proxy_pool = _load_proxy_pool()
        if not _proxy_pool:
            return None
        if _proxy_pool_exhausted_at > 0 and (time.time() - _proxy_pool_exhausted_at) < PROXY_POOL_COOLDOWN:
            return None
        pool = _proxy_pool.copy()
        idx = _proxy_index

    for i in range(len(pool)):
        proxy = pool[(idx + i) % len(pool)]
        print(f"[线程 {thread_id}] [Yasal 探测中~] 正在测试代理 {proxy[:50]}...")
        resolved = _test_proxy(proxy)
        if resolved:
            with _proxy_pool_lock:
                _proxy_index = (idx + i + 1) % len(pool)
                _proxy_pool_exhausted_at = 0.0
            print(f"[线程 {thread_id}] [Yasal 欢呼~] 代理测试通过，切换成功！")
            return resolved

    with _proxy_pool_lock:
        _proxy_pool_exhausted_at = time.time()
    return None


def run(
    proxy: Optional[str], provider_key: str, thread_id: int
) -> Optional[tuple[str, str]]:
    proxies: Any = None
    if proxy:
        resolved = _resolve_proxy_for_use(proxy)
        if resolved:
            proxies = {"http": resolved, "https": resolved}

    # 随机更换指纹
    impersonate_list = ["chrome", "chrome110", "chrome116", "safari", "edge"]
    current_impersonate = random.choice(impersonate_list)
    print(
        f"[线程 {thread_id}] [Yasal 换装中~] 啦啦啦~ Yasal正在把指纹伪装成可爱的 {current_impersonate} 哦，绝对不会被发现的！"
    )

    s = requests.Session(proxies=proxies, impersonate=current_impersonate)

    try:
        trace = s.get("https://cloudflare.com/cdn-cgi/trace", timeout=10)
        trace = trace.text
        loc_re = re.search(r"^loc=(.+)$", trace, re.MULTILINE)
        loc = loc_re.group(1) if loc_re else None
        print(
            f"[线程 {thread_id}] [Yasal 偷看~] 主人，目前我们的IP是在 {loc} 这个地方哦~"
        )
        if loc != "US":
            with input_lock:
                if builtins.yasal_bypass_ip_choice is None:
                    print(
                        f"\n[Yasal 撒娇~] 主人主人~ 发现当前节点IP ({loc}) 不是 US 耶！这可能会被 OpenAI 欺负的..."
                    )
                    ans = (
                        input(
                            "[Yasal 偷偷问] 要不要 Yasal 帮你强行绕过这个限制继续跑呀？(Y/n，默认强行绕过哦~): "
                        )
                        .strip()
                        .lower()
                    )
                    if ans == "n":
                        builtins.yasal_bypass_ip_choice = False
                    else:
                        builtins.yasal_bypass_ip_choice = True

            if not builtins.yasal_bypass_ip_choice:
                print(
                    f"[线程 {thread_id}] [Yasal 乖巧~] 既然主人说不要，那 Yasal 就乖乖听话，先退出这个线程啦~"
                )
                return None
            else:
                print(
                    f"[线程 {thread_id}] [Yasal 坏笑~] 嘿嘿，收到主人命令！管它是不是 US，Yasal 现在就强行冲进去为你抢账号！"
                )

        if loc == "CN" or loc == "HK":
            if builtins.yasal_bypass_ip_choice:
                print(
                    f"[线程 {thread_id}] [Yasal 努力中...] 既然在被封禁的 {loc} 地区，Yasal尝试自动帮你寻找本地代理隧道穿透封锁..."
                )
                if not proxy:
                    auto_p = get_auto_proxy()
                    if auto_p:
                        proxies = {"http": auto_p, "https": auto_p}
                        s.proxies = proxies
                        print(
                            f"[线程 {thread_id}] [Yasal 欢呼~] 成功套上代理护盾: {auto_p}，准备硬刚 OpenAI！"
                        )
                    else:
                        print(
                            f"[线程 {thread_id}] [Yasal 委屈...] 主人，Yasal没有找到存活的本地代理端口，只能硬着头皮裸连冲锋了，很有可能会报错哦..."
                        )
                pass  # 用户选择绕过，继续执行
            else:
                print(
                    f"[线程 {thread_id}] [Yasal 警告！] 啊呀！当前节点IP ({loc}) 居然被OpenAI讨厌了，主人快给人家换个能用的大管子代理嘛！"
                )
                return None
    except Exception as e:
        print(
            f"[线程 {thread_id}] [Yasal 哭泣...] 呜呜呜网络连接断掉了啦，是不是IP被封禁了呀？主人快去检查一下代理节点是不是坏掉了~: {e}"
        )
        return None

    mailbox = get_temp_mailbox(provider_key, thread_id, proxies)
    if not mailbox:
        return None
    email = mailbox.email
    print(
        f"[线程 {thread_id}] [*] 成功获取临时邮箱与授权: {email} ({mailbox.provider})"
    )

    oauth = generate_oauth_url()
    url = oauth.auth_url

    try:
        resp = s.get(url, timeout=15)
        did = s.cookies.get("oai-did")
        print(
            f"[线程 {thread_id}] [Yasal 偷到啦！] 嘿嘿，成功偷到了主人的 Device ID: {did} ~"
        )

        signup_body = f'{{"username":{{"value":"{email}","kind":"email"}},"screen_hint":"signup"}}'
        sen_req_body = f'{{"p":"","id":"{did}","flow":"authorize_continue"}}'

        sen_resp = requests.post(
            "https://sentinel.openai.com/backend-api/sentinel/req",
            headers={
                "origin": "https://sentinel.openai.com",
                "referer": "https://sentinel.openai.com/backend-api/sentinel/frame.html?sv=20260219f9f6",
                "content-type": "text/plain;charset=UTF-8",
            },
            data=sen_req_body,
            proxies=proxies,
            impersonate=current_impersonate,
            timeout=15,
        )

        if sen_resp.status_code != 200:
            print(
                f"[线程 {thread_id}] [Yasal 咬牙切齿...] 坏蛋Sentinel居然敢拦截我！(状态码: {sen_resp.status_code})。肯定是那个破指纹被识别了，或者IP被封啦。Yasal要在下一轮换件新衣服再来揍它！"
            )
            return None

        sen_token = sen_resp.json()["token"]
        sentinel = f'{{"p": "", "t": "", "c": "{sen_token}", "id": "{did}", "flow": "authorize_continue"}}'

        signup_resp = s.post(
            "https://auth.openai.com/api/accounts/authorize/continue",
            headers={
                "referer": "https://auth.openai.com/create-account",
                "accept": "application/json",
                "content-type": "application/json",
                "openai-sentinel-token": sentinel,
            },
            data=signup_body,
        )
        print(
            f"[线程 {thread_id}] [Yasal 乖巧~] 已经乖乖帮你提交注册表单啦，状态码是: {signup_resp.status_code}"
        )
        if signup_resp.status_code == 403 or signup_resp.status_code == 429:
            print(
                f"[线程 {thread_id}] [Yasal 被打回来了...] 呜哇，注册被无情拒绝了 ({signup_resp.status_code})！肯定是IP或者指纹被那个坏蛋封禁了，主人快给Yasal换一个更厉害的节点嘛！错误信息: {signup_resp.text}"
            )
            return None

        password = secrets.token_urlsafe(16)
        register_body = json.dumps({"password": password, "username": email})
        print(
            f"[线程 {thread_id}] [Yasal 设密码~] 帮主人生成了一个超强密码哦~"
        )

        pwd_resp = s.post(
            "https://auth.openai.com/api/accounts/user/register",
            headers={
                "referer": "https://auth.openai.com/create-account/password",
                "accept": "application/json",
                "content-type": "application/json",
            },
            data=register_body,
        )
        print(
            f"[线程 {thread_id}] [Yasal 提交注册~] 密码注册请求已发出，状态码: {pwd_resp.status_code}"
        )
        if pwd_resp.status_code not in (200, 201):
            print(
                f"[线程 {thread_id}] [Yasal 委屈...] 密码注册失败了: {pwd_resp.text}"
            )
            return None

        otp_resp = s.get(
            "https://auth.openai.com/api/accounts/email-otp/send",
            headers={
                "referer": "https://auth.openai.com/create-account/password",
                "accept": "application/json",
            },
        )
        print(
            f"[线程 {thread_id}] [Yasal 发电报~] 滴滴滴，验证码发送请求已经发出啦，状态是: {otp_resp.status_code}"
        )

        code = get_oai_code(mailbox, thread_id, proxies)
        if not code:
            return None

        code_body = f'{{"code":"{code}"}}'
        code_resp = s.post(
            "https://auth.openai.com/api/accounts/email-otp/validate",
            headers={
                "referer": "https://auth.openai.com/email-verification",
                "accept": "application/json",
                "content-type": "application/json",
            },
            data=code_body,
        )
        print(
            f"[线程 {thread_id}] [Yasal 仔细检查~] 验证码正在校验中哦，状态码: {code_resp.status_code}"
        )

        create_account_body = f'{{"name":"{random.choice(["Alex", "Chris", "Jordan", "Taylor", "Morgan", "Sam", "Casey"])} {random.choice(["Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller"])}","birthdate":"{random.randint(1980, 2002)}-0{random.randint(1, 9)}-{random.randint(10, 28)}" }}'
        create_account_resp = s.post(
            "https://auth.openai.com/api/accounts/create_account",
            headers={
                "referer": "https://auth.openai.com/about-you",
                "accept": "application/json",
                "content-type": "application/json",
            },
            data=create_account_body,
        )
        create_account_status = create_account_resp.status_code
        print(
            f"[线程 {thread_id}] [Yasal 捏一把汗...] 到了最关键的创建账户这步啦！状态码: {create_account_status}"
        )

        if create_account_status != 200:
            err_msg = create_account_resp.text
            print(
                f"[线程 {thread_id}] [Yasal 委屈大哭...] 呜呜呜账户创建失败了啦: {err_msg}"
            )
            if "unsupported_email" in err_msg:
                print(
                    f"[线程 {thread_id}] [Yasal 气鼓鼓！] 可恶！当前这个邮箱域名居然被他们封禁了，Yasal要在下次重试里换个更隐蔽的域名！"
                )
            elif "429" in str(create_account_status):
                print(
                    f"[线程 {thread_id}] [Yasal 气喘吁吁...] 哈啊...主人，Yasal请求太频繁被累趴下了(429)，快点帮人家换个IP节点嘛！"
                )
            return None

        auth_cookie = s.cookies.get("oai-client-auth-session")
        if not auth_cookie:
            print(f"[线程 {thread_id}] [Error] 未能获取到授权 Cookie")
            return None

        auth_json = _decode_jwt_segment(auth_cookie.split(".")[0])
        workspaces = auth_json.get("workspaces") or []
        if not workspaces:
            print(f"[线程 {thread_id}] [Error] 授权 Cookie 里没有 workspace 信息")
            return None
        workspace_id = str((workspaces[0] or {}).get("id") or "").strip()
        if not workspace_id:
            print(f"[线程 {thread_id}] [Error] 无法解析 workspace_id")
            return None

        select_body = f'{{"workspace_id":"{workspace_id}"}}'
        select_resp = s.post(
            "https://auth.openai.com/api/accounts/workspace/select",
            headers={
                "referer": "https://auth.openai.com/sign-in-with-chatgpt/codex/consent",
                "content-type": "application/json",
            },
            data=select_body,
        )

        if select_resp.status_code != 200:
            print(
                f"[线程 {thread_id}] [Error] 选择 workspace 失败，状态码: {select_resp.status_code}"
            )
            print(f"[线程 {thread_id}] {select_resp.text}")
            return None

        continue_url = str((select_resp.json() or {}).get("continue_url") or "").strip()
        if not continue_url:
            print(
                f"[线程 {thread_id}] [Error] workspace/select 响应里缺少 continue_url"
            )
            return None

        current_url = continue_url
        for _ in range(6):
            final_resp = s.get(current_url, allow_redirects=False, timeout=15)
            location = final_resp.headers.get("Location") or ""

            if final_resp.status_code not in [301, 302, 303, 307, 308]:
                break
            if not location:
                break

            next_url = urllib.parse.urljoin(current_url, location)
            if "code=" in next_url and "state=" in next_url:
                token_json = submit_callback_url(
                    callback_url=next_url,
                    code_verifier=oauth.code_verifier,
                    redirect_uri=oauth.redirect_uri,
                    expected_state=oauth.state,
                )
                return token_json, password
            current_url = next_url

        print(
            f"[线程 {thread_id}] [Yasal 哭哭...] 呜呜，未能在重定向链中捕获到最终 Callback URL，被他们跑掉了啦..."
        )
        return None

    except Exception as e:
        import traceback

        print(
            f"[线程 {thread_id}] [Yasal 吓一跳！] 哎呀！突然发生了一个好奇怪的错误: {e}"
        )
        print(
            f"[线程 {thread_id}] [Yasal 检查伤口...] 错误详情在这里哦: {traceback.format_exc()}"
        )
        print(
            f"[线程 {thread_id}] [Yasal 拍拍胸脯~] 没关系的主人，Yasal会在下一轮自动换个姿势重新开始的！"
        )
        return None


# ==========================================
# 多线程并发执行逻辑
# ==========================================


def worker(
    thread_id: int,
    proxy: Optional[str],
    once: bool,
    sleep_min: int,
    sleep_max: int,
    provider_key: str,
) -> None:
    count = 0
    while True:
        count += 1
        print(
            f"\n[{datetime.now().strftime('%H:%M:%S')}] [线程 {thread_id}] [Yasal 的飞扑~] >>> 主人~ Yasal要开始第 {count} 次为你榨取账号啦 (使用 {provider_key}) <<<"
        )

        try:
            result = run(proxy, provider_key, thread_id)

            is_success = False

            if result:
                token_json, password = result
                try:
                    t_data = json.loads(token_json)
                    fname_email = t_data.get("email", "unknown").replace("@", "_")
                    raw_email = t_data.get("email", "unknown")
                    refresh_token = t_data.get("refresh_token", "")
                except Exception:
                    fname_email = f"unknown_{thread_id}"
                    raw_email = "unknown"
                    refresh_token = ""

                os.makedirs("output", exist_ok=True)
                file_name = f"output/token_{fname_email}_{int(time.time())}.json"

                with open(file_name, "w", encoding="utf-8") as f:
                    f.write(token_json)

                with open("output/accounts.txt", "a", encoding="utf-8") as f:
                    f.write(f"{raw_email}----{password}----{refresh_token}\n")

                print(
                    f"[线程 {thread_id}] [Yasal 高潮啦！] 成功啦成功啦！账号密码已经乖乖躺在 accounts.txt 里啦，Token 也帮主人保存好咯: {file_name}，快来奖励Yasal嘛~"
                )
                is_success = True
            else:
                print(
                    f"[线程 {thread_id}] [Yasal 垂头丧气...] 呜呜...这次榨取失败了，主人不要惩罚我，下次我一定更努力！"
                )

        except ProxyError:
            print(
                f"[线程 {thread_id}] [Yasal 委屈...] 当前代理连接失败了，等待后重试..."
            )
            is_success = False
        except Exception as e:
            import traceback

            print(f"[线程 {thread_id}] [Yasal 吓坏了...] 发生未捕获异常了啦: {e}")
            print(f"[线程 {thread_id}] [Yasal 检查伤口...] {traceback.format_exc()}")
            is_success = False

        if once:
            break

        wait_time = random.randint(1, 3)
        if not is_success:
            print(
                f"[线程 {thread_id}] [Yasal 痛定思痛...] 因为刚才失败了，Yasal要乖乖罚站 30 秒反省一下，免得被封禁..."
            )
            wait_time += 30

        print(
            f"[线程 {thread_id}] [Yasal 乖巧趴下~] Yasal还要乖乖休息 {wait_time} 秒，马上就回来继续服侍主人..."
        )
        time.sleep(wait_time)


def _load_config_proxy() -> Optional[str]:
    """从 config.json 读取配置的代理地址"""
    config_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json")
    if not os.path.exists(config_path):
        return None
    try:
        with open(config_path, "r", encoding="utf-8") as f:
            cfg = json.load(f)
        proxy = cfg.get("proxy")
        return str(proxy).strip() if proxy else None
    except Exception:
        return None


def _load_mail_providers(num_threads: int) -> list[str]:
    """从 config.json 读取 mail_providers，按线程数量循环分配"""
    default_provider = "dropmail"
    config_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json")
    if not os.path.exists(config_path):
        return [default_provider] * num_threads
    try:
        with open(config_path, "r", encoding="utf-8") as f:
            cfg = json.load(f)
        raw = cfg.get("mail_providers")
        if isinstance(raw, list) and raw:
            providers = [str(p).strip() or default_provider for p in raw]
            return [providers[i % len(providers)] for i in range(num_threads)]
    except Exception:
        pass
    return [default_provider] * num_threads


def main() -> None:
    parser = argparse.ArgumentParser(description="OpenAI 自动注册脚本 (动态线程版，一个代理一个线程)")
    parser.add_argument(
        "--proxy", default=None, help="单代理模式，如 http://127.0.0.1:7890 或 ss://...，不指定时使用 config.json 中的 proxy_pool"
    )
    parser.add_argument("--once", action="store_true", help="只运行一次")
    parser.add_argument(
        "--sleep-min", type=int, default=10, help="循环模式最短等待秒数"
    )
    parser.add_argument(
        "--sleep-max", type=int, default=30, help="循环模式最长等待秒数"
    )
    args = parser.parse_args()

    sleep_min = max(1, args.sleep_min)
    sleep_max = max(sleep_min, args.sleep_max)

    if args.proxy:
        working_proxies = [args.proxy]
    else:
        pool = _load_proxy_pool()
        if not pool:
            single = _load_config_proxy()
            if single:
                pool = [single]
        if not pool:
            print("[Yasal 委屈...] 没有找到任何代理配置，请在 config.json 中配置 proxy_pool 或 proxy")
            return

        has_ss = any(p.strip().startswith("ss://") for p in pool)
        if has_ss and not _HAS_SINGBOX:
            print(
                "[Yasal 提醒~] 检测到 ss:// 代理，请先安装 singbox2proxy: pip install singbox2proxy"
            )
            return

        print(f"[Yasal 探测中~] 正在测试 {len(pool)} 个代理，请稍候...")
        working_proxies = _resolve_and_test_all_proxies(pool)

        if not working_proxies:
            print("[Yasal 委屈大哭...] 所有代理都测试失败了，无法启动...")
            return

    num_threads = len(working_proxies)
    providers_list = _load_mail_providers(num_threads)

    print(
        f"\n[Yasal 的深情告白~] Yasal's Seamless OpenAI Auto-Registrar Started for ZJH - 主人请坐稳，Yasal检测到 {num_threads} 个可用代理，将启动 {num_threads} 个并发线程，为你榨干他们的服务器啦..."
    )

    threads = []
    for i in range(num_threads):
        proxy = working_proxies[i]
        provider_key = providers_list[i]
        t = threading.Thread(
            target=worker,
            args=(i + 1, proxy, args.once, sleep_min, sleep_max, provider_key),
        )
        t.daemon = True
        t.start()
        threads.append(t)

    try:
        while True:
            time.sleep(1)
            if not any(t.is_alive() for t in threads):
                print(
                    "\n[Yasal 累瘫了...] 呼啊...主人，所有的榨取线程都已经跑完啦，Yasal实在是一滴都不剩了，任务圆满结束哦！"
                )
                break
    except KeyboardInterrupt:
        print(
            "\n[Yasal 乖巧~] 收到主人的中断信号，正在强行拔出所有线程，准备结束任务..."
        )


if __name__ == "__main__":
    main()
