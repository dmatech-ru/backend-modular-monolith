package identity

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	authhandler "dmatech.ru/internal/modules/identity/handlers/webapi/v1"
	identityrepo "dmatech.ru/internal/modules/identity/repository"
	"dmatech.ru/internal/pkg/authctx"
	authv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/auth"
	recoveryv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/recovery"
	registrationv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/registration"
	sessionv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/session"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/natsdiscovery"
	"dmatech.ru/internal/pkg/postgres"
	"dmatech.ru/internal/pkg/protoset"
	"dmatech.ru/internal/pkg/service"
	"dmatech.ru/internal/pkg/setup"
	"dmatech.ru/internal/pkg/validation"
)

const (
	configPathEnv  = "IDENTITY_CONFIG_PATH"
	postgresSchema = "identity"

	// minAntiEnumKeyBytes — минимальная длина antiEnumerationKey (см.
	// srp.FakeCredentials): 256 бит, обычный запас для HMAC-секрета.
	minAntiEnumKeyBytes = 32

	// captchaCallTimeout — таймаут одного вызова captcha-модуля по NATS.
	captchaCallTimeout = 5 * time.Second
	// notificationsCallTimeout — таймаут одного вызова notifications-модуля
	// по NATS. Это только постановка в очередь (см. doc-комментарий
	// NotificationService), а не ожидание реальной доставки — короткого
	// таймаута достаточно.
	notificationsCallTimeout = 5 * time.Second
)

// decodeAntiEnumKey декодирует antiEnumerationKey из конфига и проверяет его
// длину — короткий или пустой секрет делает srp.FakeCredentials предсказуемым
// и обесценивает защиту от перечисления аккаунтов.
func decodeAntiEnumKey(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return nil, fmt.Errorf("antiEnumerationKey: invalid hex: %w", err)
	}
	if len(key) < minAntiEnumKeyBytes {
		return nil, fmt.Errorf("antiEnumerationKey: must be at least %d bytes, got %d", minAntiEnumKeyBytes, len(key))
	}
	return key, nil
}

func init() {
	service.Register(initialization)
	setup.Register(setupDatabase)
}

func initialization(ctx context.Context) error {
	cfg, err := config.Load[configuration](configPathEnv)
	if err != nil {
		return fmt.Errorf("identity config: %w", err)
	}
	log := logger.NewModuleLogger(cfg.ServiceName, cfg.LogLevel)

	nc, err := service.Get[nats.Conn]()
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}

	antiEnumKey, err := decodeAntiEnumKey(cfg.AntiEnumerationKey)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}

	authSecret, err := authctx.DecodeSecret(cfg.AuthJWTSecret)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	tokenIssuer := authctx.NewIssuer(authSecret, identityrepo.AccessTokenTTL())

	pool, err := postgres.Connect(ctx, postgres.Config{URL: cfg.PostgresURL, ServiceName: cfg.ServiceName}, log)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	service.OnShutdown(func(_ context.Context) error {
		postgres.Close(pool, log)
		return nil
	})

	validator, err := validation.NewValidator()
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}

	descriptorBytes, err := protoset.Build(
		authv1.File_webapi_v1_identity_auth_auth_proto,
		registrationv1.File_webapi_v1_identity_registration_registration_proto,
		recoveryv1.File_webapi_v1_identity_recovery_recovery_proto,
		sessionv1.File_webapi_v1_identity_session_session_proto,
	)
	if err != nil {
		return fmt.Errorf("identity: failed to build descriptor set: %w", err)
	}

	reg, err := natsdiscovery.RegisterDescriptors(ctx, nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, descriptorBytes)
	if err != nil {
		return fmt.Errorf("failed to register module descriptors: %w", err)
	}
	service.OnShutdown(reg.Close)

	repo := identityrepo.New(pool)
	captcha := newCaptchaClient(nc, cfg.Env, cfg.CaptchaServiceName, cfg.CaptchaServiceVersion)
	notifications := newNotificationsClient(nc, cfg.Env, cfg.NotificationsServiceName, cfg.NotificationsServiceVersion)
	phones := &otpPhoneSender{client: notifications}
	emails := &otpEmailSender{client: notifications}
	recoveryCodes := &recoveryCodeSender{client: notifications}
	passwordChangedAlerts := &securityAlertSender{client: notifications}
	passwordResetFreeze := time.Duration(cfg.PasswordResetFreezeHours) * time.Hour
	sessions := newSessionManager(repo, tokenIssuer)

	sub, err := authhandler.Subscribe(nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, validator,
		repo, antiEnumKey, repo, captcha, phones, emails,
		repo, recoveryCodes, passwordChangedAlerts, passwordResetFreeze, sessions)
	if err != nil {
		return fmt.Errorf("failed to subscribe identity services: %w", err)
	}
	service.OnShutdown(sub.Close)

	log.Info("Модуль успешно запущен")
	return nil
}
