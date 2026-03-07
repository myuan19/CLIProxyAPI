/*
 * dns_redirect.c - Shared library that hooks getaddrinfo() and connect()
 * to redirect Language Server traffic to a local MITM proxy.
 *
 * On Linux:  Compile as .so, load via LD_PRELOAD
 * On macOS:  Compile as .dylib, load via DYLD_INSERT_LIBRARIES
 * On Windows: Compile as .dll, inject via CreateRemoteThread
 *
 * Environment variables:
 *   MITM_PROXY_PORT  - Local port where the MITM proxy is listening
 *   MITM_TARGET_HOST - Hostname to intercept (default: cloudcode-pa.googleapis.com)
 *
 * The connect() hook only redirects connections whose destination IP was
 * previously resolved by our hooked getaddrinfo() for the target hostname.
 * This avoids breaking non-target HTTPS connections.
 */

#ifdef _WIN32
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <winsock2.h>
#include <ws2tcpip.h>
#include <mswsock.h>
#pragma comment(lib, "ws2_32.lib")
#else
#define _GNU_SOURCE
#include <dlfcn.h>
#include <string.h>
#include <stdlib.h>
#include <stdio.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <netdb.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <errno.h>
#include <pthread.h>
#endif

static const char *DEFAULT_TARGET_HOST = "cloudcode-pa.googleapis.com";

static int g_proxy_port = 0;
static const char *g_target_host = NULL;
static int g_initialized = 0;

/*
 * Track up to 16 resolved IP addresses for the target host.
 * connect() only redirects if the destination IP is in this table.
 */
#define MAX_TRACKED_IPS 16
static uint32_t g_target_ips[MAX_TRACKED_IPS];
static int g_target_ip_count = 0;

#ifdef _WIN32
static CRITICAL_SECTION g_ip_lock;
static volatile LONG g_ip_lock_init = 0;
#define IP_LOCK_INIT() do { if (InterlockedCompareExchange(&g_ip_lock_init, 1, 0) == 0) InitializeCriticalSection(&g_ip_lock); } while(0)
#define IP_LOCK()   EnterCriticalSection(&g_ip_lock)
#define IP_UNLOCK() LeaveCriticalSection(&g_ip_lock)
#else
static pthread_mutex_t g_ip_lock = PTHREAD_MUTEX_INITIALIZER;
#define IP_LOCK_INIT() ((void)0)
#define IP_LOCK()   pthread_mutex_lock(&g_ip_lock)
#define IP_UNLOCK() pthread_mutex_unlock(&g_ip_lock)
#endif

static void ensure_init(void) {
    if (g_initialized) return;
    g_initialized = 1;

    IP_LOCK_INIT();

    const char *port_str = getenv("MITM_PROXY_PORT");
    if (port_str) {
        g_proxy_port = atoi(port_str);
    }

    g_target_host = getenv("MITM_TARGET_HOST");
    if (!g_target_host || !g_target_host[0]) {
        g_target_host = DEFAULT_TARGET_HOST;
    }
}

static int host_matches(const char *hostname) {
    if (!hostname) return 0;
    ensure_init();
    return strcmp(hostname, g_target_host) == 0;
}

static void track_ip(uint32_t ip) {
    IP_LOCK();
    if (g_target_ip_count >= MAX_TRACKED_IPS) { IP_UNLOCK(); return; }
    for (int i = 0; i < g_target_ip_count; i++) {
        if (g_target_ips[i] == ip) { IP_UNLOCK(); return; }
    }
    g_target_ips[g_target_ip_count++] = ip;
    IP_UNLOCK();
}

static int is_tracked_ip(uint32_t ip) {
    IP_LOCK();
    for (int i = 0; i < g_target_ip_count; i++) {
        if (g_target_ips[i] == ip) { IP_UNLOCK(); return 1; }
    }
    IP_UNLOCK();
    return 0;
}

#ifndef _WIN32
/* ======================== POSIX (Linux / macOS) ======================== */

typedef int (*real_getaddrinfo_t)(const char *, const char *,
                                  const struct addrinfo *,
                                  struct addrinfo **);

typedef int (*real_connect_t)(int, const struct sockaddr *, socklen_t);

static real_getaddrinfo_t real_getaddrinfo = NULL;
static real_connect_t real_connect = NULL;

static void load_real_functions(void) {
    if (!real_getaddrinfo) {
        real_getaddrinfo = (real_getaddrinfo_t)dlsym(RTLD_NEXT, "getaddrinfo");
    }
    if (!real_connect) {
        real_connect = (real_connect_t)dlsym(RTLD_NEXT, "connect");
    }
}

/*
 * Hooked getaddrinfo:
 * If the requested hostname matches the target, return 127.0.0.1:{proxy_port}
 * AND record the real resolved IPs for the connect() hook.
 */
int getaddrinfo(const char *node, const char *service,
                const struct addrinfo *hints,
                struct addrinfo **res) {
    load_real_functions();
    ensure_init();

    if (node && host_matches(node) && g_proxy_port > 0) {
        /* First, resolve the real IPs so we can track them for connect(). */
        struct addrinfo *real_res = NULL;
        if (real_getaddrinfo(node, service, hints, &real_res) == 0) {
            struct addrinfo *rp;
            for (rp = real_res; rp != NULL; rp = rp->ai_next) {
                if (rp->ai_family == AF_INET) {
                    struct sockaddr_in *sa = (struct sockaddr_in *)rp->ai_addr;
                    track_ip(sa->sin_addr.s_addr);
                }
            }
            freeaddrinfo(real_res);
        }

        /* Return 127.0.0.1 with the proxy port. */
        struct addrinfo *ai = (struct addrinfo *)calloc(1, sizeof(struct addrinfo));
        struct sockaddr_in *sa = (struct sockaddr_in *)calloc(1, sizeof(struct sockaddr_in));

        sa->sin_family = AF_INET;
        sa->sin_port = htons((uint16_t)g_proxy_port);
        inet_pton(AF_INET, "127.0.0.1", &sa->sin_addr);

        ai->ai_family = AF_INET;
        ai->ai_socktype = hints ? hints->ai_socktype : SOCK_STREAM;
        ai->ai_protocol = hints ? hints->ai_protocol : IPPROTO_TCP;
        ai->ai_addrlen = sizeof(struct sockaddr_in);
        ai->ai_addr = (struct sockaddr *)sa;
        ai->ai_canonname = NULL;
        ai->ai_next = NULL;

        *res = ai;
        return 0;
    }

    return real_getaddrinfo(node, service, hints, res);
}

