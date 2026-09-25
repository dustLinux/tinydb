# webdb C client library
# Targets: libwebdb.a libwebdb.so example size-check test
# Size budget: each artifact must stay under 512 KiB (WEBDB_LIB_MAX_BYTES).

CC      ?= clang
CFLAGS  ?= -O2 -std=c99 -Wall -Wextra
CFLAGS  += -fPIC
PREFIX  ?= .

OBJS = webdb.o

all: libwebdb.a libwebdb.so example

webdb.o: webdb.c webdb.h
	$(CC) $(CFLAGS) -c webdb.c -o webdb.o

libwebdb.a: $(OBJS)
	ar rcs $@ $(OBJS)

libwebdb.so: $(OBJS)
	$(CC) -shared -o $@ $(OBJS)

example: example.c libwebdb.a
	$(CC) $(CFLAGS) example.c libwebdb.a -o example

size-check: libwebdb.a libwebdb.so
	@ok=1; \
	for f in libwebdb.a libwebdb.so; do \
	  sz=$$(stat -c %s $$f); \
	  if [ $$sz -gt 524288 ]; then \
	    echo "FAIL $$f = $$sz bytes > 524288"; ok=0; \
	  else \
	    echo "ok   $$f = $$sz bytes (<= 512KiB)"; \
	  fi; \
	done; [ $$ok -eq 1 ]

# Live test against a running server:
#   make test BASE=http://127.0.0.1:8099 TOKEN=$$(cat ../data/token)
test: example size-check
	./example $(BASE) $(TOKEN)

clean:
	rm -f $(OBJS) libwebdb.a libwebdb.so example

.PHONY: all size-check test clean
