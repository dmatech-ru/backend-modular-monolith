-- Сессии (IdentitySessionService): одна строка — один выданный refresh-
-- токен (несколько одновременных сессий на аккаунт — разные устройства,
-- см. README). refresh_token_hash — хеш (SHA-256), сам токен в базу не
-- попадает, как и везде в этом модуле с чувствительными секретами (коды
-- подтверждения, challenge'и). revoked_at — одноразовость/ротация:
-- IdentitySessionService.Refresh гасит старую строку и создаёт новую;
-- повторное предъявление уже погашенного refresh-токена — сигнал
-- компрометации (см. doc-комментарий сервиса в session.proto).
CREATE TABLE identity.sessions (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            UUID NOT NULL REFERENCES identity.users (id) ON DELETE CASCADE,

    refresh_token_hash BYTEA NOT NULL,
    user_agent         TEXT NOT NULL DEFAULT '',

    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ NOT NULL,
    revoked_at         TIMESTAMPTZ
);

CREATE UNIQUE INDEX sessions_refresh_token_hash_idx ON identity.sessions (refresh_token_hash);
CREATE INDEX sessions_user_id_idx ON identity.sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON identity.sessions (expires_at);
