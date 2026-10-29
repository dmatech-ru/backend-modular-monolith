// Package template — шаблоны сообщений, встроенные в бинарь (go:embed),
// без БД и отдельного сервиса: шаблоны правятся через деплой, не через
// админку, а такого требования (редактировать копирайт без деплоя) не
// было. HTML (email) собирается через html/template — автоэскейпинг
// переменных защищает от HTML-инъекции через значение любой из variables;
// plain-text каналы (SMS/Telegram/MAX) — через text/template, где эскейпинг
// не нужен и не имел бы смысла.
//
// Шаблоны локализованы: файлы лежат в templates/<key>/<locale>/<файл>, см.
// локаль как её определяет internal/pkg/locale (единственное место в
// проекте, где локаль запроса влияет на реальный текст, а не на ключ
// ошибки — получатель здесь живой человек, не клиентское приложение, см.
// doc-комментарий пакета locale). Отсутствие перевода для конкретной
// локали не ошибка запуска и не ошибка отправки: Render молча использует
// locale.Default, если файла для запрошенной локали нет.
//
// email.body.tmpl — не самостоятельный документ, а блок "content",
// подставляемый в общую для всех key/locale вёрстку email_layout.tmpl
// (см. этот файл рядом с template.go): вёрстка/разметка письма живёт в
// одном месте, а не копируется в каждую пару key×locale. Раскладка —
// обычное для html/template "extends"-наследование через Clone +
// {{define "content"}}: layout зовёт {{template "content" .}}, Go на
// старте клонирует базовую вёрстку под каждый email.body.tmpl и
// подключает к клону его "content". Локаль прокинута в сам шаблон
// (не только в выбор файла) через поле Locale в данных рендера — это
// то, что нужно, например, верстке для <html lang="...">.
package template

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"strings"
	texttemplate "text/template"

	deliveryv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/delivery"
	templatesv1 "dmatech.ru/internal/pkg/contracts/peer/common/v1/templates"
	"dmatech.ru/internal/pkg/locale"
)

//go:embed templates
var templatesFS embed.FS

//go:embed email_layout.tmpl
var emailLayoutSrc string

// emailData — данные, с которыми исполняется email_layout.tmpl (и через
// {{template "content" .}} — вложенный в неё content-блок конкретного
// email.body.tmpl): Vars — переменные подстановки (то, что раньше было
// самим корнем данных — content-блоки обращаются к ним как .Vars.code),
// Locale — для вёрстки (например, <html lang="{{.Locale}}">), а не только
// для выбора файла.
type emailData struct {
	Locale locale.Locale
	Vars   map[string]string
}

// ErrTemplateNotFound — для (templateKey, channel) нет файла шаблона:
// либо templateKey не из peer.common.templates.v1.TemplateKey, либо для этого
// канала у него просто нет файла (например, его ещё не завели для MAX).
var ErrTemplateNotFound = errors.New("template: no template file for this key/channel")

// Renderer хранит уже распарсенные шаблоны (загружены один раз в New —
// соответствие шаблонов должно быть видно сразу при старте модуля, а не
// только при первой реальной отправке). Карты после New не изменяются,
// поэтому конкурентные Render безопасны без мьютекса.
type Renderer struct {
	text map[string]*texttemplate.Template
	html map[string]*htmltemplate.Template
}

// New загружает и парсит все embedded-шаблоны. Ошибка в любом из них —
// ошибка запуска модуля notifications, а не сбой при первой отправке.
func New() (*Renderer, error) {
	baseLayout, err := htmltemplate.New("email_layout").Parse(emailLayoutSrc)
	if err != nil {
		return nil, fmt.Errorf("template: parse email_layout.tmpl: %w", err)
	}

	r := &Renderer{
		text: map[string]*texttemplate.Template{},
		html: map[string]*htmltemplate.Template{},
	}

	err = fs.WalkDir(templatesFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, "templates/")

		data, err := templatesFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %q: %w", rel, err)
		}

		if strings.HasSuffix(rel, "/email.body.tmpl") {
			// email.body.tmpl определяет только {{define "content"}}...{{end}}
			// (см. doc-комментарий пакета) — клон общей вёрстки, не
			// самостоятельный шаблон, поэтому Clone() каждый раз: один
			// "content" на клон, у самой email_layout этого define нет.
			tmpl, err := baseLayout.Clone()
			if err != nil {
				return fmt.Errorf("clone email_layout for %q: %w", rel, err)
			}
			if _, err := tmpl.Parse(string(data)); err != nil {
				return fmt.Errorf("parse %q: %w", rel, err)
			}
			r.html[rel] = tmpl
			return nil
		}

		tmpl, err := texttemplate.New(rel).Parse(string(data))
		if err != nil {
			return fmt.Errorf("parse %q: %w", rel, err)
		}
		r.text[rel] = tmpl
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("template: load templates: %w", err)
	}
	return r, nil
}

