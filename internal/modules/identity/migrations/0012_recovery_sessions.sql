-- Восстановление доступа (IdentityRecoveryService): идентификатор -> доступные
-- каналы -> код -> подтверждение -> сброс пароля (ResetRecoveryPassword
-- удаляет эту строку). email/phone — слепок с identity.auth на момент
-- InitiateRecovery (см. CreateRecoverySession), не переопрашиваются повторно,
-- чтобы дальнейшие шаги не зависели от того, не поменялся ли email/телефон
-- аккаунта за время сессии. Код хранится хешем (SHA-256), не в открытом виде.
CREATE TABLE identity.recovery_sessions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES identity.users (id) ON DELETE CASCADE,

    email           TEXT,
    phone           TEXT,

    code_hash       BYTEA,
    code_expires_at TIMESTAMPTZ,
    attempts        INTEGER NOT NULL DEFAULT 0,
    code_verified   BOOLEAN NOT NULL DEFAULT FALSE,

    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX recovery_sessions_expires_at_idx ON identity.recovery_sessions (expires_at);
