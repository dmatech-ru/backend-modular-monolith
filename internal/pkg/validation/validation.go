// Package validation — статическая проверка формы входящих proto-сообщений
// через protovalidate (правила buf.validate.* прямо в .proto, см. поля с
// (buf.validate.field).cel в identity/proto/webapi/v1/identity/*/*.proto) и
// перевод найденных нарушений в единый формат ошибок
// (webapi.common.errors.v1.ErrorPayload).
//
// Обработчик, подключивший Interceptor, может считать входные данные уже
// прошедшими проверку формы (непустые обязательные поля и т.п.) — ему
// остаётся только бизнес-логика (существование сессии, доступность канала и
// подобное), которую buf.validate не выражает и не должен: это не форма
// запроса, а состояние системы.
//
// id правила в .proto — это и есть ключ ошибки (ErrorPayload.key/
// FieldError.key, см. doc-комментарий там): backend не переводит текст
// ошибки на основе локали запроса (см. internal/pkg/locale — локаль нужна
// только для шаблонов notifications, не для ошибок API), только возвращает
// стабильный ключ, который клиент сам показывает локализованной строкой.
package validation

import (
	"context"
	"errors"
	"fmt"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	common "dmatech.ru/internal/pkg/contracts/webapi/v1/common"
)

// Validator — псевдоним protovalidate.Validator: вызывающему коду (другим
// модулям) не нужно импортировать buf.build/go/protovalidate напрямую
// только чтобы объявить тип поля/параметра.
type Validator = protovalidate.Validator

// NewValidator создаёт Validator, общий для всех RPC-методов модуля —
// компиляция CEL-правил кэшируется внутри него по типу сообщения, пересборка
// на каждый запрос не нужна (см. protovalidate.New).
func NewValidator() (Validator, error) {
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("validation: build validator: %w", err)
	}
	return v, nil
}

// Interceptor валидирует каждое входящее unary-сообщение перед вызовом
// обработчика. Нарушение правила — connect.CodeInvalidArgument с деталью
// common.ErrorPayload (см. toConnectError); сбой самой проверки (например,
// ошибка компиляции CEL-выражения — баг в .proto, не во входных данных) —
// connect.CodeInternal.
func Interceptor(v Validator) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			msg, ok := req.Any().(proto.Message)
			if !ok {
				return next(ctx, req)
			}
			if err := v.Validate(msg); err != nil {
				return nil, toConnectError(err)
			}
			return next(ctx, req)
		}
	}
}

// toConnectError превращает ошибку protovalidate.Validator.Validate в
// connect-ошибку с деталью common.ErrorPayload: одна common.FieldError на
// нарушение — name — путь до поля, key — id правила (наш ключ локализации,
// см. doc-комментарий пакета), message — фиксированный текст для
// логов/отладки, не для показа пользователю.
//
// Ошибка, не являющаяся *protovalidate.ValidationError (CompilationError/
// RuntimeError — баг в правиле, не во входных данных), возвращается как
// CodeInternal: это не то, что должен "чинить" клиент, меняя запрос.
func toConnectError(err error) error {
	var verr *protovalidate.ValidationError
	if !errors.As(err, &verr) {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("validation: %w", err))
	}

	const key = "common.validation.failed"
	payload := &common.ErrorPayload{
		Code:    int32(connect.CodeInvalidArgument),
		Message: "request failed validation",
		Key:     key,
	}
	for _, v := range verr.Violations {
		payload.Fields = append(payload.Fields, &common.FieldError{
			Name:    protovalidate.FieldPathString(v.Proto.GetField()),
			Key:     v.Proto.GetRuleId(),
			Message: v.Proto.GetMessage(),
		})
	}

	cerr := connect.NewError(connect.CodeInvalidArgument, errors.New(payload.Message))
	if detail, derr := connect.NewErrorDetail(payload); derr == nil {
		cerr.AddDetail(detail)
	}
	return cerr
}
