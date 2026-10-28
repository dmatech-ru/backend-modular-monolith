// Package srp реализует серверную и клиентскую половины SRP-6a (Secure
// Remote Password): клиент доказывает знание пароля без передачи его по
// сети, а сервер хранит только verifier — из которого пароль восстановить
// нельзя. Пароль не передаётся по сети ни при входе, ни при регистрации —
// доверие к транспорту (TLS) не входит в модель угроз: salt и verifier
// вычисляются на клиенте, сервер эту функцию (ComputeVerifier) не вызывает
// и пароль не видит ни разу, в том числе при регистрации.
//
// Группа — 2048-битная группа из RFC 5054, Appendix A.3 (N, g = 2); хэш —
// SHA-256 вместо SHA-1 из оригинала RFC (совместимость с внешними SRP-
// клиентами, рассчитанными на RFC 5054 "как есть", не требуется — контракт
// этого сервиса собственный, см. identity/proto/webapi/v1/identity/auth.proto).
//
// Вычисления (H — SHA-256, PAD — левое дополнение нулями до длины N в байтах):
//
//	k = H(N | PAD(g))
//	x = H(salt | H(identifier | ":" | password))
//	v = g^x mod N                                   (verifier, хранится на сервере)
//	B = (k*v + g^b) mod N                           (эфемерное значение сервера)
//	A = g^a mod N                                    (эфемерное значение клиента)
//	u = H(PAD(A) | PAD(B))
//	S = (B - k*g^x)^(a + u*x) mod N  (клиент)  =  (A * v^u)^b mod N  (сервер)
//	K = H(S)
//	M1 = H(PAD(A) | PAD(B) | K)                      (доказательство клиента)
//	M2 = H(PAD(A) | M1 | K)                          (доказательство сервера)
package srp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"math/big"
)

var (
	// ErrInvalidPublicValue — присланное A или B равно 0 по модулю N: по
	// правилам SRP-6a это должно отвергаться (иначе общий секрет становится
	// предсказуемым).
	ErrInvalidPublicValue = errors.New("srp: public value is invalid")
	// ErrProofMismatch — доказательство (M1 или M2) не совпало с ожидаемым.
	ErrProofMismatch = errors.New("srp: proof does not match")
	// ErrValueTooLarge — присланное значение длиннее группы N: отклоняется
	// до разбора в big.Int, чтобы не тратить CPU на экспоненцирование с
	// контролируемым атакующим огромным основанием/показателем.
	ErrValueTooLarge = errors.New("srp: value exceeds group size")
)

var (
	g = big.NewInt(2)
	n = mustParseHex(nHex)
	k = hashInt(padTo(n, n.Bytes()), padTo(n, g.Bytes()))

	nLen = (n.BitLen() + 7) / 8 // 256 байт для 2048-битной группы
)

// Взято из RFC 5054, Appendix A.3 ("2048-bit Group"); извлечено
// программным конкатенированием текста RFC, без ручной перепечатки.
const nHex = "AC6BDB41324A9A9BF166DE5E1389582FAF72B6651987EE07FC3192943DB56050A37329CBB4A099ED8193E0757767A13DD52312AB4B03310DCD7F48A9DA04FD50E8083969EDB767B0CF6095179A163AB3661A05FBD5FAAAE82918A9962F0B93B855F97993EC975EEAA80D740ADBF4FF747359D041D5C33EA71D281E446B14773BCA97B43A23FB801676BD207A436C6481F1D2B9078717461A5B9D32E688F87748544523B524B0D57D5EA77A2775D2ECFA032CFBDBF52FB3786160279004E57AE6AF874E7303CE53299CCC041C7BC308D82A5698F3A8D0C38271AE35F8E9DBFBB694B5C803D89F7AE435DE236D525F54759B65E372FCD68EF20FA7111F9E4AFF73"

func mustParseHex(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("srp: invalid group constant")
	}
	return v
}

