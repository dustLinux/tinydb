// Command webdb is the tinydb server: a tiny SQLite-backed database with a
// REST API, streaming AES-256-GCM encryption at rest and a ~10 MiB RSS budget.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/dustlinux/tinydb/internal/logx"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/dustlinux/tinydb/internal/auth"
	"github.com/dustlinux/tinydb/internal/httpx"
	"github.com/dustlinux/tinydb/internal/server"
	"github.com/dustlinux/tinydb/internal/store"
)

func main() {
	// Подкоманда `webdb token` — выпуск JWT. Работает без запуска сервера и
	// без каталога данных: нужен только секрет.
	if len(os.Args) > 1 && os.Args[1] == "token" {
		os.Exit(runTokenCmd(os.Args[2:]))
	}
	// On GOOS=android the Go runtime defaults to MADV_FREE: freed pages stay
	// accounted in RSS until the kernel reclaims them, which breaks the 10 MiB
	// budget. GODEBUG is only read at exec time, so re-exec once with
	// madvdontneed=1 (MADV_DONTNEED returns pages to the kernel eagerly).
	if gd := os.Getenv("GODEBUG"); !strings.Contains(gd, "madvdontneed=") {
		newGD := "madvdontneed=1"
		if gd != "" {
			newGD = gd + ",madvdontneed=1"
		}
		env := make([]string, 0, len(os.Environ())+2)
		for _, e := range os.Environ() {
			// Drop GODEBUG (replaced below) and LD_PRELOAD: Termux's
			// libtermux-exec shim is only useful for exec'ing scripts and
			// costs ~52 KiB of RSS in a server that never execs.
			if strings.HasPrefix(e, "GODEBUG=") || strings.HasPrefix(e, "LD_PRELOAD=") {
				continue
			}
			env = append(env, e)
		}
		env = append(env, "GODEBUG="+newGD)
		if exe, err := os.Executable(); err == nil {
			_ = syscall.Exec(exe, os.Args, env)
			// exec failed: fall through and run without it
		}
	}

	var (
		addr       = flag.String("addr", "127.0.0.1:8080", "listen address (loopback by default; pass :PORT or 0.0.0.0:PORT to expose)")
		dataDir    = flag.String("data", "./data", "data directory (db.sqlite, db.sqlite.enc, db.key)")
		maxSizeMiB = flag.Int64("max-size", 100, "storage quota in MiB (disk)")
		cacheKiB   = flag.Int("cache-kb", 64, "SQLite page cache in KiB per connection (RAM budget)")
		goMemMiB   = flag.Int("go-memlimit", 2, "Go heap limit in MiB, 0 = off (RAM budget)")
		autosave   = flag.Duration("autosave", 60*time.Second, "write encrypted snapshot every N (0 = only on shutdown/manual flush)")
		writeback  = flag.Duration("writeback", time.Second, "apply RAM write-back buffer to SQLite every N (0 = only on read barriers/limits/flush/shutdown)")
		bufBytes   = flag.Int("buffer-bytes", 1<<20, "write-back buffer cap in bytes (RAM)")
		bufItems   = flag.Int("buffer-items", 10000, "write-back buffer cap in mutations (RAM)")
		keepPlain  = flag.Bool("keep-plain", false, "keep plaintext db.sqlite after shutdown")
		maxBodyMiB = flag.Int64("max-body", 4, "max request body in MiB")
		maxImport  = flag.Int64("max-import", 64, "max /v1/import upload in MiB (streamed, not buffered in RAM)")
		cors       = flag.Bool("cors", false, "enable permissive CORS")
		tokenFlag  = flag.String("token", "", "API token (or env WEBDB_TOKEN); default: generated to <data>/token")
		tokenFile  = flag.String("token-file", "", "token file (default <data>/token)")
		noAuth     = flag.Bool("no-auth", false, "disable API authentication (local development only)")
		keyFile    = flag.String("key-file", "", "32-byte key file (default <data>/db.key)")
		passEnv    = flag.String("passphrase", "", "encrypt with passphrase instead of key file (prefer env WEBDB_PASSPHRASE)")
		passFile   = flag.String("passphrase-file", "", "read passphrase from file")
		jwtSecret  = flag.String("jwt-secret", "", "also accept signed JWTs (HS256; prefer env WEBDB_JWT_SECRET)")
		jwtSecretF = flag.String("jwt-secret-file", "", "read JWT secret from file")
		jwtIssuer  = flag.String("jwt-issuer", "", "require JWT claim 'iss' to equal this")
		jwtAud     = flag.String("jwt-audience", "", "require JWT claim 'aud' to contain this")
		jwtLeeway  = flag.Duration("jwt-leeway", 30*time.Second, "clock skew allowance for exp/nbf")
	)
	flag.Parse()

	log := logx.New(os.Stderr, logx.LevelInfo)
	logx.SetDefault(log)

	// Hard RAM ceiling: keeps the Go heap (and therefore RSS) inside the
	// 10 MiB budget together with the SQLite cache. GC runs earlier too.
	if *goMemMiB > 0 {
		debug.SetMemoryLimit(int64(*goMemMiB) << 20)
		debug.SetGCPercent(30) // smaller heap spikes inside the RSS budget
	}

	// Passphrase resolution: flag < file < env.
	passphrase := *passEnv
	if passphrase == "" {
		if *passFile != "" {
			b, err := os.ReadFile(*passFile)
			if err != nil {
				log.Error("read passphrase file", "err", err)
				os.Exit(1)
			}
			passphrase = string(trimNewline(b))
		} else {
			passphrase = os.Getenv("WEBDB_PASSPHRASE")
		}
	}

	// Key file (only when no passphrase).
	keyPath := *keyFile
	if keyPath == "" {
		keyPath = filepath.Join(*dataDir, "db.key")
	}
	var key []byte
	if passphrase == "" {
		k, err := loadOrCreateKey(keyPath)
		if err != nil {
			log.Error("key file", "err", err, "path", keyPath)
			os.Exit(1)
		}
		key = k
	}

	st, err := store.Open(store.Config{
		Dir:           *dataDir,
		MaxBytes:      *maxSizeMiB << 20,
		Key:           key,
		Passphrase:    passphrase,
		KeepPlain:     *keepPlain,
		CacheKiB:      *cacheKiB,
		BufferBytes:   *bufBytes,
		BufferItems:   *bufItems,
		GoMemLimitMiB: *goMemMiB,
		Logger:        log,
	})
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}

	// API token.
	token := *tokenFlag
	if token == "" {
		token = os.Getenv("WEBDB_TOKEN")
	}
	if *noAuth {
		token = ""
		log.Warn("authentication disabled (-no-auth)")
	} else if token == "" {
		tf := *tokenFile
		if tf == "" {
			tf = filepath.Join(*dataDir, "token")
		}
		if b, err := os.ReadFile(tf); err == nil && len(trimNewline(b)) > 0 {
			token = string(trimNewline(b))
		} else {
			token = randomToken()
			if err := os.WriteFile(tf, []byte(token+"\n"), 0o600); err != nil {
				log.Error("write token file", "err", err)
				os.Exit(1)
			}
		}
		// Never log the token itself: logs end up in files and bug reports.
		log.Info("API token loaded", "file", tf)
	}

	// Autosave (encrypted snapshot in background) + write-back flusher.
	stopAuto := make(chan struct{})
	stopWB := make(chan struct{})
	go st.Autosave(*autosave, stopAuto)
	go st.Writeback(*writeback, stopWB)

	// JWT (необязательно): принимать подписанные токены в дополнение к статиченому.
	jwtCfg := auth.Config{Issuer: *jwtIssuer, Audience: *jwtAud, Leeway: *jwtLeeway}
	if s := auth.SecretFromEnv("WEBDB_JWT_SECRET"); len(s) > 0 && *jwtSecret == "" && *jwtSecretF == "" {
		jwtCfg.Secret = s
	}
	if *jwtSecret != "" {
		jwtCfg.Secret = []byte(*jwtSecret)
	}
	if *jwtSecretF != "" {
		b, err := auth.SecretFromFile(*jwtSecretF)
		if err != nil {
			log.Error("read jwt secret file", "err", err)
			os.Exit(1)
		}
		jwtCfg.Secret = b
	}
	if jwtCfg.Enabled() && len(jwtCfg.Secret) < 16 {
		log.Warn("JWT secret is shorter than 16 bytes — HMAC key is weak")
	}
	if !*noAuth && jwtCfg.Enabled() {
		log.Info("JWT auth enabled", "issuer", jwtCfg.Issuer, "audience", jwtCfg.Audience)
	}

	h := server.New(server.Config{
		Store:     st,
		Token:     token,
		MaxBody:   *maxBodyMiB << 20,
		MaxImport: *maxImport << 20,
		CORS:      *cors,
		Logger:    log,
		JWT:       jwtCfg,
	})
	srv := httpx.NewServer(httpx.ServerConfig{
		Addr:     *addr,
		Handler:  h,
		IdleTO:   60 * time.Second,
		ErrorLog: func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) },
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("tinydb listening",
			"addr", *addr,
			"version", server.Version,
			"data", *dataDir,
			"quota_mib", *maxSizeMiB,
			"autosave", autosave.String(),
			"writeback", writeback.String(),
			"buffer_bytes", *bufBytes,
			"buffer_items", *bufItems,
			"auth", token != "",
			"go", runtime.GOOS+"/"+runtime.GOARCH)
		errCh <- srv.ListenAndServe()
	}()

	var exitCode int
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			log.Error("server error", "err", err)
			exitCode = 1
		}
		stop()
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	close(stopAuto)
	close(stopWB)
	if err := st.Close(); err != nil {
		log.Error("store close/flush", "err", err)
		exitCode = 1
	}
	if exitCode == 0 {
		log.Info("bye (encrypted snapshot saved)")
	}
	os.Exit(exitCode)
}

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != 32 {
			return nil, fmt.Errorf("key file %s must be exactly 32 bytes (got %d)", path, len(b))
		}
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, k, 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
