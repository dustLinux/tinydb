// Package auth реализует проверку JWT (HS256) для API-сервера.
//
// Зачем он нужен, если уже есть статический токен: статический токен — это
// «ключ от всего». Обычно приложению нужны разные права, срок жизни и отзыв
// отдельных выданных токенов. JWT даёт это без хранения состояния: подписанный
// секретом токен проверяется на сервере за O(1), а выдавать и отзывать его
// может внешняя система.
//
// Осознанные решения:
//   - только HS256 и никаких других alg. Алгоритм берётся только из
//     нашего конфига, а не из заголовка токена, поэтому «alg: none» и
//     подмена HS/RS невозможны в принципе;
//   - подпись сравнивается constant-time;
//   - проверяются exp/nbf/iat (с небольшим допуском на рассинхрон часов) и,
//     если заданы, iss/aud;
//   - только стандартная библиотека — проект держит нулевые зависимости.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Алгоритм единственный и фиксированный.
const algHS256 = "HS256"

// Ошибки проверки (сервер отвечает на все одинаково — 401).
var (
	ErrMalformed = errors.New("jwt: malformed token")
	ErrSignature = errors.New("jwt: bad signature")
	ErrExpired   = errors.New("jwt: expired")
	ErrNotYet    = errors.New("jwt: not valid yet")
	ErrIssuer    = errors.New("jwt: unexpected issuer")
	ErrAudience  = errors.New("jwt: unexpected audience")
)

// Claims — подтверждённый набор утверждений. Значения проверяются в Verify,
// наружу отдаётся только то, чему можно доверять.
type Claims struct {
	Subject   string
	Issuer    string
	Audience  string
	Expires   time.Time
	NotBefore time.Time
	IssuedAt  time.Time
	ID        string
	// Scopes — опциональный список прав ("read", "write", …). Сервер их пока
	// не применяет к маршрутам: значение — входные данные для приложения.
	Scopes []string
}

// Config задаёт, чем проверяем.
type Config struct {
	// Secret — HMAC-ключ. Пустой = JWT-аутентификация выключена.
	Secret []byte
	// Issuer, если непусто — требуется совпадение claim "iss".
	Issuer string
	// Audience, если непусто — требуется совпадение claim "aud".
	Audience string
	// Leeway допускает рассинхрон часов (по умолчанию 30s).
	Leeway time.Duration
}

// Enabled сообщает, включена ли JWT-аутентификация.
func (c Config) Enabled() bool { return len(c.Secret) > 0 }

func (c Config) leeway() time.Duration {
	if c.Leeway > 0 {
		return c.Leeway
	}
	return 30 * time.Second
}

// Verify проверяет подпись и срок действия токена.
func (c Config) Verify(token string) (*Claims, error) {
	if !c.Enabled() {
		return nil, errors.New("jwt: disabled")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, ErrMalformed
	}
	// Заголовок должен явно называть HS256. «none» и прочие отвергаются.
	if header.Alg != algHS256 {
		return nil, ErrSignature
	}
	wantMAC := sign(c.Secret, parts[0]+"."+parts[1])
	gotMAC, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	if !hmac.Equal(wantMAC, gotMAC) {
		return nil, ErrSignature
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var raw struct {
		Sub string          `json:"sub"`
		Iss string          `json:"iss"`
		Aud json.RawMessage `json:"aud"`
		Exp json.Number     `json:"exp"`
		Nbf json.Number     `json:"nbf"`
		Iat json.Number     `json:"iat"`
		Jti string          `json:"jti"`
		Scp json.RawMessage `json:"scp"`
	}
	dec := json.NewDecoder(strings.NewReader(string(payloadRaw)))
	if err := dec.Decode(&raw); err != nil {
		return nil, ErrMalformed
	}
	cl := &Claims{Subject: raw.Sub, Issuer: raw.Iss, ID: raw.Jti}

	// aud может быть строкой или массивом строк.
	var auds []string
	if len(raw.Aud) > 0 {
		var one string
		if err := json.Unmarshal(raw.Aud, &one); err == nil {
			auds = []string{one}
		} else if err := json.Unmarshal(raw.Aud, &auds); err != nil {
			return nil, ErrMalformed
		}
	}
	switch len(auds) {
	case 0:
	case 1:
		cl.Audience = auds[0]
	default:
		cl.Audience = strings.Join(auds, ",")
	}

	now := time.Now()
	var errClaim bool
	if s := raw.Exp.String(); s != "" {
		sec, err := secOf(raw.Exp)
		if err != nil {
			return nil, ErrMalformed
		}
		cl.Expires = time.Unix(sec, 0)
		if now.After(cl.Expires.Add(c.leeway())) {
			return nil, ErrExpired
		}
	}
	if s := raw.Nbf.String(); s != "" {
		sec, err := secOf(raw.Nbf)
		if err != nil {
			return nil, ErrMalformed
		}
		cl.NotBefore = time.Unix(sec, 0)
		if now.Add(c.leeway()).Before(cl.NotBefore) {
			return nil, ErrNotYet
		}
	}
	if s := raw.Iat.String(); s != "" {
		sec, err := secOf(raw.Iat)
		if err != nil {
			return nil, ErrMalformed
		}
		cl.IssuedAt = time.Unix(sec, 0)
	}
	_ = errClaim

	if c.Issuer != "" && cl.Issuer != c.Issuer {
		return nil, ErrIssuer
	}
	if c.Audience != "" && !contains(auds, c.Audience) {
		return nil, ErrAudience
	}

	// scp бывает строкой ("read write") или массивом.
	if len(raw.Scp) > 0 {
		var one string
		if err := json.Unmarshal(raw.Scp, &one); err == nil {
			cl.Scopes = strings.Fields(one)
		} else if err := json.Unmarshal(raw.Scp, &cl.Scopes); err != nil {
			return nil, ErrMalformed
		}
	}
	return cl, nil
}

func secOf(n json.Number) (int64, error) {
	f, err := n.Float64()
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func sign(secret []byte, signingInput string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}

// Sign собирает подписанный токен. Используется утилитой `webdb token` и
// тестами: сервер только проверяет, но держать выдачу в одном месте удобно.
func Sign(secret []byte, claims map[string]any) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("jwt: пустой секрет")
	}
	head, err := json.Marshal(map[string]string{"alg": algHS256, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(head) + "." + enc.EncodeToString(body)
	return signingInput + "." + enc.EncodeToString(sign(secret, signingInput)), nil
}

// SecretFromEnv читает секрет из переменной окружения (удобно для docker/systemd).
func SecretFromEnv(env string) []byte {
	if v := os.Getenv(env); v != "" {
		return []byte(v)
	}
	return nil
}

// SecretFromFile читает секрет из файла, обрезая завершающий перевод строки.
func SecretFromFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(string(b), "\r\n")), nil
}

// NewID возвращает случайный идентификатор токена (jti).
func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", b[:])
}

// ConstantTimeEqual — вспомогательное для сравнения произвольных секретов.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
