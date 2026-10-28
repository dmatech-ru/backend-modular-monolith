package captcha

import (
	"context"
	"fmt"
	"net/http"

	"github.com/nats-io/nats.go"

	peerhandler "dmatech.ru/internal/modules/captcha/handlers/peer/v1"
	webapihandler "dmatech.ru/internal/modules/captcha/handlers/webapi/v1"
	captchawebapiv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/captcha"

	"dmatech.ru/internal/pkg/config"
	"dmatech.ru/internal/pkg/logger"
	"dmatech.ru/internal/pkg/natsdiscovery"
	"dmatech.ru/internal/pkg/natsrpc"
	"dmatech.ru/internal/pkg/protoset"
	"dmatech.ru/internal/pkg/service"
)

const configPathEnv = "CAPTCHA_CONFIG_PATH"

func init() {
	service.Register(initialization)
}

func initialization(ctx context.Context) error {
	cfg, err := config.Load[configuration](configPathEnv)
	if err != nil {
		return fmt.Errorf("captcha config: %w", err)
	}
	log := logger.NewModuleLogger(cfg.ServiceName, cfg.LogLevel)

	nc, err := service.Get[nats.Conn]()
	if err != nil {
		return fmt.Errorf("captcha: %w", err)
	}

	store, err := newKVStore(ctx, nc, cfg.ChallengeTTL)
	if err != nil {
		return fmt.Errorf("captcha: %w", err)
	}
	svc := NewService(store, cfg.DefaultDifficulty, cfg.ChallengeTTL)

	// Один общий NATS-subject на оба контракта: webapi (видна гейтвею) и
	// internal (видна только другим модулям по NATS). Различает их путь
	// запроса ("/webapi.captcha.v1.CaptchaService/..." против
	// "/peer.captcha.v1.CaptchaService/...") — пакеты разные, коллизий нет.
	mux := http.NewServeMux()
	webapiPath, webapiHTTPHandler := webapihandler.NewHandler(svc)
	mux.Handle(webapiPath, webapiHTTPHandler)
	peerPath, peerHTTPHandler := peerhandler.NewHandler(svc)
	mux.Handle(peerPath, peerHTTPHandler)

	sub, err := natsrpc.ServeHandler(nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, mux)
	if err != nil {
		return fmt.Errorf("failed to subscribe CaptchaService: %w", err)
	}
	service.OnShutdown(sub.Close)

	// В дескриптор для discovery попадает только webapi-файл — гейтвей
	// никогда не узнаёт о существовании peer.captcha.v1.CaptchaService
	// и физически не сможет его проксировать, даже по ошибке.
	descriptorBytes, err := protoset.Build(captchawebapiv1.File_webapi_v1_captcha_captcha_proto)
	if err != nil {
		return fmt.Errorf("captcha: failed to build descriptor set: %w", err)
	}
	reg, err := natsdiscovery.RegisterDescriptors(ctx, nc, log, cfg.Env, cfg.ServiceName, cfg.AppVersion, descriptorBytes)
	if err != nil {
		return fmt.Errorf("failed to register module descriptors: %w", err)
	}
	service.OnShutdown(reg.Close)

	log.Info("Модуль успешно запущен")
	return nil
}
