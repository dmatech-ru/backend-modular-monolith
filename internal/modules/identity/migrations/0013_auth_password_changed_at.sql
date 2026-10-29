-- ResetPassword (IdentityRecoveryService) фиксирует момент смены пароля —
-- нужен, чтобы посчитать frozen_until (passwordResetFreezeHours в конфиге
-- identity), см. doc-комментарий сервиса в recovery.proto.
ALTER TABLE identity.auth
    ADD COLUMN password_changed_at TIMESTAMPTZ;
