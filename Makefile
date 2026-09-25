# tinydb — сборка, тесты, размеры.
#
#   make build     — бинарь bin/webdb (release-флаги + проверка ≤10MiB)
#   make test      — gofmt + vet + e2e smoke + клиентские тесты (Go и C)
#   make run       — запуск сервера (127.0.0.1:8099, ./data)
#   make clients   — либы Go и C (+ size-check C ≤ 512KiB)
#   make check     — build + test (полная верификация)
#   make clean     — артефакты
#
# Тесты самодостаточны: smoke и C-example сами поднимают сервер,
# если он не запущен (tests/lib.sh).

GO      ?= go
BIN     := bin/webdb
ADDR    ?= 127.0.0.1:8099
DATA    ?= ./data
# -tags libsqlite3: динамическая линковка с системным SQLite (RAM/размер).
# -gcflags=all=-B: без проверок границ в пакетах (~77KiB text).
# -ldflags "-s -w": стрип symtab/debug.
GOFLAGS := -tags libsqlite3 -gcflags=all=-B -ldflags "-s -w"
# Лимит бинаря (байт) — лимит из README «Лимиты».
BIN_MAX := 10485760

.PHONY: all build run test vet fmt-check smoke clients clients-go clients-c \
        check clean

all: build

build:
	$(GO) build $(GOFLAGS) -o $(BIN).tmp ./cmd/webdb
	mv $(BIN).tmp $(BIN)
	@sz=$$(wc -c < $(BIN)); \
	if [ "$$sz" -gt $(BIN_MAX) ]; then \
		echo "FAIL bin/webdb = $$sz > $(BIN_MAX)"; exit 1; \
	fi; \
	echo "ok   bin/webdb = $$sz bytes (<= 10MiB)"

run: build
	./$(BIN) -addr $(ADDR) -data $(DATA) -writeback 1s

vet:
	$(GO) vet ./...

fmt-check:
	@out=$$(gofmt -l cmd internal client-go 2>/dev/null); \
	if [ -n "$$out" ]; then echo "FAIL gofmt:"; echo "$$out"; exit 1; fi
	@echo "ok   gofmt"

# e2e: 44 проверки, включая write-back и RSS ≤ 10MiB (RSS_MAX_KB переопределяем).
# Поднимает сервер сам, если нужно; чужой работающий сервер не трогает.
smoke:
	bash tests/smoke.sh

# Тесты безопасности: auth, SQL-инъекции, валидации, права файлов,
# tamper/wrong-key, passphrase, отсутствие токена в логе, CORS.
security:
	bash tests/security.sh

# Интеграционный тест Go-клиента: сам поднимает bin/webdb на :18099.
clients-go:
	cd client-go && $(GO) build ./... && $(GO) test ./...

# C: сборка либ + size-check (≤512KiB) + прогон example против живого сервера.
clients-c:
	$(MAKE) -C client-c all size-check
	bash tests/c-example.sh

clients: clients-go clients-c

test: fmt-check vet smoke security clients

# Полная верификация: свежая сборка + все тесты.
check: build test
	@echo "CHECK: ok (bin/webdb $$(wc -c < $(BIN)) bytes)"

clean:
	rm -f $(BIN) $(BIN).tmp
	$(MAKE) -C client-c clean
	cd client-go && $(GO) clean ./...
