// Package locale — перенос локали запроса через границы NATS/connect-
// вызовов. Браузер указывает локаль явным заголовком X-Locale или, если он
// не задан, стандартным Accept-Language (с учётом q-весов, RFC 9110
// §12.5.4) — backend это значение только ПЕРЕНОСИТ между модулями
// (ServerInterceptor кладёт его в context.Context на входе,
// ClientInterceptor переносит обратно в заголовок исходящего вызова другого
// модуля), а не переводит на основе него тексты API-ошибок (см.
// internal/pkg/validation и webapi/v1/common/errors.proto — ошибки несут
// стабильный ключ, перевод — на клиенте). Локаль используется только там,
// где получатель — не клиентское приложение, а живой человек: тексты
// шаблонов notifications (internal/modules/notifications/template).
//
// natsrpc (gateway → identity, identity → notifications) уже прозрачно
// переносит HTTP-заголовки через NATS в обе стороны — само значение
// заголовка доходит до любого модуля бесплатно. Не хватает только явного
// шага "прочитать заголовок во входящем запросе" и "положить его в заголовок
// исходящего" — для этого и нужны два интерцептора ниже.
package locale

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"
)

// Locale — код языка (primary BCP-47 subtag, например "ru"/"en").
type Locale string

const (
	// HeaderLocale — явный, собственный заголовок: если задан и известен
	// (см. Supported), имеет приоритет над HeaderAcceptLanguage.
	HeaderLocale = "X-Locale"
	// HeaderAcceptLanguage — стандартный HTTP-заголовок, запасной источник
	// (когда клиент не прислал HeaderLocale).
	HeaderAcceptLanguage = "Accept-Language"

	// Default — локаль, когда ни один заголовок не задан или не распознан.
	Default Locale = "ru"
)

// Supported — локали, для которых реально есть шаблоны/переводы. Запрошенная
// локаль, которой здесь нет, молча заменяется на Default — отсутствие
// перевода не должно ломать запрос (см. doc-комментарий пакета template).
var Supported = map[Locale]bool{
	"ru": true,
	"en": true,
}

// Parse определяет локаль запроса по заголовкам: сначала HeaderLocale (точное
// значение), затем HeaderAcceptLanguage (с учётом q-весов), иначе Default.
func Parse(header http.Header) Locale {
	if v := normalize(header.Get(HeaderLocale)); Supported[v] {
		return v
	}
	for _, tag := range parseAcceptLanguage(header.Get(HeaderAcceptLanguage)) {
		if Supported[tag] {
			return tag
		}
	}
	return Default
}

func normalize(tag string) Locale {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexByte(tag, '-'); i >= 0 {
		tag = tag[:i]
	}
	return Locale(tag)
}

// parseAcceptLanguage возвращает primary-subtags из Accept-Language
// (например "ru-RU,ru;q=0.9,en;q=0.8" -> ["ru","ru","en"]), отсортированные
// по убыванию q (без указанного веса — q=1, невалидный вес игнорируется).
func parseAcceptLanguage(header string) []Locale {
	if header == "" {
		return nil
	}

	type weighted struct {
		tag Locale
		q   float64
	}
	var tags []weighted
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tagPart, q := part, 1.0
		if i := strings.IndexByte(part, ';'); i >= 0 {
			tagPart = part[:i]
			if qv, ok := strings.CutPrefix(strings.TrimSpace(part[i+1:]), "q="); ok {
				if parsed, err := strconv.ParseFloat(qv, 64); err == nil {
					q = parsed
				}
			}
		}
		if tag := normalize(tagPart); tag != "" {
			tags = append(tags, weighted{tag: tag, q: q})
		}
	}
	sort.SliceStable(tags, func(i, j int) bool { return tags[i].q > tags[j].q })

	result := make([]Locale, len(tags))
	for i, t := range tags {
		result[i] = t.tag
	}
	return result
}

type ctxKey struct{}

// NewContext кладёт locale в ctx — см. ServerInterceptor.
func NewContext(ctx context.Context, l Locale) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext читает locale из ctx; Default, если её там нет (ctx не прошёл
// через ServerInterceptor — например, в тестах, где это не важно).
func FromContext(ctx context.Context) Locale {
	if l, ok := ctx.Value(ctxKey{}).(Locale); ok {
		return l
	}
	return Default
}

// ServerInterceptor читает локаль из заголовков входящего запроса (см.
// Parse) и кладёт её в context.Context — дальше по цепочке вызовов
// (включая исходящие к другим модулям, см. ClientInterceptor) она доступна
// через FromContext. На успешном ответе эхо́м возвращает её же в
// HeaderLocale: если запрошенная локаль не входила в Supported или клиент
// не прислал ни одного заголовка, Parse молча подставила Default — без
// эха клиент не узнал бы, какая локаль реально применилась (например, к
// шаблону письма), и не смог бы ни показать это пользователю, ни
// предложить переключиться на то, что реально поддерживается.
func ServerInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			resolved := Parse(req.Header())
			ctx = NewContext(ctx, resolved)
			resp, err := next(ctx, req)
			if resp != nil {
				resp.Header().Set(HeaderLocale, string(resolved))
			}
			return resp, err
		}
	}
}

// ClientInterceptor переносит локаль из context.Context (положенную туда
// ServerInterceptor'ом на входящей стороне того же запроса) в заголовок
// исходящего вызова другого модуля — иначе локаль не ушла бы дальше первого
// же межмодульного вызова: natsrpc сам переносит заголовки байт-в-байт, но
// не копирует context.Context в заголовки — это явный шаг.
func ClientInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set(HeaderLocale, string(FromContext(ctx)))
			return next(ctx, req)
		}
	}
}
