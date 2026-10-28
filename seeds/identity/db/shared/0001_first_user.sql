-- Первый пользователь. Фиксированный id делает сид идемпотентным.
-- DO UPDATE (а не DO NOTHING): это дев-фикстура, которую правят в файле и
-- переразворачивают через make setup — правки должны долетать до базы.
INSERT INTO identity.users (id, status, permissions)
VALUES ('00000000-0000-4000-8000-000000000001', 'active', '')
ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, permissions = EXCLUDED.permissions;

INSERT INTO identity.profiles (user_id, first_name, last_name, middle_name)
VALUES ('00000000-0000-4000-8000-000000000001', 'Дмитрий', 'Калюк', 'Олегович')
ON CONFLICT (user_id) DO UPDATE SET
	first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name, middle_name = EXCLUDED.middle_name;

-- srp_salt/srp_verifier для identifier='admin@admin.local', пароль 'admin12345' —
-- посчитаны клиентской математикой (internal/pkg/srp.GenerateSalt/ComputeVerifier),
-- сервер пароль не видел. Это единственный способ завести дев-аккаунт сидом:
-- зная пароль тут, его можно использовать в rest-examples для InitializeSession/
-- CompleteAuthentication (см. internal/pkg/srp/srp_test.go — те же формулы на клиенте).
INSERT INTO identity.auth (user_id, email, phone, srp_salt, srp_verifier)
VALUES (
	'00000000-0000-4000-8000-000000000001', 'admin@admin.local', '+71111111111',
	decode('5b306963b4f970f07850e992d97c0a1d', 'hex'),
	decode('a5708c22d74eb7bd170cb181065b557369c5fc05b2a7b5167445ff087edd3b511c912061ece39ea1d57ed9329454301a23124fc88da68b6aa780f52bb76415049b1cf57ace974d87aa318cdcf0cbd9c674739bbf76cdaf8410845a4c491f121e1b2dee3980c397f0988dcf010ac58350d4ae389c1c517012808374e4b899cf791af8517b261bced04d90606a03702ced7644779aa74455d360cbbdfb30a9ab09b4199b4a67cb8c779a399ea44a9729139b3dfc2490b0ab194c6e39ddd58541b68f5750f20c0fef39d566c1c758e1143160f22a6d44ab9709ec6ee5022f54a498ef4a083a683626141d5c1efabaaca57e6707696d1fd0a260ad5ae64514ee42da', 'hex')
)
ON CONFLICT (user_id) DO UPDATE SET
	email = EXCLUDED.email, phone = EXCLUDED.phone,
	srp_salt = EXCLUDED.srp_salt, srp_verifier = EXCLUDED.srp_verifier;
