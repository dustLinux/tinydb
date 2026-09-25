/* webdb.c — implementation of the minimal tinydb C client.
 * Single file, POSIX sockets only. See webdb.h for the API.
 */
#define _POSIX_C_SOURCE 200809L
#include "webdb.h"

#include <errno.h>
#include <netdb.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

static __thread char g_err[512];

const char *webdb_last_error(void) { return g_err; }

static void set_err(const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(g_err, sizeof(g_err), fmt, ap);
    va_end(ap);
}

/* ---- URL parsing --------------------------------------------------- */

int webdb_client_init(webdb_client *c, const char *base_url, const char *token) {
    if (!c || !base_url) return -1;
    memset(c, 0, sizeof(*c));
    const char *p = base_url;
    if (strncmp(p, "http://", 7) == 0) p += 7;
    else if (strncmp(p, "https://", 8) == 0) {
        set_err("https is not supported (run the server on plain HTTP locally)");
        return -1;
    }
    const char *slash = strchr(p, '/');
    const char *colon = strchr(p, ':');
    size_t hostlen = slash ? (size_t)(slash - p) : strlen(p);
    const char *port = "80";
    size_t hlen = hostlen;
    if (colon && (!slash || colon < slash)) {
        hlen = (size_t)(colon - p);
        port = colon + 1;
        const char *pc = port;
        while (*pc && *pc != '/') pc++;
        /* copy port separately below */
    }
    if (hlen == 0) { set_err("bad url: no host"); return -1; }

    c->host = malloc(hlen + 1);
    if (!c->host) goto oom;
    memcpy(c->host, p, hlen);
    c->host[hlen] = 0;

    if (colon && (!slash || colon < slash)) {
        size_t plen = strcspn(colon + 1, "/");
        c->port = malloc(plen + 1);
        if (!c->port) goto oom;
        memcpy(c->port, colon + 1, plen);
        c->port[plen] = 0;
    } else {
        c->port = strdup("80");
        if (!c->port) goto oom;
    }

    if (slash) {
        size_t blen = strlen(slash);
        while (blen > 1 && slash[blen - 1] == '/') blen--;
        c->base_path = malloc(blen + 1);
        if (!c->base_path) goto oom;
        memcpy(c->base_path, slash, blen);
        c->base_path[blen] = 0;
    } else {
        c->base_path = strdup("");
        if (!c->base_path) goto oom;
    }
    if (token) {
        c->token = strdup(token);
        if (!c->token) goto oom;
    }
    return 0;
oom:
    set_err("out of memory");
    webdb_client_free(c);
    return -1;
}

void webdb_client_free(webdb_client *c) {
    if (!c) return;
    free(c->host);
    free(c->port);
    free(c->base_path);
    free(c->token);
    memset(c, 0, sizeof(*c));
}

void webdb_response_free(webdb_response *r) {
    if (!r) return;
    free(r->body);
    free(r->error);
    r->body = NULL;
    r->error = NULL;
    r->len = 0;
    r->status = 0;
}

/* ---- buffer -------------------------------------------------------- */

typedef struct { char *p; size_t len, cap; } buf;

static int buf_reserve(buf *b, size_t extra) {
    if (b->len + extra + 1 <= b->cap) return 0;
    size_t ncap = b->cap ? b->cap : 512;
    while (ncap < b->len + extra + 1) ncap *= 2;
    char *np = realloc(b->p, ncap);
    if (!np) { set_err("out of memory"); return -1; }
    b->p = np;
    b->cap = ncap;
    return 0;
}

static int buf_add(buf *b, const void *data, size_t n) {
    if (buf_reserve(b, n) != 0) return -1;
    memcpy(b->p + b->len, data, n);
    b->len += n;
    b->p[b->len] = 0;
    return 0;
}

static int buf_puts(buf *b, const char *s) { return buf_add(b, s, strlen(s)); }

static void buf_free(buf *b) { free(b->p); b->p = NULL; b->len = b->cap = 0; }

/* ---- connection ---------------------------------------------------- */

