/* example.c — end-to-end demo/test of the webdb C client.
 *
 * Usage: ./example <base-url> <token>
 * Exit code 0 = all checks passed.
 */
#define _POSIX_C_SOURCE 200809L
#include "webdb.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int fails = 0;

static void chk(const char *name, int ok, const webdb_response *r) {
    if (ok) {
        printf("  ok   %s\n", name);
    } else {
        printf("  FAIL %s (status=%d)%s%s\n", name, r ? r->status : -1,
               r && r->error ? " err=" : "", r && r->error ? r->error : "");
        if (r && r->body && r->body[0])
            printf("       body: %.200s\n", r->body);
        fails++;
    }
}

static int has(const webdb_response *r, const char *needle) {
    return r && r->body && strstr(r->body, needle) != NULL;
}

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "usage: %s <base-url> <token>\n", argv[0]);
        return 2;
    }
    webdb_client c;
    if (webdb_client_init(&c, argv[1], argv[2]) != 0) {
        fprintf(stderr, "init: %s\n", webdb_last_error());
        return 2;
    }
    webdb_response r;

    printf("== health\n");
    webdb_health(&c, &r);
    chk("health", r.status == 200 && has(&r, "\"ok\""), &r);
    webdb_response_free(&r);

    printf("== auth\n");
    {
        webdb_client bad;
        webdb_client_init(&bad, argv[1], "wrong");
        webdb_stats(&bad, &r);
        chk("stats401", r.status == 401, &r);
        webdb_response_free(&r);
        webdb_client_free(&bad);
    }

    printf("== collections\n");
    webdb_delete_collection(&c, "cdemo", &r); /* ignore result, reset */
    webdb_response_free(&r);
    webdb_create_collection(&c, "cdemo", &r);
    chk("create", r.status == 201 && has(&r, "\"created\":true"), &r);
    webdb_response_free(&r);
    webdb_create_collection(&c, "cdemo", &r);
    chk("create409", r.status == 409, &r);
    webdb_response_free(&r);
    webdb_list_collections(&c, &r);
    chk("list", r.status == 200 && has(&r, "\"cdemo\""), &r);
    webdb_response_free(&r);

    printf("== docs\n");
    webdb_insert(&c, "cdemo", "{\"title\":\"widget\",\"price\":10}", &r);
    chk("insert", r.status == 201 && has(&r, "\"_id\""), &r);
    /* extract id: "_id":"xxxx" */
    char id[80] = "";
    if (has(&r, "\"_id\":\"")) {
        const char *p = strstr(r.body, "\"_id\":\"") + 7;
        const char *e = strchr(p, '"');
        if (e && (size_t)(e - p) < sizeof(id)) {
            memcpy(id, p, (size_t)(e - p));
            id[e - p] = 0;
        }
    }
    webdb_response_free(&r);

    webdb_bulk_insert(&c, "cdemo",
                      "[{\"title\":\"gizmo\",\"price\":5},{\"title\":\"gadget\",\"price\":25}]", &r);
    chk("bulk", r.status == 201 && has(&r, "\"inserted\":2"), &r);
    webdb_response_free(&r);

    webdb_get(&c, "cdemo", id, &r);
    chk("get", r.status == 200 && has(&r, "\"widget\""), &r);
    webdb_response_free(&r);

    webdb_list(&c, "cdemo", "limit=10&order=price:asc", &r);
    chk("list+order", r.status == 200 && has(&r, "\"total\":3"), &r);
    webdb_response_free(&r);

    webdb_list(&c, "cdemo", "price=25", &r);
    chk("filter", r.status == 200 && has(&r, "\"total\":1"), &r);
    webdb_response_free(&r);

    webdb_put(&c, "cdemo", "nope", "{\"x\":1}", 0, &r);
    chk("put404", r.status == 404, &r);
    webdb_response_free(&r);

    webdb_put(&c, "cdemo", "k1", "{\"x\":1}", 1, &r);
    chk("upsert", r.status == 200 || r.status == 201, &r);
    webdb_response_free(&r);

    webdb_patch(&c, "cdemo", id, "{\"price\":99}", &r);
    chk("patch", r.status == 200 && has(&r, "\"price\":99"), &r);
    webdb_response_free(&r);

    webdb_delete(&c, "cdemo", "k1", &r);
    chk("delete", r.status == 200, &r);
    webdb_response_free(&r);

    printf("== sql\n");
    webdb_sql(&c, "SELECT COUNT(*) AS n FROM docs WHERE collection='cdemo'", NULL, &r);
    chk("select", r.status == 200 && has(&r, "\"n\""), &r);
    webdb_response_free(&r);
    webdb_sql(&c, "DELETE FROM docs", NULL, &r);
    chk("write400", r.status == 400, &r);
    webdb_response_free(&r);

    printf("== indexes\n");
    webdb_create_index(&c, "cdemo", "price", &r);
    chk("idx-create", r.status == 201, &r);
    webdb_response_free(&r);
    webdb_list_indexes(&c, "cdemo", &r);
    chk("idx-list", r.status == 200 && has(&r, "ix_cdemo_price"), &r);
    webdb_response_free(&r);
    webdb_drop_index(&c, "cdemo", "price", &r);
    chk("idx-drop", r.status == 200 && has(&r, "\"dropped\":\"price\""), &r);
    webdb_response_free(&r);
    webdb_list_indexes(&c, "cdemo", &r);
    chk("idx-list-after-drop", r.status == 200 && !has(&r, "ix_cdemo_price"), &r);
    webdb_response_free(&r);

    printf("== stats/flush\n");
    webdb_stats(&c, &r);
    chk("stats", r.status == 200 && has(&r, "\"encrypted\":true"), &r);
    webdb_response_free(&r);
    webdb_flush(&c, &r);
    chk("flush", r.status == 200, &r);
    webdb_response_free(&r);

    /* big payload: >64KB response must decode (chunked) */
    printf("== big response\n");
    {
        /* reset: test must be idempotent across consecutive runs */
        webdb_delete_collection(&c, "big", &r);
        webdb_response_free(&r);
        size_t cap = 1 << 18; /* ~262KB: 200 items * ~512 bytes */
        char *big = malloc(cap);
        size_t len = 0;
        len += (size_t)snprintf(big + len, cap - len, "[");
        for (int i = 0; i < 200; i++) {
            len += (size_t)snprintf(big + len, cap - len,
                                    "%s{\"pad\":\"", i ? "," : "");
            for (int k = 0; k < 500; k++) big[len++] = 'x';
            len += (size_t)snprintf(big + len, cap - len, "\"}");
        }
        snprintf(big + len, cap - len, "]");
        webdb_insert(&c, "big", big, &r);
        chk("bulk200", r.status == 201 && has(&r, "\"inserted\":200"), &r);
        webdb_response_free(&r);
        free(big);
        webdb_list(&c, "big", "limit=500", &r);
        chk("big-list", r.status == 200 && has(&r, "\"total\":200"), &r);
        webdb_response_free(&r);
    }

    webdb_client_free(&c);
    printf(fails ? "\nRESULT: FAIL (%d)\n" : "\nRESULT: PASS\n", fails);
    return fails ? 1 : 0;
}
