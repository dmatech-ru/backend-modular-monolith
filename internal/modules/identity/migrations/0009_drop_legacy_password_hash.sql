-- Легаси-колонка: аутентификация полностью на SRP (identity.auth.srp_salt/
-- srp_verifier), пароль на сервер не передаётся и password_hash никогда не
-- заполнялся (см. internal/pkg/srp и handlers/webapi/v1/auth.go).
ALTER TABLE identity.auth
    DROP COLUMN password_hash;