static int conn_open(const webdb_client *c) {
    struct addrinfo hints, *res = NULL;
    memset(&hints, 0, sizeof(hints));
    hints.ai_socktype = SOCK_STREAM;
    hints.ai_family = AF_UNSPEC;
    int rc = getaddrinfo(c->host, c->port, &hints, &res);
    if (rc != 0) {
        set_err("getaddrinfo: %s", gai_strerror(rc));
        return -1;
    }
    int fd = -1;
    for (struct addrinfo *ai = res; ai; ai = ai->ai_next) {
        fd = socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
        if (fd < 0) continue;
        if (connect(fd, ai->ai_addr, ai->ai_addrlen) == 0) break;
        close(fd);
        fd = -1;
    }
    freeaddrinfo(res);
    if (fd < 0) set_err("connect to %s:%s failed: %s", c->host, c->port, strerror(errno));
    return fd;
}

static int send_all(int fd, const char *data, size_t n) {
    while (n > 0) {
        ssize_t w = send(fd, data, n, 0);
        if (w <= 0) { set_err("send: %s", strerror(errno)); return -1; }
        data += w;
        n -= (size_t)w;
    }
    return 0;
}

/* ---- response reading ---------------------------------------------- */

/* Growable read buffer with a small pushback window. */
typedef struct {
    buf b;
    size_t pos;
} rbuf;

static int rbuf_fill(rbuf *rb, int fd) {
    size_t old = rb->b.len;
    if (buf_reserve(&rb->b, 8192) != 0) return -1;
    ssize_t n = recv(fd, rb->b.p + old, 8192, 0);
    if (n <= 0) {
        if (n == 0) return 1; /* EOF */
        set_err("recv: %s", strerror(errno));
        return -1;
    }
    rb->b.len = old + (size_t)n;
    rb->b.p[rb->b.len] = 0;
    return 0;
}

/* Find "\r\n" or "\n" from pos; returns pointer to line start in *out.
 * Returns length of line (without terminator), or -1 if more data needed,
 * -2 on EOF-before-complete. */
static long line_next(rbuf *rb, int fd, const char **out) {
    for (;;) {
        for (size_t i = rb->pos; i + 1 < rb->b.len; i++) {
            if (rb->b.p[i] == '\r' && rb->b.p[i + 1] == '\n') {
                *out = rb->b.p + rb->pos;
                long n = (long)(i - rb->pos);
                rb->pos = i + 2;
                return n;
            }
            if (rb->b.p[i] == '\n') {
                *out = rb->b.p + rb->pos;
                long n = (long)(i - rb->pos);
                rb->pos = i + 1;
                return n;
            }
        }
        int rc = rbuf_fill(rb, fd);
        if (rc != 0) return -1 - (rc == 1); /* -1 need more, -2 eof */
    }
}

static int hdr_get(const char *headers, const char *name, char *out, size_t outsz) {
    size_t nlen = strlen(name);
    const char *p = headers;
    while (*p) {
        const char *eol = strstr(p, "\r\n");
        size_t ll = eol ? (size_t)(eol - p) : strlen(p);
        if (ll > nlen && strncasecmp(p, name, nlen) == 0 && p[nlen] == ':') {
            const char *v = p + nlen + 1;
            size_t vl = ll - nlen - 1;
            while (vl > 0 && (*v == ' ' || *v == '\t')) { v++; vl--; }
            while (vl > 0 && (v[vl - 1] == ' ' || v[vl - 1] == '\t')) vl--;
            if (vl >= outsz) vl = outsz - 1;
            memcpy(out, v, vl);
            out[vl] = 0;
            return 1;
        }
        if (!eol) break;
        p = eol + 2;
    }
    return 0;
}

