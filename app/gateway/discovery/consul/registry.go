package consul

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kratos/kratos/v2/registry"
	"github.com/hashicorp/consul/api"
)

// Registry implements registry.Discovery backed by Consul's health API.
//
// Unlike kratos/contrib/registry/consul, the polling goroutine for a service
// is started exactly once and survives transient consul failures. The upstream
// implementation stores the serviceSet before the initial resolve and, when
// that resolve errors (e.g. consul still starting during a cold boot), returns
// an error while leaving a dead serviceSet behind — every later Watch for the
// same name then returns a watcher that never receives events, so the gateway
// gets stuck on no_available_node until the process restarts.
type Registry struct {
	cli     *api.Client
	timeout time.Duration

	lock     sync.Mutex
	services map[string]*serviceSet
}

// NewRegistry creates a consul-backed discovery registry.
func NewRegistry(cli *api.Client) *Registry {
	return &Registry{
		cli:      cli,
		services: make(map[string]*serviceSet),
	}
}

type serviceSet struct {
	serviceName string
	services    atomic.Value // []*registry.ServiceInstance

	lock     sync.RWMutex
	watchers map[*watcher]struct{}

	ctx    context.Context
	cancel context.CancelFunc
}

func (s *serviceSet) broadcast(services []*registry.ServiceInstance) {
	s.services.Store(services)
	s.lock.RLock()
	defer s.lock.RUnlock()
	for w := range s.watchers {
		select {
		case w.event <- struct{}{}:
		default:
		}
	}
}

func (s *serviceSet) current() []*registry.ServiceInstance {
	ss, _ := s.services.Load().([]*registry.ServiceInstance)
	return ss
}

type watcher struct {
	event chan struct{}
	set   *serviceSet

	ctx    context.Context
	cancel context.CancelFunc
}

func (w *watcher) Next() ([]*registry.ServiceInstance, error) {
	select {
	case <-w.ctx.Done():
		return nil, w.ctx.Err()
	case <-w.event:
	}
	return w.set.current(), nil
}

func (w *watcher) Stop() error {
	w.cancel()
	w.set.lock.Lock()
	defer w.set.lock.Unlock()
	delete(w.set.watchers, w)
	return nil
}

// GetService returns the cached instances, falling back to a live query.
func (r *Registry) GetService(ctx context.Context, name string) ([]*registry.ServiceInstance, error) {
	if set := r.getSet(name); set != nil {
		if cur := set.current(); len(cur) > 0 {
			return cur, nil
		}
	}
	services, _, err := r.query(ctx, name, 0)
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("service %s not found in registry", name)
	}
	return services, nil
}

// Watch creates a watcher for the service. The polling goroutine is created on
// the first watch for a name and retries forever on transient errors, so a
// connection-refused during cold start can never permanently break discovery.
func (r *Registry) Watch(ctx context.Context, name string) (registry.Watcher, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	set := r.getOrCreateSet(name)

	wctx, wcancel := context.WithCancel(ctx)
	w := &watcher{
		event:  make(chan struct{}, 1),
		set:    set,
		ctx:    wctx,
		cancel: wcancel,
	}
	set.lock.Lock()
	set.watchers[w] = struct{}{}
	if cur := set.current(); len(cur) > 0 {
		select {
		case w.event <- struct{}{}:
		default:
		}
	}
	set.lock.Unlock()
	return w, nil
}

func (r *Registry) getSet(name string) *serviceSet {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.services[name]
}

func (r *Registry) getOrCreateSet(name string) *serviceSet {
	r.lock.Lock()
	defer r.lock.Unlock()
	if set, ok := r.services[name]; ok {
		return set
	}
	ctx, cancel := context.WithCancel(context.Background())
	set := &serviceSet{
		serviceName: name,
		watchers:    make(map[*watcher]struct{}),
		ctx:         ctx,
		cancel:      cancel,
	}
	r.services[name] = set
	go r.poll(ctx, set)
	return set
}

func (r *Registry) query(ctx context.Context, name string, index uint64) ([]*registry.ServiceInstance, uint64, error) {
	opts := &api.QueryOptions{
		WaitIndex: index,
		WaitTime:  time.Second * 55,
	}
	opts = opts.WithContext(ctx)
	entries, meta, err := r.cli.Health().Service(name, "", true, opts)
	if err != nil {
		return nil, 0, err
	}
	services := make([]*registry.ServiceInstance, 0, len(entries))
	for _, entry := range entries {
		var version string
		for _, tag := range entry.Service.Tags {
			if v, ok := strings.CutPrefix(tag, "version="); ok {
				version = v
				break
			}
		}
		endpoints := make([]string, 0, len(entry.Service.TaggedAddresses))
		for scheme, addr := range entry.Service.TaggedAddresses {
			if scheme == "lan_ipv4" || scheme == "wan_ipv4" || scheme == "lan_ipv6" || scheme == "wan_ipv6" {
				continue
			}
			endpoints = append(endpoints, addr.Address)
		}
		if len(endpoints) == 0 && entry.Service.Address != "" && entry.Service.Port != 0 {
			endpoints = append(endpoints, fmt.Sprintf("http://%s:%d", entry.Service.Address, entry.Service.Port))
		}
		services = append(services, &registry.ServiceInstance{
			ID:        entry.Service.ID,
			Name:      entry.Service.Service,
			Metadata:  entry.Service.Meta,
			Version:   version,
			Endpoints: endpoints,
		})
	}
	return services, meta.LastIndex, nil
}

func (r *Registry) poll(ctx context.Context, set *serviceSet) {
	// Initial resolve: bounded timeout, exponential backoff on failure so a
	// cold-start window where consul is not yet accepting connections is
	// retried instead of being fatal.
	idx := uint64(0)
	backoff := time.Second
	for {
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		services, lastIndex, err := r.query(qctx, set.serviceName, 0)
		cancel()
		if err == nil {
			if len(services) > 0 {
				set.broadcast(services)
			}
			idx = lastIndex
			break
		}
		if ctx.Err() != nil {
			return
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			qctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			services, lastIndex, err := r.query(qctx, set.serviceName, idx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if len(services) != 0 && lastIndex != idx {
				set.broadcast(services)
			}
			idx = lastIndex
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}