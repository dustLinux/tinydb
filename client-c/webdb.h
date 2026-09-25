/* webdb.h — minimal C client library for the tinydb REST API.
 *
 * - Plain HTTP/1.1 over TCP (getaddrinfo), no third-party deps.
 * - Bearer token auth, Content-Length requests, Content-Length /
 *   chunked / close-delimited response decoding.
 * - Responses are plain NUL-terminated JSON strings; parse them with
 *   your favourite JSON library (jansson/cJSON) or substring checks.
 *
 * Size budget: libwebdb.a and libwebdb.so must stay under 512 KiB
 * (enforced by the Makefile `size-check` target).
 *
 * Example:
 *
 *     webdb_client c;
 *     webdb_response r;
 *     webdb_client_init(&c, "http://127.0.0.1:8080", "token");
 *     if (webdb_health(&c, &r) == 0 && r.status == 200)
 *         printf("%.*s\\n", (int)r.len, r.body);
 *     webdb_response_free(&r);
 *     webdb_client_free(&c);
 */
#ifndef WEBDB_H
#define WEBDB_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

#define WEBDB_VERSION "1.0.0"
/* Hard size limit for built artifacts (bytes). */
#define WEBDB_LIB_MAX_BYTES 524288

typedef struct {
    char *host;      /* parsed from base URL */
    char *port;
    char *base_path; /* "" or "/prefix" (no trailing slash) */
    char *token;     /* bearer token or NULL */
} webdb_client;

typedef struct {
    int    status; /* HTTP status code, -1 on transport error */
    char  *body;   /* NUL-terminated response body (may be "") */
    size_t len;    /* body length in bytes (excluding NUL) */
    char  *error;  /* transport error message or NULL */
} webdb_response;

/* Lifecycle ---------------------------------------------------------- */

/* base_url: "http://host:port" or "http://host:port/path".
 * token:    bearer token or NULL when the server runs with -no-auth.
 * Returns 0 on success, -1 on bad URL / OOM (message via webdb_last_error). */
int  webdb_client_init(webdb_client *c, const char *base_url, const char *token);
void webdb_client_free(webdb_client *c);
void webdb_response_free(webdb_response *r);

/* Last transport-level error message (thread-local static buffer). */
const char *webdb_last_error(void);

/* Raw request -------------------------------------------------------- */

/* Perform an arbitrary request. json_body may be NULL (no body).
 * Returns 0 when a response was received (any status), -1 on transport
 * error (then r->status == -1 and r->error is set). */
int webdb_request(const webdb_client *c, const char *method, const char *path,
                  const char *json_body, webdb_response *r);

/* Convenience endpoints ---------------------------------------------- */

int webdb_health(const webdb_client *c, webdb_response *r);
int webdb_stats(const webdb_client *c, webdb_response *r);
int webdb_flush(const webdb_client *c, webdb_response *r);

int webdb_create_collection(const webdb_client *c, const char *name, webdb_response *r);
int webdb_list_collections(const webdb_client *c, webdb_response *r);
int webdb_delete_collection(const webdb_client *c, const char *name, webdb_response *r);

int webdb_insert(const webdb_client *c, const char *collection,
                 const char *json_doc, webdb_response *r);
int webdb_bulk_insert(const webdb_client *c, const char *collection,
                      const char *json_array, webdb_response *r);
int webdb_get(const webdb_client *c, const char *collection, const char *id,
              webdb_response *r);
/* query: extra query string without '?', e.g. "limit=10&order=price:desc"; may be NULL */
int webdb_list(const webdb_client *c, const char *collection, const char *query,
               webdb_response *r);
int webdb_put(const webdb_client *c, const char *collection, const char *id,
              const char *json_doc, int upsert, webdb_response *r);
int webdb_patch(const webdb_client *c, const char *collection, const char *id,
                const char *json_patch, webdb_response *r);
int webdb_delete(const webdb_client *c, const char *collection, const char *id,
                 webdb_response *r);

/* sql: SELECT statement. args_json: JSON array literal or NULL. */
int webdb_sql(const webdb_client *c, const char *sql, const char *args_json,
              webdb_response *r);

int webdb_create_index(const webdb_client *c, const char *collection,
                       const char *field, webdb_response *r);
int webdb_list_indexes(const webdb_client *c, const char *collection,
                       webdb_response *r);
/* field must match the server-side name rules: [A-Za-z0-9_-], <=64. */
int webdb_drop_index(const webdb_client *c, const char *collection,
                     const char *field, webdb_response *r);

#ifdef __cplusplus
}
#endif

#endif /* WEBDB_H */
