// Package v1 реализует внутренний контракт notifications-модуля
// (peer.notifications.v1.NotificationService) поверх connect-go. Наружу
// через гейтвей не торчит — webapi-контракта у notifications нет вообще,
// её зовут только другие модули по NATS (см. internal/pkg/natsrpc.Client).
package v1

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	notificationsv1 "dmatech.ru/internal/pkg/contracts/peer/v1/notifications"
	"dmatech.ru/internal/pkg/contracts/peer/v1/notifications/notificationsv1connect"
)

// Service — доменная логика, нужная обработчику.
type Service interface {
	Enqueue(ctx context.Context, req *notificationsv1.SendRequest) error
}

// NotificationHandler реализует notificationsv1connect.NotificationServiceHandler.
type NotificationHandler struct {
	notificationsv1connect.UnimplementedNotificationServiceHandler

	svc Service
}

// NewHandler собирает HTTP-обработчик peer.notifications.v1.NotificationService.
func NewHandler(svc Service, opts ...connect.HandlerOption) (string, http.Handler) {
	return notificationsv1connect.NewNotificationServiceHandler(&NotificationHandler{svc: svc}, opts...)
}

// Send кладёт сообщение в очередь и подтверждает приём — не реальную
// доставку (см. doc-комментарий NotificationService в notifications.proto).
func (h *NotificationHandler) Send(
	ctx context.Context,
	req *connect.Request[notificationsv1.SendRequest],
) (*connect.Response[notificationsv1.SendResponse], error) {
	if err := h.svc.Enqueue(ctx, req.Msg); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&notificationsv1.SendResponse{}), nil
}
