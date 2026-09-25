package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("test-secret-32-bytes-long-xxxxx")

func cfg() Config {
	return Config{Secret: testSecret}
}

func TestSignAndVerify(t *testing.T) {
	tok, err := Sign(testSecret, map[string]any{
		"sub": "alice",
		"iss": "tinydb",
		"aud": "api",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
		"jti": NewID(),
		"scp": "read write",
	})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := cfg().Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if cl.Subject != "alice" || cl.Issuer != "tinydb" || cl.Audience != "api" {
		t.Fatalf("claims: %+v", cl)
	}
	if len(cl.Scopes) != 2 || cl.Scopes[0] != "read" || cl.Scopes[1] != "write" {
		t.Fatalf("scopes: %v", cl.Scopes)
	}
	if cl.ID == "" {
		t.Fatal("пустой jti")
	}
}

func TestAudienceArray(t *testing.T) {
	c := Config{Secret: testSecret, Audience: "api2"}
	tok, _ := Sign(testSecret, map[string]any{"aud": []string{"api1", "api2"}, "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := c.Verify(tok); err != nil {
		t.Fatalf("массив aud должен приниматься: %v", err)
	}
}

// alg: none — классическая атака: сервер не должен доверять alg из заголовка.
func TestAlgNoneRejected(t *testing.T) {
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := enc.EncodeToString([]byte(`{"sub":"attacker"}`))
	tok := head + "." + body + "."
	if _, err := cfg().Verify(tok); err != ErrSignature {
		t.Fatalf("alg=none принят: %v", err)
	}
}

func TestAlgSwapRejected(t *testing.T) {
	// Заголовок говорит RS256, подпись HS256 — нельзя.
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body := enc.EncodeToString([]byte(`{"sub":"attacker"}`))
	si := head + "." + body
	tok := si + "." + enc.EncodeToString(sign(testSecret, si))
	if _, err := cfg().Verify(tok); err != ErrSignature {
		t.Fatalf("подмена алгоритма принята: %v", err)
	}
}

func TestWrongSecretRejected(t *testing.T) {
	tok, _ := Sign([]byte("other-secret"), map[string]any{"sub": "x", "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := cfg().Verify(tok); err != ErrSignature {
		t.Fatalf("чужой секрет принят: %v", err)
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	tok, _ := Sign(testSecret, map[string]any{"sub": "alice", "exp": time.Now().Add(time.Hour).Unix()})
	enc := base64.RawURLEncoding
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("плохой токен от Sign: %q", tok)
	}
	// Меняем sub в теле, подпись не трогаем.
	bad := enc.EncodeToString([]byte(`{"sub":"root","exp":` + itoa(time.Now().Add(time.Hour).Unix()) + `}`))
	tampered := parts[0] + "." + bad + "." + parts[2]
	if _, err := cfg().Verify(tampered); err != ErrSignature {
		t.Fatalf("подделанное тело принято: %v", err)
	}
}

func TestExpired(t *testing.T) {
	c := Config{Secret: testSecret, Leeway: time.Second}
	tok, _ := Sign(testSecret, map[string]any{"sub": "x", "exp": time.Now().Add(-time.Hour).Unix()})
	if _, err := c.Verify(tok); err != ErrExpired {
		t.Fatalf("просроченный принят: %v", err)
	}
	// С допуском секунда только что истёкший токен ещё принимается.
	tok2, _ := Sign(testSecret, map[string]any{"sub": "x", "exp": time.Now().Unix()})
	if _, err := c.Verify(tok2); err != nil {
		t.Fatalf("leeway не работает: %v", err)
	}
}

func TestNotYetValid(t *testing.T) {
	c := Config{Secret: testSecret, Leeway: time.Second}
	tok, _ := Sign(testSecret, map[string]any{"sub": "x", "nbf": time.Now().Add(time.Hour).Unix()})
	if _, err := c.Verify(tok); err != ErrNotYet {
		t.Fatalf("будущий nbf принят: %v", err)
	}
}

func TestIssuerAudienceMismatch(t *testing.T) {
	tok, _ := Sign(testSecret, map[string]any{
		"iss": "other", "aud": "other", "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := (Config{Secret: testSecret, Issuer: "tinydb"}).Verify(tok); err != ErrIssuer {
		t.Fatalf("issuer не проверен: %v", err)
	}
	if _, err := (Config{Secret: testSecret, Audience: "api"}).Verify(tok); err != ErrAudience {
		t.Fatalf("audience не проверен: %v", err)
	}
}

func TestMalformed(t *testing.T) {
	for _, tok := range []string{
		"", "abc", "a.b", "a.b.c.d", "!!.??.@@", "eyJhbGciOiJIUzI1NiJ9.###.sig",
	} {
		if _, err := cfg().Verify(tok); err == nil {
			t.Fatalf("мусорный токен принят: %q", tok)
		}
	}
}

func TestDisabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Fatal("пустой секрет должен выключать JWT")
	}
	if _, err := (Config{}).Verify("a.b.c"); err == nil {
		t.Fatal("при выключенном JWT проверка обязана падать")
	}
}

// --- утилиты ---

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