// Render рендерит шаблон templateKey для канала channel на локали loc с
// переменными variables. subject заполнен только для DELIVERY_CHANNEL_EMAIL
// (остальные каналы заголовка не имеют). Если для loc нет файла — молча
// используется locale.Default (см. doc-комментарий пакета): отсутствие
// перевода не должно блокировать отправку.
func (r *Renderer) Render(templateKey templatesv1.TemplateKey, channel deliveryv1.DeliveryChannel, loc locale.Locale, variables map[string]string) (subject, body string, err error) {
	keyDir, err := templateKeyToken(templateKey)
	if err != nil {
		return "", "", err
	}
	chToken, err := channelToken(channel)
	if err != nil {
		return "", "", err
	}

	if channel == deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL {
		subject, err = r.execText(candidatePaths(keyDir, loc, "email.subject.tmpl"), variables)
		if err != nil {
			return "", "", err
		}
		body, err = r.execHTML(candidatePaths(keyDir, loc, "email.body.tmpl"), emailData{Locale: loc, Vars: variables})
		if err != nil {
			return "", "", err
		}
		return strings.TrimSpace(subject), body, nil
	}

	body, err = r.execText(candidatePaths(keyDir, loc, chToken+".body.tmpl"), variables)
	if err != nil {
		return "", "", err
	}
	return "", body, nil
}

// candidatePaths — путь для запрошенной локали, затем (если она не
// locale.Default) путь для locale.Default как запасной вариант.
func candidatePaths(keyDir string, loc locale.Locale, filename string) []string {
	paths := []string{keyDir + "/" + string(loc) + "/" + filename}
	if loc != locale.Default {
		paths = append(paths, keyDir+"/"+string(locale.Default)+"/"+filename)
	}
	return paths
}

func (r *Renderer) execText(paths []string, variables map[string]string) (string, error) {
	for _, path := range paths {
		tmpl, ok := r.text[path]
		if !ok {
			continue
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, variables); err != nil {
			return "", fmt.Errorf("template: execute %q: %w", path, err)
		}
		return buf.String(), nil
	}
	return "", fmt.Errorf("%w: %s", ErrTemplateNotFound, paths[0])
}

func (r *Renderer) execHTML(paths []string, data emailData) (string, error) {
	for _, path := range paths {
		tmpl, ok := r.html[path]
		if !ok {
			continue
		}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "email_layout", data); err != nil {
			return "", fmt.Errorf("template: execute %q: %w", path, err)
		}
		return buf.String(), nil
	}
	return "", fmt.Errorf("%w: %s", ErrTemplateNotFound, paths[0])
}

// templateKeyToken — каталог внутри templates/ для каждого значения enum.
// Новый шаблон = новое значение enum в webapi/v1/templates/templates.proto
// + новый case здесь + файлы в templates/<token>/.
func templateKeyToken(key templatesv1.TemplateKey) (string, error) {
	switch key {
	case templatesv1.TemplateKey_TEMPLATE_KEY_OTP_CODE:
		return "otp_code", nil
	case templatesv1.TemplateKey_TEMPLATE_KEY_PASSWORD_RESET_CODE:
		return "password_reset_code", nil
	case templatesv1.TemplateKey_TEMPLATE_KEY_PASSWORD_CHANGED:
		return "password_changed", nil
	default:
		return "", fmt.Errorf("%w: template_key %v", ErrTemplateNotFound, key)
	}
}

func channelToken(channel deliveryv1.DeliveryChannel) (string, error) {
	switch channel {
	case deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL:
		return "email", nil
	case deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_SMS:
		return "sms", nil
	case deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_TELEGRAM:
		return "telegram", nil
	case deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_MAX:
		return "max", nil
	default:
		return "", fmt.Errorf("template: unknown delivery channel %v", channel)
	}
}
