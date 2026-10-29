package identity

import (
	"context"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"

	authhandler "dmatech.ru/internal/modules/identity/handlers/webapi/v1"
	deliveryv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/delivery"
	templatesv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/templates"
	notificationspeerv1 "dmatech.ru/internal/pkg/contracts/peer/v1/notifications"
	"dmatech.ru/internal/pkg/contracts/peer/v1/notifications/notificationsv1connect"
	"dmatech.ru/internal/pkg/locale"
	"dmatech.ru/internal/pkg/natsrpc"
)

// newNotificationsClient строит натsrpc-клиент к peer.notifications.v1.
// NotificationService (не webapi — наружу через гейтвей этот вызов не
// идёт), тем же приёмом, что identity уже зовёт captcha (см.
// captcha_client.go): identity знает только сгенерированные типы и
// имя/версию сервиса из своего конфига, без Go-импорта internal/modules/notifications.
//
// locale.ClientInterceptor переносит локаль текущего запроса (положенную в
// context.Context при входе в identity, см. locale.ServerInterceptor в
// auth.handler.go) в заголовок этого исходящего вызова — notifications
// рендерит шаблон на этой локали (см. internal/modules/notifications/
// template). Без этого интерцептора локаль обрывалась бы на границе
// identity: natsrpc переносит заголовки байт-в-байт, но не копирует
// context.Context в заголовки сам.
func newNotificationsClient(nc *nats.Conn, env, serviceName, version string) notificationsv1connect.NotificationServiceClient {
	httpClient := natsrpc.NewClient(nc, env, serviceName, version, notificationsCallTimeout)
	return notificationsv1connect.NewNotificationServiceClient(httpClient, "http://"+serviceName,
		connect.WithInterceptors(locale.ClientInterceptor()))
}

// otpPhoneSender/otpEmailSender реализуют authhandler.PhoneSender/
// EmailSender через peer.notifications.v1.NotificationService.Send с
// template_key = TEMPLATE_KEY_OTP_CODE — identity больше не формирует текст
// сообщения сам (см. internal/modules/notifications/template), только
// передаёт переменную code.

type otpPhoneSender struct {
	client notificationsv1connect.NotificationServiceClient
}

func (s *otpPhoneSender) SendCode(ctx context.Context, phone, code string) error {
	_, err := s.client.Send(ctx, connect.NewRequest(&notificationspeerv1.SendRequest{
		Channel:     deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
		Destination: phone,
		TemplateKey: templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		Variables:   map[string]string{"code": code},
	}))
	return err
}

// IsPhoneSender — см. doc-комментарий authhandler.PhoneSender про то, зачем
// этот метод существует без тела: только чтобы *otpPhoneSender не
// удовлетворял authhandler.EmailSender по чистой случайности совпадения сигнатур.
func (s *otpPhoneSender) IsPhoneSender() {}

type otpEmailSender struct {
	client notificationsv1connect.NotificationServiceClient
}

func (s *otpEmailSender) SendCode(ctx context.Context, email, code string) error {
	_, err := s.client.Send(ctx, connect.NewRequest(&notificationspeerv1.SendRequest{
		Channel:     deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		Destination: email,
		TemplateKey: templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE,
		Variables:   map[string]string{"code": code},
	}))
	return err
}

func (s *otpEmailSender) IsEmailSender() {}

var (
	_ authhandler.PhoneSender = (*otpPhoneSender)(nil)
	_ authhandler.EmailSender = (*otpEmailSender)(nil)
)

// recoveryCodeSender реализует authhandler.RecoveryCodeSender через
// peer.notifications.v1.NotificationService.Send с template_key =
// TEMPLATE_KEY_PASSWORD_RESET_CODE — отдельный от TEMPLATE_KEY_OTP_CODE
// шаблон (см. templates.proto), канал не фиксирован заранее (в отличие от
// otpPhoneSender/otpEmailSender): IdentityRecoveryService сам решает,
// email/SMS/Telegram, по выбору пользователя среди доступных методов.
type recoveryCodeSender struct {
	client notificationsv1connect.NotificationServiceClient
}

func (s *recoveryCodeSender) SendCode(ctx context.Context, channel deliveryv1.DeliveryChannel, destination, code string) error {
	_, err := s.client.Send(ctx, connect.NewRequest(&notificationspeerv1.SendRequest{
		Channel:     channel,
		Destination: destination,
		TemplateKey: templatesv1.TemplateKey_TEMPLATE_KEY_PASSWORD_RESET_CODE,
		Variables:   map[string]string{"code": code},
	}))
	return err
}

var _ authhandler.RecoveryCodeSender = (*recoveryCodeSender)(nil)

// securityAlertSender реализует authhandler.PasswordChangedNotifier через
// peer.notifications.v1.NotificationService.Send с template_key =
// TEMPLATE_KEY_PASSWORD_CHANGED — без variables, в отличие от
// otpPhoneSender/recoveryCodeSender: текст статический (см.
// internal/modules/notifications/template/templates/password_changed).
type securityAlertSender struct {
	client notificationsv1connect.NotificationServiceClient
}

func (s *securityAlertSender) NotifyEmail(ctx context.Context, email string) error {
	_, err := s.client.Send(ctx, connect.NewRequest(&notificationspeerv1.SendRequest{
		Channel:     deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
		Destination: email,
		TemplateKey: templatesv1.TemplateKey_TEMPLATE_KEY_PASSWORD_CHANGED,
	}))
	return err
}

func (s *securityAlertSender) NotifyPhone(ctx context.Context, phone string) error {
	_, err := s.client.Send(ctx, connect.NewRequest(&notificationspeerv1.SendRequest{
		Channel:     deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
		Destination: phone,
		TemplateKey: templatesv1.TemplateKey_TEMPLATE_KEY_PASSWORD_CHANGED,
	}))
	return err
}

var _ authhandler.PasswordChangedNotifier = (*securityAlertSender)(nil)
