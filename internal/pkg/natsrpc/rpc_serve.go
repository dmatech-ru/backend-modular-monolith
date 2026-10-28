package natsrpc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
)

// RPCSubscription — подписка модуля на свои RPC-методы в NATS, созданная
// ServeHandler. Close снимает подписку.
type RPCSubscription struct {
	sub *nats.Subscription
}

// Close снимает подписку. Повторные вызовы безопасны (nats.Subscription
// сам идемпотентен для уже отписанной подписки).
func (s *RPCSubscription) Close(_ context.Context) error {
	return s.sub.Unsubscribe()
}

// ServeHandler подписывает handler на все RPC-методы маршрута
// {env}.{serviceName}.{version}: для каждого NATS-запроса на
// RPCSubjectPrefix(...)+".>" эмулирует HTTP-вызов handler.ServeHTTP и
// отправляет результат (статус, заголовки, тело) отправителю через reply.
// Подписка использует queue group serviceName, поэтому несколько реплик
// одной версии делят нагрузку между собой.
func ServeHandler(nc *nats.Conn, log *slog.Logger, env, serviceName, version string, handler http.Handler) (*RPCSubscription, error) {
	prefix := RPCSubjectPrefix(env, serviceName, version)

	sub, err := nc.QueueSubscribe(prefix+".>", sanitizeToken(serviceName), func(msg *nats.Msg) {
		serveOverNATS(log, prefix, handler, msg)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to %q: %w", prefix+".>", err)
	}

	log.Info("Subscribed to RPC requests", "subject", prefix+".>")
	return &RPCSubscription{sub: sub}, nil
}

func serveOverNATS(log *slog.Logger, prefix string, handler http.Handler, msg *nats.Msg) {
	if msg.Reply == "" {
		log.Warn("Dropping RPC request without a reply subject", "subject", msg.Subject)
		return
	}

	methodPath := strings.TrimPrefix(msg.Subject, prefix+".")

	req, err := http.NewRequest(http.MethodPost, methodPath, bytes.NewReader(msg.Data))
	if err != nil {
		log.Error("Failed to build request from NATS message", "subject", msg.Subject, "error", err)
		respond(log, msg, http.StatusInternalServerError, nil, nil)
		return
	}
	req.Header = http.Header(msg.Header).Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}

	rw := newRPCResponseWriter()
	handler.ServeHTTP(rw, req)

	respond(log, msg, rw.status, rw.header, rw.body.Bytes())
}

// respond отправляет статус, заголовки и тело отправителю msg через reply.
func respond(log *slog.Logger, msg *nats.Msg, status int, header http.Header, body []byte) {
	reply := nats.NewMsg(msg.Reply)
	for name, values := range header {
		for _, v := range values {
			reply.Header.Add(name, v)
		}
	}
	reply.Header.Set(RPCStatusHeader, strconv.Itoa(status))
	reply.Data = body

	if err := msg.RespondMsg(reply); err != nil {
		log.Error("Failed to respond over NATS", "subject", msg.Subject, "error", err)
	}
}

// rpcResponseWriter — минимальная реализация http.ResponseWriter, достаточная
// для прогона connect-go хендлера без реального сетевого соединения.
type rpcResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRPCResponseWriter() *rpcResponseWriter {
	return &rpcResponseWriter{header: make(http.Header), status: http.StatusOK}
}

func (w *rpcResponseWriter) Header() http.Header { return w.header }

func (w *rpcResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

func (w *rpcResponseWriter) WriteHeader(status int) { w.status = status }