static int read_response(int fd, webdb_response *r) {
    rbuf rb = {0};
    buf head = {0};
    const char *line;
    long n;
    int status = -1;

    /* status line */
    while ((n = line_next(&rb, fd, &line)) == -1) {}
    if (n < 0) { set_err("connection closed before status line"); goto fail; }
    if (n >= 12 && strncmp(line, "HTTP/", 5) == 0) status = atoi(line + 9);
    /* headers until empty line; keep raw for lookups */
    for (;;) {
        long hl = line_next(&rb, fd, &line);
        while (hl == -1) {}
        if (hl < 0) { set_err("connection closed in headers"); goto fail; }
        if (hl == 0) break;
        if (buf_add(&head, line, (size_t)hl) != 0) goto fail;
        if (buf_add(&head, "\r\n", 2) != 0) goto fail;
    }

    char te[32] = "", cl[32] = "";
    hdr_get(head.p ? head.p : "", "Transfer-Encoding", te, sizeof(te));
    hdr_get(head.p ? head.p : "", "Content-Length", cl, sizeof(cl));

    buf body = {0};
    int chunked = (strstr(te, "chunked") != NULL);

    if (chunked) {
        for (;;) {
            long l = line_next(&rb, fd, &line);
            while (l == -1) {}
            if (l < 0) { set_err("truncated chunked body"); buf_free(&body); goto fail; }
            /* size line may carry extensions after ';' */
            char *endp = NULL;
            unsigned long sz = strtoul(line, &endp, 16);
            if (sz == 0) break;
            /* ensure sz data bytes available, pulling as needed */
            for (;;) {
                size_t avail = rb.b.len - rb.pos;
                if (avail >= sz) break;
                int rc = rbuf_fill(&rb, fd);
                if (rc != 0) { set_err("truncated chunk data"); buf_free(&body); goto fail; }
            }
            if (buf_add(&body, rb.b.p + rb.pos, sz) != 0) { buf_free(&body); goto fail; }
            rb.pos += sz;
            /* trailing CRLF */
            while (rb.pos + 1 >= rb.b.len) { if (rbuf_fill(&rb, fd) != 0) break; }
            if (rb.pos < rb.b.len && rb.b.p[rb.pos] == '\r') rb.pos++;
            if (rb.pos < rb.b.len && rb.b.p[rb.pos] == '\n') rb.pos++;
        }
    } else if (cl[0]) {
        size_t want = (size_t)strtoull(cl, NULL, 10);
        /* bytes already buffered after headers */
        if (rb.b.len - rb.pos > 0) {
            size_t have = rb.b.len - rb.pos;
            size_t take = have < want ? have : want;
            if (buf_add(&body, rb.b.p + rb.pos, take) != 0) goto fail;
            rb.pos += take;
        }
        while (body.len < want) {
            if (buf_reserve(&body, 8192) != 0) goto fail;
            ssize_t got = recv(fd, body.p + body.len, 8192, 0);
            if (got <= 0) { set_err("truncated body"); buf_free(&body); goto fail; }
            body.len += (size_t)got;
            body.p[body.len] = 0;
        }
        body.len = want;
        if (!body.p) { body.p = calloc(1, 1); }
        body.p[body.len] = 0;
    } else {
        /* close-delimited */
        if (rb.pos < rb.b.len) {
            if (buf_add(&body, rb.b.p + rb.pos, rb.b.len - rb.pos) != 0) goto fail;
        }
        for (;;) {
            if (buf_reserve(&body, 8192) != 0) goto fail;
            ssize_t got = recv(fd, body.p + body.len, 8192, 0);
            if (got < 0) { set_err("recv: %s", strerror(errno)); buf_free(&body); goto fail; }
            if (got == 0) break;
            body.len += (size_t)got;
            body.p[body.len] = 0;
        }
    }

    r->status = status;
    r->body = body.p ? body.p : calloc(1, 1);
    r->len = body.len;
    if (r->body) r->body[r->len] = 0;
    buf_free(&head);
    free(rb.b.p);
    return 0;
fail:
    buf_free(&head);
    buf_free(&rb.b);
    r->status = -1;
    r->error = strdup(g_err[0] ? g_err : "read error");
    return -1;
}

/* ---- request ------------------------------------------------------- */

