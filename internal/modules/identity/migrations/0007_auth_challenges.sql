-- Серверные эфемерные значения второго шага SRP-аутентификации (см.
-- IdentityAuthService.InitializeSession / CompleteAuthentication).
-- Таблица с тем же именем уже существовала в 0003 и была удалена в 0004
-- (см. обновлённый дизайн: второй шаг аутентификации тогда ещё не был
-- реализован); сейчас таблица нужна снова — хранит verifier и эфемерные
-- значения, чтобы CompleteAuthentication мог пересчитать общий секрет без
-- повторного обращения к identity.auth.
CREATE TABLE identity.auth_challenges (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES identity.users (id) ON DELETE CASCADE,
    verifier      BYTEA NOT NULL,
    server_secret BYTEA NOT NULL,
    server_public BYTEA NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX auth_challenges_expires_at_idx ON identity.auth_challenges (expires_at);