func hash(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func hashInt(parts ...[]byte) *big.Int {
	return new(big.Int).SetBytes(hash(parts...))
}

// padTo дополняет b слева нулями до длины N в байтах (nLen), как того
// требует SRP-6a при вычислении u = H(PAD(A) | PAD(B)) и k = H(N | PAD(g)).
// Здесь N передаётся явно, а не через глобальную nLen, чтобы функция не
// зависела от порядка инициализации пакетных переменных.
func padTo(modulus *big.Int, b []byte) []byte {
	l := (modulus.BitLen() + 7) / 8
	if len(b) >= l {
		return b[len(b)-l:]
	}
	out := make([]byte, l)
	copy(out[l-len(b):], b)
	return out
}

func pad(b []byte) []byte { return padTo(n, b) }

// randomScalar возвращает случайное 256-битное число — достаточно для
// эфемерных секретов (a, b) в паре с 2048-битной группой.
func randomScalar() (*big.Int, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(buf), nil
}

func checkSize(b []byte) error {
	if len(b) == 0 || len(b) > nLen {
		return ErrValueTooLarge
	}
	return nil
}

// computeX вычисляет "закрытый ключ" x = H(salt | H(identifier | ":" | password)),
// из которого выводится verifier. Пароль нигде за пределами клиента не
// появляется: сервер эту функцию не вызывает — ни при регистрации (verifier
// присылает клиент), ни при входе (у сервера есть только verifier, не x).
func computeX(identifier, password string, salt []byte) *big.Int {
	inner := hash([]byte(identifier), []byte(":"), []byte(password))
	return hashInt(salt, inner)
}

// GenerateSalt возвращает криптографически случайную соль фиксированной
// длины. Вызывается клиентом при регистрации — сервер пароль не видит и
// соль для нового пользователя не выбирает.
func GenerateSalt() ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// ComputeVerifier вычисляет verifier v = g^x mod N при регистрации. Вызывается
// клиентом: пароль никогда не покидает клиента и на сервер не передаётся
// (сервер получает только salt и verifier — восстановить из них пароль
// невозможно).
func ComputeVerifier(identifier, password string, salt []byte) []byte {
	x := computeX(identifier, password, salt)
	return new(big.Int).Exp(g, x, n).Bytes()
}

// ValidVerifier проверяет только то, что verifier мог бы быть значением
// этой группы (не пустой и не длиннее N) — без пароля сервер не может
// проверить, что verifier действительно был вычислен от какого-то пароля.
// Используется при регистрации, чтобы отклонить явно некорректный ввод, не
// дожидаясь первой попытки входа.
func ValidVerifier(verifier []byte) bool {
	return checkSize(verifier) == nil
}

// FakeCredentials детерминированно производит правдоподобные salt/verifier
// для identifier, у которого на самом деле нет аккаунта — защита
// InitializeSession от перечисления аккаунтов (account enumeration): сервер
// без обращения к БД отвечает тем же по форме, что и для существующего
// identifier, и отличить несуществующий identifier от существующего по коду
// ответа невозможно. Один и тот же identifier при любом повторном запросе и
// на любой реплике даёт один и тот же результат (key общий для всех реплик),
// поэтому поведение не меняется между запросами.
//
// key должен быть секретом (минимум 32 случайных байта), одинаковым на всех
// репликах. В отличие от группы SRP (N, g — публичные константы протокола),
// key — это секрет именно этого сервиса: если key узнает атакующий, он
// сможет сам посчитать FakeCredentials и отличить фейковый salt от
// настоящего, что обесценит защиту.
//
// Пройти второй шаг (CompleteAuthentication) с таким verifier невозможно ни
// при каком пароле — он не выводится ни из какого x, поэтому proof всегда
// не совпадёт. Для атакующего это выглядит как "неверный пароль", то есть
// так же, как для настоящего, но несуществующего, аккаунта.
func FakeCredentials(key []byte, identifier string) (salt, verifier []byte) {
	salt = expand(key, "srp-fake-salt", identifier, 16)
	v := new(big.Int).Mod(new(big.Int).SetBytes(expand(key, "srp-fake-verifier", identifier, nLen)), n)
	return salt, v.Bytes()
}

// expand растягивает key+label+identifier до length байт через HMAC-SHA256
// со счётчиком (тот же приём, что HKDF-expand) — детерминированный источник
// псевдослучайных байт произвольной длины из короткого секрета.
func expand(key []byte, label, identifier string, length int) []byte {
	out := make([]byte, 0, length)
	for counter := byte(1); len(out) < length; counter++ {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(label))
		mac.Write([]byte{0})
		mac.Write([]byte(identifier))
		mac.Write([]byte{counter})
		out = append(out, mac.Sum(nil)...)
	}
	return out[:length]
}

// NewServerChallenge генерирует эфемерные серверные значения SRP-обмена:
// secret (b) нужно сохранить и передать в VerifyClientProof на втором шаге,
// public (B = (k*v + g^b) mod N) отправляется клиенту вместе с солью.
func NewServerChallenge(verifier []byte) (secret, public []byte, err error) {
	if err := checkSize(verifier); err != nil {
		return nil, nil, err
	}
	v := new(big.Int).SetBytes(verifier)

	b, err := randomScalar()
	if err != nil {
		return nil, nil, err
	}

	B := new(big.Int).Mod(
		new(big.Int).Add(new(big.Int).Mul(k, v), new(big.Int).Exp(g, b, n)),
		n,
	)
	if B.Sign() == 0 {
		return NewServerChallenge(verifier) // вероятность исчезающе мала
	}

	return b.Bytes(), B.Bytes(), nil
}

