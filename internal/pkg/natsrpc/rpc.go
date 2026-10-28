// Package natsrpc — RPC-транспорт поверх NATS: соглашение об именовании
// subject'ов для вызова метода конкретного модуля и мост, который подаёт
// такие NATS-запросы на вход обычному http.Handler (см. ServeHandler).
//
// Какой модуль и версию обслуживает subject, известно из discovery (см.
// internal/pkg/natsdiscovery) — этот пакет не зависит от неё и сам
// санитизирует переданные env/serviceName/version.
package natsrpc

import (
	"regexp"
	"strings"
)

var invalidTokenChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sanitizeToken превращает строку в безопасный токен NATS-subject: точки
// (разделитель токенов), wildcard-символы и прочее заменяются на "_".
func sanitizeToken(s string) string {
	return invalidTokenChars.ReplaceAllString(strings.TrimSpace(s), "_")
}

// RPCStatusHeader — заголовок в NATS-ответе на RPC-запрос, содержащий исходный
// HTTP-статус, с которым ответил обработчик модуля. Гейтвей переносит его в
// статус HTTP-ответа клиенту и не пробрасывает сам заголовок дальше.
const RPCStatusHeader = "Nats-Rpc-Status"

// RPCSubjectPrefix возвращает общий префикс NATS-subject для всех RPC-методов
// маршрута {env}.{serviceName}.{version}. Модуль подписывается на
// RPCSubjectPrefix(...) + ".>", чтобы получать запросы по всем своим методам.
func RPCSubjectPrefix(env, serviceName, version string) string {
	return sanitizeToken(env) + "." + sanitizeToken(serviceName) + "." + sanitizeToken(version) + ".rpc"
}

// RPCSubject строит subject для конкретного HTTP-пути RPC-метода (например,
// "/webapi.identity.auth.v1.IdentityAuthService/InitializeSession" — это ровно
// Connect/gRPC-процедура, сгенерированная protoc-gen-connect-go).
func RPCSubject(env, serviceName, version, methodPath string) string {
	return RPCSubjectPrefix(env, serviceName, version) + "." + methodPath
}