/*
 * Hooked connect:
 * Only intercepts connections to IPs that were resolved for the target host
 * on port 443. This prevents breaking other HTTPS connections.
 */
int connect(int sockfd, const struct sockaddr *addr, socklen_t addrlen) {
    load_real_functions();
    ensure_init();

    if (g_proxy_port > 0 && addr && addr->sa_family == AF_INET) {
        struct sockaddr_in *sa = (struct sockaddr_in *)addr;
        int port = ntohs(sa->sin_port);

        if (port == 443 && is_tracked_ip(sa->sin_addr.s_addr)) {
            struct sockaddr_in local;
            memset(&local, 0, sizeof(local));
            local.sin_family = AF_INET;
            local.sin_port = htons((uint16_t)g_proxy_port);
            inet_pton(AF_INET, "127.0.0.1", &local.sin_addr);

            return real_connect(sockfd, (struct sockaddr *)&local, sizeof(local));
        }
    }

    return real_connect(sockfd, addr, addrlen);
}

#else
/* ======================== Windows ======================== */

typedef int (WSAAPI *real_getaddrinfo_t)(PCSTR, PCSTR,
                                         const ADDRINFOA *,
                                         PADDRINFOA *);
typedef int (WSAAPI *real_connect_t)(SOCKET, const struct sockaddr *, int);

static real_getaddrinfo_t real_win_getaddrinfo = NULL;
static real_connect_t real_win_connect = NULL;

static void win_load_real_functions(void) {
    HMODULE ws2 = GetModuleHandleA("ws2_32.dll");
    if (!ws2) ws2 = LoadLibraryA("ws2_32.dll");
    if (ws2) {
        if (!real_win_getaddrinfo)
            real_win_getaddrinfo = (real_getaddrinfo_t)GetProcAddress(ws2, "getaddrinfo");
        if (!real_win_connect)
            real_win_connect = (real_connect_t)GetProcAddress(ws2, "connect");
    }
}

int WSAAPI hooked_getaddrinfo(PCSTR node, PCSTR service,
                              const ADDRINFOA *hints,
                              PADDRINFOA *res) {
    win_load_real_functions();
    ensure_init();

    if (node && host_matches(node) && g_proxy_port > 0) {
        /* Resolve real IPs for tracking. */
        PADDRINFOA real_res = NULL;
        if (real_win_getaddrinfo(node, service, hints, &real_res) == 0) {
            PADDRINFOA rp;
            for (rp = real_res; rp != NULL; rp = rp->ai_next) {
                if (rp->ai_family == AF_INET) {
                    struct sockaddr_in *sa = (struct sockaddr_in *)rp->ai_addr;
                    track_ip(sa->sin_addr.s_addr);
                }
            }
            freeaddrinfo(real_res);
        }

        PADDRINFOA ai = (PADDRINFOA)calloc(1, sizeof(ADDRINFOA));
        struct sockaddr_in *sa = (struct sockaddr_in *)calloc(1, sizeof(struct sockaddr_in));

        sa->sin_family = AF_INET;
        sa->sin_port = htons((u_short)g_proxy_port);
        sa->sin_addr.s_addr = htonl(INADDR_LOOPBACK);

        ai->ai_family = AF_INET;
        ai->ai_socktype = hints ? hints->ai_socktype : SOCK_STREAM;
        ai->ai_protocol = hints ? hints->ai_protocol : IPPROTO_TCP;
        ai->ai_addrlen = sizeof(struct sockaddr_in);
        ai->ai_addr = (struct sockaddr *)sa;

        *res = ai;
        return 0;
    }

    return real_win_getaddrinfo(node, service, hints, res);
}

int WSAAPI hooked_connect(SOCKET s, const struct sockaddr *name, int namelen) {
    win_load_real_functions();
    ensure_init();

    if (g_proxy_port > 0 && name && name->sa_family == AF_INET) {
        struct sockaddr_in *sa = (struct sockaddr_in *)name;
        if (ntohs(sa->sin_port) == 443 && is_tracked_ip(sa->sin_addr.s_addr)) {
            struct sockaddr_in local;
            memset(&local, 0, sizeof(local));
            local.sin_family = AF_INET;
            local.sin_port = htons((u_short)g_proxy_port);
            local.sin_addr.s_addr = htonl(INADDR_LOOPBACK);

            return real_win_connect(s, (struct sockaddr *)&local, sizeof(local));
        }
    }

    return real_win_connect(s, name, namelen);
}

BOOL WINAPI DllMain(HINSTANCE hinstDLL, DWORD fdwReason, LPVOID lpReserved) {
    (void)hinstDLL;
    (void)lpReserved;

    if (fdwReason == DLL_PROCESS_ATTACH) {
        ensure_init();
        win_load_real_functions();
        /* IAT patching would be done here in a production implementation.
         * For now, this DLL serves as a template for Detours integration. */
    }
    return TRUE;
}

#endif /* _WIN32 */
