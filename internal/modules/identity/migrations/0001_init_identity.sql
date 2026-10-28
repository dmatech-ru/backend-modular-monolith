CREATE SCHEMA IF NOT EXISTS identity;

CREATE FUNCTION identity.set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Справочник прав: id — инкрементальный и одновременно номер бита в битсете (начиная с 0).
-- id не переиспользуется, иначе у существующих пользователей поменяется смысл права.
CREATE TABLE identity.permissions (
    id          INTEGER GENERATED ALWAYS AS IDENTITY (MINVALUE 0 START WITH 0) PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT ''
);

-- Готовые наборы прав; permissions — битсет в base64.
CREATE TABLE identity.permission_groups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    permissions TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER permission_groups_updated_at BEFORE UPDATE ON identity.permission_groups
    FOR EACH ROW EXECUTE FUNCTION identity.set_updated_at();

-- Пользователь: только статус и права, без персональных данных.
CREATE TABLE identity.users (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'active', 'blocked', 'deleted')),
    permissions TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

CREATE INDEX users_status_idx ON identity.users (status);

CREATE TRIGGER users_updated_at BEFORE UPDATE ON identity.users
    FOR EACH ROW EXECUTE FUNCTION identity.set_updated_at();

-- Профиль: при удалении аккаунта поля заменяются заглушками.
CREATE TABLE identity.profiles (
    user_id    UUID PRIMARY KEY REFERENCES identity.users (id) ON DELETE CASCADE,
    first_name TEXT NOT NULL DEFAULT '',
    last_name  TEXT NOT NULL DEFAULT '',
    avatar_url TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER profiles_updated_at BEFORE UPDATE ON identity.profiles
    FOR EACH ROW EXECUTE FUNCTION identity.set_updated_at();

-- Данные для авторизации: при удалении аккаунта обнуляются.
CREATE TABLE identity.auth (
    user_id       UUID PRIMARY KEY REFERENCES identity.users (id) ON DELETE CASCADE,
    email         TEXT,
    phone         TEXT,
    password_hash TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX auth_email_idx ON identity.auth (lower(email));
CREATE UNIQUE INDEX auth_phone_idx ON identity.auth (phone);

CREATE TRIGGER auth_updated_at BEFORE UPDATE ON identity.auth
    FOR EACH ROW EXECUTE FUNCTION identity.set_updated_at();
