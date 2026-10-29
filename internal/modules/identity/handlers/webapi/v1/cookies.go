package v1

import (
	"net/http"
	"time"

	"dmatech.ru/internal/modules/identity/repository"
	"dmatech.ru/internal/pkg/authctx"
)

// setSessionCookies выставляет access/refresh/session_info куки на ответе
// после успешного входа/регистрации/ротации (см. doc-комментарий
// CookieAccessToken и соседей в internal/pkg/authctx). permissions едет
// открытым текстом в session_info — зеркало для UI, не источник истины:
// сервер эту куку никогда не читает обратно, подделать её в devtools
// можно, но это не даёт ничего, кроме неправильно нарисованной кнопки —
// настоящая проверка прав всегда идёт через CookieAccessToken на gateway.
func setSessionCookies(header http.Header, access, refresh, permissions string, accessExpiresAt time.Time) {
	header.Add("Set-Cookie", (&http.Cookie{
		Name: authctx.CookieAccessToken, Value: access, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, Expires: accessExpiresAt,
	}).String())
	header.Add("Set-Cookie", (&http.Cookie{
		Name: authctx.CookieRefreshToken, Value: refresh, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(repository.RefreshTokenTTL()),
	}).String())
	header.Add("Set-Cookie", (&http.Cookie{
		Name: authctx.CookieSessionInfo, Value: permissions, Path: "/",
		HttpOnly: false, Secure: true, SameSite: http.SameSiteLaxMode, Expires: accessExpiresAt,
	}).String())
}

// clearSessionCookies очищает все три куки (Logout) — то же имя/путь,
// MaxAge=-1 заставляет браузер удалить куку немедленно.
func clearSessionCookies(header http.Header) {
	expired := time.Unix(0, 0)
	for _, c := range []struct {
		name     string
		httpOnly bool
	}{
		{authctx.CookieAccessToken, true},
		{authctx.CookieRefreshToken, true},
		{authctx.CookieSessionInfo, false},
	} {
		header.Add("Set-Cookie", (&http.Cookie{
			Name: c.name, Value: "", Path: "/", HttpOnly: c.httpOnly,
			Secure: true, SameSite: http.SameSiteLaxMode, Expires: expired, MaxAge: -1,
		}).String())
	}
}

// readRefreshToken достаёт значение CookieRefreshToken из заголовков
// входящего запроса. http.Request заводится только как носитель для
// http.Header.Cookie — у connect-хендлера нет настоящего *http.Request,
// только req.Header().
func readRefreshToken(header http.Header) (string, bool) {
	cookie, err := (&http.Request{Header: header}).Cookie(authctx.CookieRefreshToken)
	if err != nil {
		return "", false
	}
	return cookie.Value, true
}
