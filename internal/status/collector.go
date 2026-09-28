package status

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/thumbops/agent/internal/protocol"
)

// Collector keeps nodes, pods and deployments in informer caches (watch),
// so building a summary never queries the API server.
type Collector struct {
	factory informers.SharedInformerFactory
	nodes   corelisters.NodeLister
	pods    corelisters.PodLister
	deps    appslisters.DeploymentLister
	synced  []cache.InformerSynced
	exclude []string
}

// NewCollector needs get, list and watch on nodes, pods and deployments
// (the thumbops-agent-status ClusterRole). Without them the caches never
// sync and no status is sent; actions are not affected.
func NewCollector(cs kubernetes.Interface, excludeNamespaces []string) *Collector {
	f := informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithTransform(stripForStatus))
	nodes := f.Core().V1().Nodes()
	pods := f.Core().V1().Pods()
	deps := f.Apps().V1().Deployments()
	return &Collector{
		factory: f,
		nodes:   nodes.Lister(),
		pods:    pods.Lister(),
		deps:    deps.Lister(),
		synced:  []cache.InformerSynced{nodes.Informer().HasSynced, pods.Informer().HasSynced, deps.Informer().HasSynced},
		exclude: excludeNamespaces,
	}
}

// Start begins watching; it returns immediately and stops with ctx.
func (c *Collector) Start(ctx context.Context) {
	c.factory.Start(ctx.Done())
}

// Collect summarizes the cached state at the given time. ok is false until
// every cache has synced once.
func (c *Collector) Collect(at time.Time) (s protocol.ClusterStatus, ok bool) {
	for _, synced := range c.synced {
		if !synced() {
			return s, false
		}
	}
	nodes, err := c.nodes.List(labels.Everything())
	if err != nil {
		return s, false
	}
	pods, err := c.pods.List(labels.Everything())
	if err != nil {
		return s, false
	}
	deps, err := c.deps.List(labels.Everything())
	if err != nil {
		return s, false
	}
	return Summarize(nodes, pods, deps, Options{Now: at, ExcludeNamespaces: c.exclude}), true
}

// stripForStatus drops what the summary never reads, to keep the caches
// small on big clusters: managed fields and annotations (which can hold a
// whole last-applied configuration).
func stripForStatus(obj any) (any, error) {
	if m, err := meta.Accessor(obj); err == nil {
		m.SetManagedFields(nil)
		m.SetAnnotations(nil)
	}
	return obj, nil
}
