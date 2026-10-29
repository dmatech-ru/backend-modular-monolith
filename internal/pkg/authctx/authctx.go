// Package authctx — access-токен (JWT, HS256) и перенос личности текущего
// пользователя через границы gateway → модуль, тем же приёмом, что
// internal/pkg/locale переносит локаль: gateway один раз проверяет подпись
// и срок токена (из HttpOnly-куки CookieAccessToken, см. ниже) и кладёт
// результат в доверенные заголовки (HeaderUserID/HeaderPermissions/
// HeaderSessionID) перед тем, как проксировать запрос в NATS — модулю не
// нужно ни знать секрет подписи, ни повторно его проверять, только прочитать
// заголовок через ServerInterceptor.
//
// Доверие к этим заголовкам целиком держится на том, что НИКАКОЙ клиент не
// может достучаться до модуля иначе, чем через gateway (NATS снаружи не
// виден) — gateway обязан стирать любые клиентские значения этих
// заголовков перед тем, как выставить настоящие (см. doc-комментарий
// handleAuth в gateway). Пакет не проверяет это сам: он только определяет
// имена заголовков и формат токена — тот же принцип, что у locale/validation,
// где перенос и интерпретация разнесены между транспортным и доменным кодом.
//
// Refresh-токены, ротация, identity.sessions — не здесь: это состояние,
// которое должно жить в БД и быть отзываемым, поэтому у него нет места в
// самоподписанном JWT. См. internal/modules/identity/session.go.
package authctx

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
)

const (
	// HeaderUserID/HeaderPermissions/HeaderSessionID — заголовки, которыми
	// gateway сообщает модулю результат проверки access-токена. Заполнены
	// только если токен был и прошёл проверку; для публичных методов,
	// вызванных без токena, пусты.
	HeaderUserID      = "X-Identity-User-Id"
	HeaderPermissions = "X-Identity-Permissions"
	HeaderSessionID   = "X-Identity-Session-Id"

	// CookieAccessToken — HttpOnly, подписанный JWT (см. Issuer/Verifier).
	CookieAccessToken = "access_token"
	// CookieRefreshToken — HttpOnly, непрозрачный токен ротации (см.
	// internal/modules/identity/session.go), в JWT не кодируется.
	CookieRefreshToken = "refresh_token"
	// CookieSessionInfo — НЕ HttpOnly, зеркало части claims для фронтенда
	// (список прав для UI и т.п.) — сервер её никогда не читает и не
	// доверяет ей: подделать эту куку в devtools можно, но это не даёт
	// ничего, кроме неправильно нарисованной кнопки, настоящая проверка
	// всегда идёт через CookieAccessToken.
	CookieSessionInfo = "session_info"
)

// Claims — личность запроса: либо распарсенная из access-токена (gateway,
// Verifier.Verify), либо восстановленная модулем из доверенных заголовков
// (ServerInterceptor/FromContext).
type Claims struct {
	UserID string
	// Permissions — тот же формат, что в identity.users.permissions
	// (битсет в base64): смысл битов знает только identity, здесь это
	// непрозрачная строка, которая просто путешествует вместе с запросом.
	Permissions string
	// SessionID — jti, id строки identity.sessions, которой принадлежит
	// refresh-токен, породивший этот access-токен. Не для проверки прав,
	// только для диагностики/аудита на стороне модулей, которым это
	// понадобится.
	SessionID string
}

// ErrInvalidToken — токен отсутствует, просрочен, подписан неверным
// секретом или использует неожидаемый алгоритм подписи.
var ErrInvalidToken = errors.New("authctx: invalid or expired access token")

// minSecretBytes — минимальная длина AUTH_JWT_SECRET после hex-декодирования
// (см. DecodeSecret): 256 бит, тот же запас, что antiEnumerationKey у
// identity (см. decodeAntiEnumKey) — короткий секрет сделал бы подбор
// подписи практичным.
const minSecretBytes = 32

// DecodeSecret декодирует AUTH_JWT_SECRET (hex) из конфига identity/gateway
// и проверяет его длину — общая проверка для Issuer и Verifier, которые
// читают один и тот же секрет из своих, формально независимых, конфигов
// (см. doc-комментарий AuthJWTSecret в gateway/config.go).
func DecodeSecret(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return nil, fmt.Errorf("authctx: AUTH_JWT_SECRET: invalid hex: %w", err)
	}
	if len(key) < minSecretBytes {
		return nil, fmt.Errorf("authctx: AUTH_JWT_SECRET: must be at least %d bytes, got %d", minSecretBytes, len(key))
	}
	return key, nil
}

// claims — форма, в которой Claims на самом деле сериализуются в JWT.
type claims struct {
	jwt.RegisteredClaims
	Permissions string `json:"perm,omitempty"`
}

// Issuer выпускает access-токены. Один на процесс (identity) — секрет
// общий с Verifier на стороне gateway (см. AUTH_JWT_SECRET в конфигах
// обоих модулей: единственный секрет в проекте, общий не с одним
// конкретным модулем, как antiEnumerationKey, а с gateway, поэтому он вне
// обычного per-модульного префикса переменных окружения).
type Issuer struct {
	secret []byte
	ttl    time.Duration
}

func NewIssuer(secret []byte, ttl time.Duration) *Issuer {
	return &Issuer{secret: secret, ttl: ttl}
}

// Issue подписывает новый access-токен для c. expiresAt — когда он
// перестанет быть валидным (нужно вызывающему коду, чтобы выставить Expires
// у куки той же длительности).
func (i *Issuer) Issue(c Claims) (token string, expiresAt time.Time, err error) {
	now := time.Now()
	expiresAt = now.Add(i.ttl)

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   c.UserID,
			ID:        c.SessionID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		Permissions: c.Permissions,
	})
	signed, err := tok.SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authctx: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// Verifier проверяет access-токены. Один на процесс (gateway).
type Verifier struct {
	secret []byte
}

func NewVerifier(secret []byte) *Verifier {
	return &Verifier{secret: secret}
}

// Verify проверяет подпись и срок токена и возвращает claims. Алгоритм
// подписи зафиксирован явно (jwt.WithValidMethods) — классическая защита
// от подмены алгоритма (например, "alg: none"), а не просто доверие
// заголовку токена.
func (v *Verifier) Verify(token string) (Claims, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) {
		return v.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Name}))
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	return Claims{UserID: c.Subject, Permissions: c.Permissions, SessionID: c.ID}, nil
}

type ctxKey struct{}

// NewContext кладёт claims в ctx — см. ServerInterceptor.
func NewContext(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext читает claims из ctx. ok=false — запрос анонимный (публичный
// метод, вызванный без токена, либо токен не прошёл проверку на gateway) —
// это не ошибка пакета, обработчик сам решает, обязателен ли для него
// авторизованный вызывающий.
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claims)
	return c, ok
}

// ServerInterceptor читает доверенные заголовки (выставленные gateway
// после проверки access-токена, см. doc-комментарий пакета) и кладёт их в
// context.Context. Если HeaderUserID пуст (публичный метод, вызванный без
// токена), ctx остаётся без Claims — FromContext вернёт ok=false.
func ServerInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if userID := req.Header().Get(HeaderUserID); userID != "" {
				ctx = NewContext(ctx, Claims{
					UserID:      userID,
					Permissions: req.Header().Get(HeaderPermissions),
					SessionID:   req.Header().Get(HeaderSessionID),
				})
			}
			return next(ctx, req)
		}
	}
}
