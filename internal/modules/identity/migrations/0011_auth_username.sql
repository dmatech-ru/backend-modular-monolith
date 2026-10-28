-- username как полноценный тип identifier (наравне с email/телефоном) для
-- InitializeSession/CheckIdentifier/IdentityRecoveryService.InitiateRecovery.
-- Схема готова заранее: ни один текущий write-путь (регистрация) username
-- пока не задаёт — колонка будет пустой у всех аккаунтов, пока в
-- IdentityRegistrationService не появится отдельный шаг для её выбора.
ALTER TABLE identity.auth
    ADD COLUMN username TEXT;

CREATE UNIQUE INDEX auth_username_idx ON identity.auth (lower(username));
