// Package template — шаблоны сообщений, встроенные в бинарь (go:embed),
// без БД и отдельного сервиса: шаблоны правятся через деплой, не через
// админку, а такого требования (редактировать копирайт без деплоя) не
// было. HTML (email) собирается через html/template — автоэскейпинг
// переменных защищает от HTML-инъекции через значение любой из variables;
// plain-text каналы (SMS/Telegram/MAX) — через text/template, где эскейпинг
// не нужен и не имел бы смысла.
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
)

//go:embed templates
var templatesFS embed.FS

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
	r := &Renderer{
		text: map[string]*texttemplate.Template{},
		html: map[string]*htmltemplate.Template{},
	}

	err := fs.WalkDir(templatesFS, "templates", func(path string, d fs.DirEntry, err error) error {
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
			tmpl, err := htmltemplate.New(rel).Parse(string(data))
			if err != nil {
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

// Render рендерит шаблон templateKey для канала channel с переменными
// variables. subject заполнен только для DELIVERY_CHANNEL_EMAIL (остальные
// каналы заголовка не имеют).
func (r *Renderer) Render(templateKey templatesv1.TemplateKey, channel deliveryv1.DeliveryChannel, variables map[string]string) (subject, body string, err error) {
	keyDir, err := templateKeyToken(templateKey)
	if err != nil {
		return "", "", err
	}
	chToken, err := channelToken(channel)
	if err != nil {
		return "", "", err
	}

	if channel == deliveryv1.DeliveryChannel_DELIVERY_CHANNEL_EMAIL {
		subject, err = r.execText(keyDir+"/email.subject.tmpl", variables)
		if err != nil {
			return "", "", err
		}
		body, err = r.execHTML(keyDir+"/email.body.tmpl", variables)
		if err != nil {
			return "", "", err
		}
		return strings.TrimSpace(subject), body, nil
	}

	body, err = r.execText(keyDir+"/"+chToken+".body.tmpl", variables)
	if err != nil {
		return "", "", err
	}
	return "", body, nil
}

func (r *Renderer) execText(path string, variables map[string]string) (string, error) {
	tmpl, ok := r.text[path]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrTemplateNotFound, path)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, variables); err != nil {
		return "", fmt.Errorf("template: execute %q: %w", path, err)
	}
	return buf.String(), nil
}

func (r *Renderer) execHTML(path string, variables map[string]string) (string, error) {
	tmpl, ok := r.html[path]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrTemplateNotFound, path)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, variables); err != nil {
		return "", fmt.Errorf("template: execute %q: %w", path, err)
	}
	return buf.String(), nil
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
