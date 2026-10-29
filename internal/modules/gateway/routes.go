package gateway

import (
	"cmp"
	"log/slog"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	optionsv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/options"
	"dmatech.ru/internal/pkg/natsdiscovery"
)

type route struct {
	Service    string    `json:"service"`
	Version    string    `json:"version"`
	Descriptor []byte    `json:"-"`
	Size       int       `json:"descriptorSize"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// Procedures — полные имена gRPC/Connect-сервисов (package.Service),
	// извлечённые из дескриптора; по ним строится маршрутизация в handleProxy.
	Procedures []string `json:"procedures,omitempty"`
	// PublicMethods — какие "/package.Service/Method" из этого маршрута не
	// требуют access-токена (см. (webapi.options.v1.public) в .proto и
	// doc-комментарий handleAuth) — default-deny: метод, которого здесь нет,
	// требует валидный токен.
	PublicMethods map[string]bool `json:"publicMethods,omitempty"`
}

// routeTable — потокобезопасная таблица маршрутов, наполняемая из discovery.
type routeTable struct {
	log *slog.Logger

	mu     sync.RWMutex
	routes map[string]route
	// services индексирует route по полному имени gRPC/Connect-сервиса,
	// чтобы handleProxy мог найти владельца запроса по пути "/package.Service/Method".
	services map[string]string
}

func newRouteTable(log *slog.Logger) *routeTable {
	return &routeTable{
		log:      log,
		routes:   make(map[string]route),
		services: make(map[string]string),
	}
}

// Apply применяет обновление из natsdiscovery.WatchRoutes.
func (t *routeTable) Apply(u natsdiscovery.RouteUpdate) {
	id := u.ServiceName + "." + u.Version

	t.mu.Lock()
	defer t.mu.Unlock()

	if old, ok := t.routes[id]; ok {
		for _, svc := range old.Procedures {
			delete(t.services, svc)
		}
	}

	if u.IsDeleted {
		delete(t.routes, id)
		return
	}

	procedures, publicMethods, err := extractServiceInfo(u.Descriptor)
	if err != nil {
		t.log.Warn("Failed to parse route descriptor, proxying for this route is unavailable",
			"service", u.ServiceName, "version", u.Version, "error", err)
	}

	t.routes[id] = route{
		Service:       u.ServiceName,
		Version:       u.Version,
		Descriptor:    u.Descriptor,
		Size:          len(u.Descriptor),
		UpdatedAt:     time.Now(),
		Procedures:    procedures,
		PublicMethods: publicMethods,
	}
	for _, svc := range procedures {
		t.services[svc] = id
	}
}

// List возвращает маршруты, отсортированные по сервису и версии.
func (t *routeTable) List() []route {
	t.mu.RLock()
	list := make([]route, 0, len(t.routes))
	for _, r := range t.routes {
		list = append(list, r)
	}
	t.mu.RUnlock()

	slices.SortFunc(list, func(a, b route) int {
		return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Version, b.Version))
	})
	return list
}

// LookupService находит маршрут, которому принадлежит gRPC/Connect-сервис
// с полным именем serviceFullName (например, "webapi.identity.auth.v1.IdentityAuthService").
func (t *routeTable) LookupService(serviceFullName string) (route, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	id, ok := t.services[serviceFullName]
	if !ok {
		return route{}, false
	}
	r, ok := t.routes[id]
	return r, ok
}

// extractServiceInfo разбирает дескриптор (protobuf.FileDescriptorSet, см.
// protoset.Build) и возвращает полные имена описанных в нём сервисов
// (procedures, как раньше) и то, какие из их методов отмечены
// (webapi.options.v1.public) = true (publicMethods, ключ — ровно тот же
// "/package.Service/Method", что handleProxy вычисляет из r.URL.Path).
//
// proto.GetExtension видит это расширение только потому, что его
// сгенерированный Go-пакет (optionsv1) где-то импортирован этим бинарём
// (gateway) — импорт регистрирует расширение в protoregistry.GlobalTypes,
// после чего proto.Unmarshal начинает его декодировать как обычное поле, а
// не как неизвестные байты (тот же механизм, что уже используется для
// buf.validate.field, см. internal/pkg/validation).
func extractServiceInfo(descriptor []byte) (procedures []string, publicMethods map[string]bool, err error) {
	if len(descriptor) == 0 {
		return nil, nil, nil
	}

	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(descriptor, &set); err != nil {
		return nil, nil, err
	}

	publicMethods = make(map[string]bool)
	for _, f := range set.GetFile() {
		pkg := f.GetPackage()
		for _, svc := range f.GetService() {
			svcFullName := svc.GetName()
			if pkg != "" {
				svcFullName = pkg + "." + svc.GetName()
			}
			procedures = append(procedures, svcFullName)

			for _, m := range svc.GetMethod() {
				if public, _ := proto.GetExtension(m.GetOptions(), optionsv1.E_Public).(bool); public {
					publicMethods["/"+svcFullName+"/"+m.GetName()] = true
				}
			}
		}
	}
	return procedures, publicMethods, nil
}
