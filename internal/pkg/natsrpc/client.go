package natsrpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
)

// Client реализует connect.HTTPClient поверх NATS request-reply — тот же
// протокол, которым ServeHandler принимает запросы на стороне модуля.
// Позволяет модулю вызывать другой модуль через обычный сгенерированный
// connect-клиент (NewXxxServiceClient(client, baseURL)), зная только его
// buf-контракт (сгенерированные типы) и env/serviceName/version — без
// прямого Go-импорта между модулями и без участия гейтвея: это внутренний
// вызов между модулями, не внешний HTTP-запрос.
//
// В отличие от гейтвея, которому serviceName/version произвольных модулей
// приходится узнавать через natsdiscovery (набор заранее не известен),
// вызывающий модуль здесь — известная на этапе написания кода зависимость:
// достаточно знать имя/версию целевого сервиса из своего конфига, без watch.
type Client struct {
	nc      *nats.Conn
	env     string
	service string
	version string
	timeout time.Duration
}

// NewClient возвращает клиент для вызова serviceName/version в окружении env.
// timeout — таймаут одного RPC-вызова (контекст запроса может задать более
// строгий дедлайн, но не ослабить этот).
func NewClient(nc *nats.Conn, env, serviceName, version string, timeout time.Duration) *Client {
	return &Client{nc: nc, env: env, service: serviceName, version: version, timeout: timeout}
}

// Do реализует connect.HTTPClient: пересылает req через NATS request-reply
// на RPCSubject, собранный из env/serviceName/version клиента и пути
// запроса, и восстанавливает *http.Response из ответа. Ошибка транспорта
// (включая "нет живых подписчиков") возвращается как есть — connect сам
// оборачивает её в CodeUnavailable для вызывающего кода.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("natsrpc: read request body: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(req.Context(), c.timeout)
	defer cancel()

	subject := RPCSubject(c.env, c.service, c.version, req.URL.Path)
	reply, err := c.nc.RequestMsgWithContext(ctx, &nats.Msg{
		Subject: subject,
		Header:  nats.Header(req.Header.Clone()),
		Data:    body,
	})
	if err != nil {
		return nil, fmt.Errorf("natsrpc: request %s: %w", subject, err)
	}

	status := StatusFromReply(reply)

	return &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status) + " " + http.StatusText(status),
		Header:     http.Header(reply.Header),
		Body:       io.NopCloser(bytes.NewReader(reply.Data)),
		Request:    req,
	}, nil
}
