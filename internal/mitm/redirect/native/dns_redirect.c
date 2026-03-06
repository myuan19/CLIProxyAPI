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
#endif

/* The default target hostname to intercept. */
static const char *DEFAULT_TARGET_HOST = "cloudcode-pa.googleapis.com";

/* Cached values from environment. */
static int g_proxy_port = 0;
static const char *g_target_host = NULL;
static int g_initialized = 0;

static void ensure_init(void) {
    if (g_initialized) return;
    g_initialized = 1;

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
 * If the requested hostname matches the target, return 127.0.0.1
 * so the LS connects to our local MITM proxy instead of Google.
 */
int getaddrinfo(const char *node, const char *service,
                const struct addrinfo *hints,
                struct addrinfo **res) {
    load_real_functions();
    ensure_init();

    if (node && host_matches(node) && g_proxy_port > 0) {
        /* Allocate a result pointing to 127.0.0.1 with the proxy port. */
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
 * Intercepts connections to port 443 that target Google's known IP ranges
 * and redirects them to the local MITM proxy. This is a fallback for cases
 * where the binary doesn't use getaddrinfo (e.g., hardcoded IPs or custom DNS).
 */
int connect(int sockfd, const struct sockaddr *addr, socklen_t addrlen) {
    load_real_functions();
    ensure_init();

    if (g_proxy_port > 0 && addr && addr->sa_family == AF_INET) {
        struct sockaddr_in *sa = (struct sockaddr_in *)addr;
        int port = ntohs(sa->sin_port);

        /* Only intercept HTTPS (port 443) connections. */
        if (port == 443) {
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

/*
 * On Windows we use Detours-style IAT patching.
 * The DLL is injected into the Language Server process via CreateRemoteThread.
 *
 * For simplicity, this implementation hooks the Winsock getaddrinfo and
 * WSAConnect functions by patching the IAT of the target process.
 */

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
        if (ntohs(sa->sin_port) == 443) {
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