// VerifyClientProof проверяет доказательство клиента (clientProof, M1) по
// сохранённым verifier и secret (b, из NewServerChallenge), восстановленному
// serverPublic (B) и присланным клиентом clientPublic (A) и clientProof (M1).
// При успехе возвращает серверное доказательство (M2) для ответа клиенту.
func VerifyClientProof(verifier, secret, serverPublic, clientPublic, clientProof []byte) (serverProof []byte, err error) {
	for _, b := range [][]byte{verifier, secret, serverPublic, clientPublic, clientProof} {
		if err := checkSize(b); err != nil {
			return nil, err
		}
	}

	v := new(big.Int).SetBytes(verifier)
	b := new(big.Int).SetBytes(secret)
	B := new(big.Int).SetBytes(serverPublic)
	A := new(big.Int).SetBytes(clientPublic)

	if new(big.Int).Mod(A, n).Sign() == 0 {
		return nil, ErrInvalidPublicValue
	}

	u := hashInt(pad(A.Bytes()), pad(B.Bytes()))

	// S = (A * v^u) ^ b mod N
	S := new(big.Int).Exp(
		new(big.Int).Mod(new(big.Int).Mul(A, new(big.Int).Exp(v, u, n)), n),
		b, n,
	)
	K := hash(S.Bytes())

	expectedM1 := hash(pad(A.Bytes()), pad(B.Bytes()), K)
	if subtle.ConstantTimeCompare(expectedM1, clientProof) != 1 {
		return nil, ErrProofMismatch
	}

	return hash(pad(A.Bytes()), expectedM1, K), nil
}

// NewClientEphemeral генерирует эфемерный секрет клиента (a) и его
// публичное значение A = g^a mod N.
func NewClientEphemeral() (secret, public []byte, err error) {
	a, err := randomScalar()
	if err != nil {
		return nil, nil, err
	}
	A := new(big.Int).Exp(g, a, n)
	return a.Bytes(), A.Bytes(), nil
}

// ComputeClientProof — клиентская сторона второго шага: по salt и паролю
// (которые нигде, кроме памяти клиента, не хранятся), собственному
// эфемерному secret (a, из NewClientEphemeral) и полученным от сервера
// clientPublic (A, тому же, что вернул NewClientEphemeral) и serverPublic
// (B) вычисляет доказательство (M1) и ключ сессии (нужен для последующей
// проверки серверного доказательства в VerifyServerProof).
func ComputeClientProof(identifier, password string, salt, clientSecret, clientPublic, serverPublic []byte) (proof, sessionKey []byte, err error) {
	for _, b := range [][]byte{clientSecret, clientPublic, serverPublic} {
		if err := checkSize(b); err != nil {
			return nil, nil, err
		}
	}

	a := new(big.Int).SetBytes(clientSecret)
	A := new(big.Int).SetBytes(clientPublic)
	B := new(big.Int).SetBytes(serverPublic)

	if new(big.Int).Mod(B, n).Sign() == 0 {
		return nil, nil, ErrInvalidPublicValue
	}

	x := computeX(identifier, password, salt)
	u := hashInt(pad(A.Bytes()), pad(B.Bytes()))

	// S = (B - k*g^x) ^ (a + u*x) mod N
	kgx := new(big.Int).Mod(new(big.Int).Mul(k, new(big.Int).Exp(g, x, n)), n)
	base := new(big.Int).Mod(new(big.Int).Sub(B, kgx), n)
	exp := new(big.Int).Add(a, new(big.Int).Mul(u, x))
	S := new(big.Int).Exp(base, exp, n)

	K := hash(S.Bytes())
	M1 := hash(pad(A.Bytes()), pad(B.Bytes()), K)
	return M1, K, nil
}

// VerifyServerProof — клиентская проверка серверного доказательства (M2) по
// clientPublic (A), собственному clientProof (M1) и sessionKey (K),
// полученным из ComputeClientProof. Успешная проверка означает, что сервер
// тоже знает verifier (взаимная аутентификация).
func VerifyServerProof(serverProof, clientPublic, clientProof, sessionKey []byte) error {
	if err := checkSize(clientPublic); err != nil {
		return err
	}
	expected := hash(pad(clientPublic), clientProof, sessionKey)
	if subtle.ConstantTimeCompare(expected, serverProof) != 1 {
		return ErrProofMismatch
	}
	return nil
}