int webdb_request(const webdb_client *c, const char *method, const char *path,
                  const char *json_body, webdb_response *r) {
    if (!c || !method || !path || !r) return -1;
    memset(r, 0, sizeof(*r));

    buf req = {0};
    const char *bp = c->base_path ? c->base_path : "";
    if (buf_puts(&req, method) != 0) goto oom;
    if (buf_puts(&req, " ") != 0) goto oom;
    if (buf_puts(&req, bp) != 0) goto oom;
    if (path[0] != '/' && buf_puts(&req, "/") != 0) goto oom;
    if (buf_puts(&req, path) != 0) goto oom;
    if (buf_puts(&req, " HTTP/1.1\r\nHost: ") != 0) goto oom;
    if (buf_puts(&req, c->host) != 0) goto oom;
    if (buf_puts(&req, ":") != 0) goto oom;
    if (buf_puts(&req, c->port) != 0) goto oom;
    if (buf_puts(&req, "\r\n") != 0) goto oom;
    if (c->token && buf_puts(&req, "Authorization: Bearer ") != 0) goto oom;
    if (c->token && buf_puts(&req, c->token) != 0) goto oom;
    if (c->token && buf_puts(&req, "\r\n") != 0) goto oom;
    if (json_body) {
        char tmp[64];
        if (buf_puts(&req, "Content-Type: application/json\r\n") != 0) goto oom;
        snprintf(tmp, sizeof(tmp), "Content-Length: %zu\r\n", strlen(json_body));
        if (buf_puts(&req, tmp) != 0) goto oom;
    }
    if (buf_puts(&req, "Accept: application/json\r\nConnection: close\r\n\r\n") != 0) goto oom;
    if (json_body && buf_puts(&req, json_body) != 0) goto oom;

    int fd = conn_open(c);
    if (fd < 0) { buf_free(&req); r->status = -1; r->error = strdup(g_err); return -1; }
    int rc = send_all(fd, req.p, req.len);
    buf_free(&req);
    if (rc != 0) {
        close(fd);
        r->status = -1;
        r->error = strdup(g_err);
        return -1;
    }
    rc = read_response(fd, r);
    close(fd);
    return rc;
oom:
    buf_free(&req);
    set_err("out of memory");
    r->status = -1;
    r->error = strdup(g_err);
    return -1;
}

/* ---- helpers ------------------------------------------------------- */

static int esc_path(const char *s, char *out, size_t outsz) {
    static const char *hex = "0123456789ABCDEF";
    size_t o = 0;
    for (const unsigned char *p = (const unsigned char *)s; *p; p++) {
        int unreserved = (*p >= 'A' && *p <= 'Z') || (*p >= 'a' && *p <= 'z') ||
                         (*p >= '0' && *p <= '9') || *p == '-' || *p == '_' ||
                         *p == '.' || *p == '~';
        if (unreserved) {
            if (o + 1 >= outsz) return -1;
            out[o++] = (char)*p;
        } else {
            if (o + 3 >= outsz) return -1;
            out[o++] = '%';
            out[o++] = hex[*p >> 4];
            out[o++] = hex[*p & 15];
        }
    }
    out[o] = 0;
    return 0;
}

int webdb_health(const webdb_client *c, webdb_response *r) {
    return webdb_request(c, "GET", "/health", NULL, r);
}
int webdb_stats(const webdb_client *c, webdb_response *r) {
    return webdb_request(c, "GET", "/v1/stats", NULL, r);
}
int webdb_flush(const webdb_client *c, webdb_response *r) {
    return webdb_request(c, "POST", "/v1/flush", NULL, r);
}

int webdb_create_collection(const webdb_client *c, const char *name, webdb_response *r) {
    char body[512];
    snprintf(body, sizeof(body), "{\"name\":\"%s\"}", name);
    return webdb_request(c, "POST", "/v1/collections", body, r);
}
int webdb_list_collections(const webdb_client *c, webdb_response *r) {
    return webdb_request(c, "GET", "/v1/collections", NULL, r);
}
int webdb_delete_collection(const webdb_client *c, const char *name, webdb_response *r) {
    char path[320];
    char enc[320];
    if (esc_path(name, enc, sizeof(enc)) != 0) return -1;
    snprintf(path, sizeof(path), "/v1/collections/%s", enc);
    return webdb_request(c, "DELETE", path, NULL, r);
}

int webdb_insert(const webdb_client *c, const char *collection,
                 const char *json_doc, webdb_response *r) {
    char path[320];
    char enc[320];
    if (esc_path(collection, enc, sizeof(enc)) != 0) return -1;
    snprintf(path, sizeof(path), "/v1/collections/%s/docs", enc);
    return webdb_request(c, "POST", path, json_doc, r);
}

int webdb_bulk_insert(const webdb_client *c, const char *collection,
                      const char *json_array, webdb_response *r) {
    return webdb_insert(c, collection, json_array, r);
}

int webdb_get(const webdb_client *c, const char *collection, const char *id,
              webdb_response *r) {
    char path[700], ec[300], ei[360];
    if (esc_path(collection, ec, sizeof(ec)) != 0) return -1;
    if (esc_path(id, ei, sizeof(ei)) != 0) return -1;
    snprintf(path, sizeof(path), "/v1/collections/%s/docs/%s", ec, ei);
    return webdb_request(c, "GET", path, NULL, r);
}

