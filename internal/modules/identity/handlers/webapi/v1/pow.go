package v1

import "context"

// AbuseTracker считает непройденные/неверные попытки PoW на (scope, key) —
// единственная реализация (identity/abuse.go) — NATS JetStream KV с TTL на
// бакет, тот же приём, что captcha хранит challenge'и (см. doc-комментарий
// там): окно само закрывается, отдельная фоновая задача на чистку не
// нужна. Интерфейс объявлен здесь, на стороне потребителя, чтобы тесты
// могли подставить фейк без реального NATS.
type AbuseTracker interface {
	Count(ctx context.Context, scope, key string) (int, error)
	RecordAttempt(ctx context.Context, scope, key string) error
}

// Scope-константы — один на каждый метод, у которого есть pow_challenge/
// pow_nonce (CheckIdentifier/StartPhoneVerification/StartEmailVerification
// в registration.proto, InitiateRecovery/SendRecoveryCode в recovery.proto)
// — префикс ключа в AbuseTracker, чтобы один IP/телефон/email не делил
// счётчик попыток между разными методами (иначе исчерпанный лимит на одном
// методе случайно "сжигал" бы лимит другого).
const (
	abuseScopeCheckIdentifier        = "check_identifier"
	abuseScopeStartPhoneVerification = "start_phone_verification"
	abuseScopeStartEmailVerification = "start_email_verification"
	abuseScopeInitiateRecovery       = "initiate_recovery"
	abuseScopeSendRecoveryCode       = "send_recovery_code"
)

// adaptivePoWGate — см. doc-комментарий сервиса в registration.proto про
// "после N попыток без решения (или с неверным/просроченным)": пока
// Count(scope, key) меньше threshold, вызов проходит без PoW вовсе —
// единичный легитимный пользователь не замечает этого шага, как и
// задумано (решение — доли секунды CPU, которые он даже не тратит, раз
// challenge/nonce пусты). Валидное решение — пропускает и НЕ увеличивает
// счётчик: атакующий, который решает каждый раз, уже платит ровно ту
// цену, которую PoW и должен был наложить, дальше карать нечем.
// Непройденное или неверное решение увеличивает счётчик независимо от
// того, был ли PoW обязателен на этот конкретный вызов — иначе порог
// никогда не наступит: именно "невидимые" попытки без решения и копят
// счётчик, который впоследствии его включает.
//
// ok=false — вызывающий код должен ответить STATUS_CAPTCHA_REQUIRED, не
// заворачивать в connect-ошибку: это обычный, ожидаемый ответ сервиса, не
// сбой.
func adaptivePoWGate(ctx context.Context, captcha CaptchaClient, abuse AbuseTracker, threshold int, scope, key, challenge, nonce string) (ok bool, err error) {
	count, err := abuse.Count(ctx, scope, key)
	if err != nil {
		return false, err
	}

	solved := false
	if challenge != "" && nonce != "" {
		solved, err = captcha.VerifyChallenge(ctx, challenge, nonce)
		if err != nil {
			return false, err
		}
	}
	if solved {
		return true, nil
	}

	if err := abuse.RecordAttempt(ctx, scope, key); err != nil {
		return false, err
	}
	return count < threshold, nil
}
