package notifications

import (
	"fmt"
	"regexp"
	"strings"

	deliveryv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/delivery"
)

var invalidTokenChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sanitizeToken — тот же приём, что в natsdiscovery/natsrpc: превращает
// строку в безопасный токен NATS-subject (пакеты не делят код специально,
// это три независимых небольших использования одного правила, не общая
// абстракция, которую стоило бы выносить).
func sanitizeToken(s string) string {
	return invalidTokenChars.ReplaceAllString(strings.TrimSpace(s), "_")
}

// streamSubjectsWildcard — subject'ы, которые стрим окружения env принимает
// целиком (отдельные консьюмеры затем фильтруются по channelSubject).
func streamSubjectsWildcard(env string) string {
	return sanitizeToken(env) + ".notifications.*"
}

// channelSubject строит subject конкретного канала в окружении env —
// на него Enqueue публикует, и на него же фильтруется консьюмер этого
// канала (см. channelToken и ConsumerOptions.FilterSubject).
func channelSubject(env string, channel deliveryv1.DeliveryChannel) string {
	return sanitizeToken(env) + ".notifications." + channelToken(channel)
}

// channelToken — короткое имя канала для subject'а и durable-имени
// консьюмера.
func channelToken(channel deliveryv1.DeliveryChannel) string {
	switch channel {
	case deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL:
		return "email"
	case deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS:
		return "sms"
	case deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM:
		return "telegram"
	case deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_MAX:
		return "max"
	default:
		return "unknown"
	}
}

// allChannels — каналы, на которые заводится отдельный consumer/sender.
// DELIVERY_CHANNEL_UNSPECIFIED сюда не входит: это невалидный вход
// (см. Service.Enqueue), а не канал доставки.
var allChannels = []deliveryv1.DeliveryChannel{
	deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL,
	deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS,
	deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM,
	deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_MAX,
}

var errUnknownChannel = fmt.Errorf("notifications: unknown delivery channel")
