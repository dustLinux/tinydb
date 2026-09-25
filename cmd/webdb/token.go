package main

// Подкоманда `webdb token` — выпуск подписанного JWT (HS256).
//
// Зачем она нужна: JWT проверяет сервер, а выпускать их может кто угодно, у кого
// есть секрет. Обычно это отдельная система (login-сервис, CI), но для
// разработки и тестов такой мини-выпускатель удобен: токен можно сделать вручную
// и не тащить внешнюю зависимость.
//
//   webdb token -sub alice -ttl 24h -scp "read write" -iss tinydb -aud api
//
// Секрет берётся из -secret, из файла -secret-file или из env WEBDB_JWT_SECRET
// (последнее — правильный способ, секрет не светится в истории команд).

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dustlinux/tinydb/internal/auth"
)

func runTokenCmd(args []string) int {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		secret    = fs.String("secret", "", "HMAC secret (prefer env WEBDB_JWT_SECRET)")
		secretF   = fs.String("secret-file", "", "read HMAC secret from file")
		sub       = fs.String("sub", "", "subject (who the token is for)")
		issuer    = fs.String("iss", "", "issuer claim")
		aud       = fs.String("aud", "", "audience claim")
		ttl       = fs.Duration("ttl", time.Hour, "token lifetime (0 = no exp claim)")
		nbf       = fs.Duration("nbf", 0, "not-before offset from now (0 = immediately)")
		scopes    = fs.String("scp", "", "scopes claim, space-separated, e.g. \"read write\"")
		jti       = fs.String("jti", "", "token id (random if empty)")
		fsNotUsed = fs.Bool("no-aud", false, "")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: webdb token [flags]

Выпускает JWT (HS256) для API-аутентификации tinydb.

Примеры:
  export WEBDB_JWT_SECRET=$(openssl rand -hex 32)
  webdb token -sub alice -ttl 24h -scp "read write"
  curl -H "Authorization: Bearer $(webdb token -sub bot -ttl 5m)" http://127.0.0.1:8080/v1/stats

Флаги:
`)
		fs.PrintDefaults()
	}
	_ = fsNotUsed
	if err := fs.Parse(args); err != nil {
		return 2
	}

	sec := []byte(*secret)
	if *secretF != "" {
		b, err := auth.SecretFromFile(*secretF)
		if err != nil {
			fmt.Fprintln(os.Stderr, "webdb token: read secret:", err)
			return 1
		}
		sec = b
	}
	if len(sec) == 0 {
		sec = auth.SecretFromEnv("WEBDB_JWT_SECRET")
	}
	if len(sec) == 0 {
		fmt.Fprintln(os.Stderr, "webdb token: нужен секрет: -secret, -secret-file или env WEBDB_JWT_SECRET")
		return 1
	}
	if len(sec) < 16 {
		fmt.Fprintln(os.Stderr, "webdb token: секрет короче 16 байт — HMAC-ключ слабый")
		return 1
	}

	now := time.Now()
	claims := map[string]any{
		"sub": *sub,
		"iat": now.Unix(),
		"jti": firstNonEmpty(*jti, auth.NewID()),
	}
	if *issuer != "" {
		claims["iss"] = *issuer
	}
	if *aud != "" {
		claims["aud"] = *aud
	}
	// ttl != 0 (в том числе отрицательный) — полезно для тестов: отрицательный
	// срок сразу даёт «просроченный» токен. ttl = 0 — без claim exp (вечный).
	if *ttl != 0 {
		claims["exp"] = now.Add(*ttl).Unix()
	}
	if *nbf > 0 {
		claims["nbf"] = now.Add(*nbf).Unix()
	}
	if s := strings.TrimSpace(*scopes); s != "" {
		claims["scp"] = s
	}
	if *sub == "" && *aud == "" && *issuer == "" {
		fmt.Fprintln(os.Stderr, "webdb token: задайте хотя бы -sub, -iss или -aud")
		return 1
	}

	tok, err := auth.Sign(sec, claims)
	if err != nil {
		fmt.Fprintln(os.Stderr, "webdb token:", err)
		return 1
	}
	fmt.Println(tok)
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
