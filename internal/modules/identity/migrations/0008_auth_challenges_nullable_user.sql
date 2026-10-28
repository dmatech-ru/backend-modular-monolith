-- user_id бывает NULL для "фейковых" challenge'ов анти-перечисления:
-- InitializeSession для несуществующего identifier проходит через тот же
-- путь (та же вставка в эту таблицу), что и для настоящего аккаунта, чтобы
-- ответ нельзя было отличить по форме. У фейкового challenge нет реального
-- владельца — отсюда NULL вместо FK на identity.users.
ALTER TABLE identity.auth_challenges
    ALTER COLUMN user_id DROP NOT NULL;
