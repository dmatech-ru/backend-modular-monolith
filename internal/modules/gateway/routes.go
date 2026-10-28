package gateway

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"dmatech.ru/internal/pkg/natsrpc"
)

type route struct {
	Service    string    `json:"service"`
	Version    string    `json:"version"`
	Descriptor []byte    `json:"-"`
	Size       int       `json:"descriptorSize"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// routeTable — потокобезопасная таблица маршрутов, наполняемая из discovery.
type routeTable struct {
	mu     sync.RWMutex
	routes map[string]route
}

func newRouteTable() *routeTable {
	return &routeTable{routes: make(map[string]route)}
}

// Apply применяет обновление из natsrpc.WatchRoutes.
func (t *routeTable) Apply(u natsrpc.RouteUpdate) {
	id := u.ServiceName + "." + u.Version

	t.mu.Lock()
	defer t.mu.Unlock()

	if u.IsDeleted {
		delete(t.routes, id)
		return
	}
	// TODO: компилировать дескриптор в обработчики проксирования.
	t.routes[id] = route{
		Service:    u.ServiceName,
		Version:    u.Version,
		Descriptor: u.Descriptor,
		Size:       len(u.Descriptor),
		UpdatedAt:  time.Now(),
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
