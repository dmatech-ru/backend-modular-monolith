-- Многошаговая регистрация (IdentityRegistrationService): телефон+согласие
-- на обработку персональных данных -> код из SMS -> email -> код из письма
-- -> пароль (CompleteRegistration создаёт identity.users/profiles/auth и
-- удаляет эту строку). Коды хранятся хешем (SHA-256), не в открытом виде.
CREATE TABLE identity.registration_sessions (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    phone                 TEXT NOT NULL,
    personal_data_consent BOOLEAN NOT NULL,
    phone_verified        BOOLEAN NOT NULL DEFAULT FALSE,
    phone_code_hash       BYTEA,
    phone_code_expires_at TIMESTAMPTZ,
    phone_attempts        INTEGER NOT NULL DEFAULT 0,

    email                 TEXT,
    email_verified        BOOLEAN NOT NULL DEFAULT FALSE,
    email_code_hash       BYTEA,
    email_code_expires_at TIMESTAMPTZ,
    email_attempts        INTEGER NOT NULL DEFAULT 0,

    -- Общий дедлайн всей регистрации (не только текущего кода): заброшенная
    -- на любом шаге сессия перестаёт приниматься и попадает под cleanup ниже.
    expires_at            TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX registration_sessions_expires_at_idx ON identity.registration_sessions (expires_at);