int webdb_list(const webdb_client *c, const char *collection, const char *query,
               webdb_response *r) {
    char path[700], ec[300];
    if (esc_path(collection, ec, sizeof(ec)) != 0) return -1;
    if (query && query[0])
        snprintf(path, sizeof(path), "/v1/collections/%s/docs?%s", ec, query);
    else
        snprintf(path, sizeof(path), "/v1/collections/%s/docs", ec);
    return webdb_request(c, "GET", path, NULL, r);
}

int webdb_put(const webdb_client *c, const char *collection, const char *id,
              const char *json_doc, int upsert, webdb_response *r) {
    char path[700], ec[300], ei[360];
    if (esc_path(collection, ec, sizeof(ec)) != 0) return -1;
    if (esc_path(id, ei, sizeof(ei)) != 0) return -1;
    if (upsert)
        snprintf(path, sizeof(path), "/v1/collections/%s/docs/%s?upsert=true", ec, ei);
    else
        snprintf(path, sizeof(path), "/v1/collections/%s/docs/%s", ec, ei);
    return webdb_request(c, "PUT", path, json_doc, r);
}

int webdb_patch(const webdb_client *c, const char *collection, const char *id,
                const char *json_patch, webdb_response *r) {
    char path[700], ec[300], ei[360];
    if (esc_path(collection, ec, sizeof(ec)) != 0) return -1;
    if (esc_path(id, ei, sizeof(ei)) != 0) return -1;
    snprintf(path, sizeof(path), "/v1/collections/%s/docs/%s", ec, ei);
    return webdb_request(c, "PATCH", path, json_patch, r);
}

int webdb_delete(const webdb_client *c, const char *collection, const char *id,
                 webdb_response *r) {
    char path[700], ec[300], ei[360];
    if (esc_path(collection, ec, sizeof(ec)) != 0) return -1;
    if (esc_path(id, ei, sizeof(ei)) != 0) return -1;
    snprintf(path, sizeof(path), "/v1/collections/%s/docs/%s", ec, ei);
    return webdb_request(c, "DELETE", path, NULL, r);
}

int webdb_sql(const webdb_client *c, const char *sql, const char *args_json,
              webdb_response *r) {
    /* build {"sql":"...","args":[...]} with minimal string escaping */
    buf b = {0};
    int rc = -1;
    if (buf_puts(&b, "{\"sql\":\"") != 0) goto done;
    for (const char *p = sql; *p; p++) {
        if (*p == '"' || *p == '\\') {
            char tmp[3] = {'\\', *p, 0};
            if (buf_puts(&b, tmp) != 0) goto done;
        } else if (*p == '\n') {
            if (buf_puts(&b, "\\n") != 0) goto done;
        } else {
            char tmp[2] = {*p, 0};
            if (buf_puts(&b, tmp) != 0) goto done;
        }
    }
    if (buf_puts(&b, "\"") != 0) goto done;
    if (args_json && args_json[0]) {
        if (buf_puts(&b, ",\"args\":") != 0) goto done;
        if (buf_puts(&b, args_json) != 0) goto done;
    }
    if (buf_puts(&b, "}") != 0) goto done;
    rc = webdb_request(c, "POST", "/v1/query", b.p, r);
done:
    buf_free(&b);
    return rc;
}

int webdb_create_index(const webdb_client *c, const char *collection,
                       const char *field, webdb_response *r) {
    char path[700], ec[300];
    if (esc_path(collection, ec, sizeof(ec)) != 0) return -1;
    snprintf(path, sizeof(path), "/v1/collections/%s/indexes?field=%s", ec, field);
    return webdb_request(c, "POST", path, NULL, r);
}

int webdb_list_indexes(const webdb_client *c, const char *collection,
                       webdb_response *r) {
    char path[700], ec[300];
    if (esc_path(collection, ec, sizeof(ec)) != 0) return -1;
    snprintf(path, sizeof(path), "/v1/collections/%s/indexes", ec);
    return webdb_request(c, "GET", path, NULL, r);
}

int webdb_drop_index(const webdb_client *c, const char *collection,
                     const char *field, webdb_response *r) {
    char path[700], ec[300];
    if (esc_path(collection, ec, sizeof(ec)) != 0) return -1;
    /* field is server-validated ([A-Za-z0-9_-]) — same as create_index. */
    snprintf(path, sizeof(path), "/v1/collections/%s/indexes/%s", ec, field);
    return webdb_request(c, "DELETE", path, NULL, r);
}
