package k8s

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/flytek8s/config"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/io"
	"github.com/lyft/flytestdlib/contextutils"
	stdErrors "github.com/lyft/flytestdlib/errors"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/lyft/flytestdlib/promutils"
	"github.com/lyft/flytestdlib/promutils/labeled"

	pluginsCore "github.com/lyft/flyteplugins/go/tasks/pluginmachinery/core"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/ioutils"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/k8s"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/utils"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/lyft/flytestdlib/logger"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/lyft/flyteplugins/go/tasks/errors"

	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const finalizer = "flyte/flytek8s"

const pluginStateVersion = 1

type PluginPhase uint8

const (
	PluginPhaseNotStarted PluginPhase = iota
	PluginPhaseAllocationTokenAcquired
	PluginPhaseStarted
)

type PluginState struct {
	Phase PluginPhase
}

type PluginMetrics struct {
	Scope           promutils.Scope
	GetCacheMiss    labeled.StopWatch
	GetCacheHit     labeled.StopWatch
	GetAPILatency   labeled.StopWatch
	ResourceDeleted labeled.Counter
}

func newPluginMetrics(s promutils.Scope) PluginMetrics {
	return PluginMetrics{
		Scope: s,
		GetCacheMiss: labeled.NewStopWatch("get_cache_miss", "Cache miss on get resource calls.",
			time.Millisecond, s),
		GetCacheHit: labeled.NewStopWatch("get_cache_hit", "Cache miss on get resource calls.",
			time.Millisecond, s),
		GetAPILatency: labeled.NewStopWatch("get_api", "Latency for APIServer Get calls.",
			time.Millisecond, s),
		ResourceDeleted: labeled.NewCounter("pods_deleted", "Counts how many times CheckTaskStatus is"+
			" called with a deleted resource.", s),
	}
}

func AddObjectMetadata(taskCtx pluginsCore.TaskExecutionMetadata, o k8s.Resource, cfg *config.K8sPluginConfig) {
	o.SetNamespace(taskCtx.GetNamespace())
	o.SetAnnotations(utils.UnionMaps(cfg.DefaultAnnotations, o.GetAnnotations(), utils.CopyMap(taskCtx.GetAnnotations())))
	o.SetLabels(utils.UnionMaps(o.GetLabels(), utils.CopyMap(taskCtx.GetLabels()), cfg.DefaultLabels))
	o.SetOwnerReferences([]metav1.OwnerReference{taskCtx.GetOwnerReference()})
	o.SetName(taskCtx.GetTaskExecutionID().GetGeneratedName())
	if cfg.InjectFinalizer {
		f := append(o.GetFinalizers(), finalizer)
		o.SetFinalizers(f)
	}
}

func IsK8sObjectNotExists(err error) bool {
	return k8serrors.IsNotFound(err) || k8serrors.IsGone(err) || k8serrors.IsResourceExpired(err)
}

// A generic Plugin for managing k8s-resources. Plugin writers wishing to use K8s resource can use the simplified api specified in
// pluginmachinery.core
type PluginManager struct {
	id              string
	plugin          k8s.Plugin
	resourceToWatch runtime.Object
	kubeClient      pluginsCore.KubeClient
	metrics         PluginMetrics
	// Per namespace-resource
	// backoffHandlers map[string]*ResourceAwareBackOffHandler // TODO: make this thread-safe
	backOffHandlers BackOffHandlerMap
}

func (e *PluginManager) GetProperties() pluginsCore.PluginProperties {
	return pluginsCore.PluginProperties{}
}

func (e *PluginManager) GetID() string {
	return e.id
}

type BackOffHandlerMap struct {
	sync.Map
}

func (m *BackOffHandlerMap) Set(key string, value *ResourceAwareBackOffHandler) {
	m.Store(key, value)
}

func (m *BackOffHandlerMap) Get(key string) (*ResourceAwareBackOffHandler, bool) {
	value, found := m.Load(key)
	if found == false {
		return nil, false
	} else {
		h, ok := value.(*ResourceAwareBackOffHandler)
		return h, found && ok
	}
}

type BackOffConstraintComparator func(int, int) bool

const backOffBase = time.Second * 2     // b in b^n
const maxBackoffTime = time.Minute * 10 // the max time is 10 minutes

type SimpleBackOffBlocker struct {
	backOffExponent  int
	nextEligibleTime time.Time
}

// act based on current backoff interval and set the next one accordingly
func (b *SimpleBackOffBlocker) handle(operation func() error) error {

	// check if current backoff interval has elapsed
	now := time.Now()
	if b.nextEligibleTime.Before(now) {
		err := operation() // execute request
		if err == nil {
			b.backOffExponent = 0
			b.nextEligibleTime = time.Now()
			return nil
		} else {
			backOffDuration := time.Duration(math.Pow(float64(backOffBase), float64(b.backOffExponent)))
			if backOffDuration > maxBackoffTime {
				backOffDuration = maxBackoffTime
			}

			b.nextEligibleTime = time.Now().Add(backOffDuration)
			b.backOffExponent += 1

			// TODO ssingh move this error to some better place
			return errors.Wrapf("BackOff", err, "Failed to still not")
		}
	}

	return errors.Errorf("BackOff", "need to wait more")
}

func (b *SimpleBackOffBlocker) isActive(t time.Time) bool {
	return b.nextEligibleTime.Before(t)
}

func (b *SimpleBackOffBlocker) getBlockExpirationTime() time.Time {
	return b.nextEligibleTime
}

func (b *SimpleBackOffBlocker) reset() {
	b.backOffExponent = 0
	b.nextEligibleTime = time.Now()
}

type ResourceCeilings struct {
	resourceCeilings v1.ResourceList
}

func (r *ResourceCeilings) isEligible(requestedResourceList v1.ResourceList) bool {
	eligibility := true
	for reqResource, reqQuantity := range requestedResourceList {
		eligibility = eligibility && (reqQuantity.Cmp(r.resourceCeilings[reqResource]) == -1)
	}
	return eligibility
}

func (r *ResourceCeilings) update(reqResource v1.ResourceName, reqQuantity resource.Quantity) {
	if currentCeiling, ok := r.resourceCeilings[reqResource]; !ok || reqQuantity.Cmp(currentCeiling) == -1 {
		r.resourceCeilings[reqResource] = reqQuantity
	}
}

func (r *ResourceCeilings) updateAll(resources v1.ResourceList) {
	for reqResource, reqQuantity := range resources {
		r.update(reqResource, reqQuantity)
	}
}

func (r *ResourceCeilings) reset(resource v1.ResourceName) {
	r.resourceCeilings[resource] = r.inf()
}

func (r *ResourceCeilings) resetAll() {
	for resource := range r.resourceCeilings {
		r.reset(resource)
	}
}

func (r *ResourceCeilings) inf() resource.Quantity {
	// A hack to represent RESOURCE_MAX
	return resource.MustParse("1Ei")
}

type ResourceAwareBackOffHandler struct {
	SimpleBackOffBlocker
	ResourceCeilings
}

// Act based on current backoff interval and set the next one accordingly
func (h *ResourceAwareBackOffHandler) handle(ctx context.Context, operation func() error, requestedResourceList v1.ResourceList) error {

	// Pseudo code:
	// If the backoff is inactive => we should just go ahead and execute the operation(), and handle the error properly
	//		If operation() fails because of resource => lower the ceiling
	//		Else we return whatever the result is
	//
	// Else if the backoff is active => we should reduce the number of calls to the API server in this case
	//		If resource is lower than the ceiling => We should try the operation().
	//			If operation() fails because of the lack of resource, we will lower the ceiling
	//          Else we return whatever the operation() returns
	//      Else => we block the operation(), which is where the main improvement comes from

	now := time.Now()
	if !h.SimpleBackOffBlocker.isActive(now) || h.ResourceCeilings.isEligible(requestedResourceList) {
		err := operation()
		if err != nil {
			if IsResourceQuotaExceeded(err) {
				logger.Errorf(ctx, "Failed to run the operation due to insufficient resource: [%v]\n", err)

				// When lowering the ceiling, we only want to lower the ceiling that actually needs to be lowered.
				// For example, if the creation of a pod requiring X cpus and Y memory got rejected because of
				// 	insufficient memory, we should only lower the ceiling of memory to Y, without touching the cpu ceiling

				newCeiling := GetResourceAndQuantityRequested(err)
				h.ResourceCeilings.updateAll(newCeiling)
			} else {
				logger.Errorf(ctx, "Failed to run the operation due to reasons other than insufficient resource: [%v]\n", err)
			}
			return errors.Wrapf("BackOff", err, "Failed to execute the operation")
		} else {
			h.SimpleBackOffBlocker.reset()
			h.ResourceCeilings.resetAll()
			return nil
		}
	} else { // The backoff is active and the resource request exceeds the ceiling
		logger.Errorf(ctx, "Failed to execute the operation due to backoff")
		return errors.Errorf("BackOff", "Failed to execute the operation due to backoff is "+
			"active [attempted at: %v][block expires at: %v] and the requested "+
			"resource(s) exceeds resource ceiling(s)", now, h.SimpleBackOffBlocker.getBlockExpirationTime())
	}
	return nil
}

func IsResourceQuotaExceeded(err error) bool {
	return k8serrors.IsForbidden(err) && strings.Contains(err.Error(), "exceeded quota")
}

func GetResourceAndQuantityRequested(err error) v1.ResourceList {
	// re := regexp.MustCompile(`(?P<part>(?P<key>requested|used|limited): limits.(?P<resource_type>[a-zA-Z]+)=(?P<quantity_expr>[a-zA-Z0-9]+))`)
	// Playground: https://play.golang.org/p/oOr6CMmW7IE
	re := regexp.MustCompile(`(?P<key>requested): limits.(?P<resource_type>[a-zA-Z]+)=(?P<quantity_expr>[a-zA-Z0-9]+)`)
	matches := re.FindAllStringSubmatch(err.Error(), -1)

	// re := regexp.MustCompile(`(?P<part>(?P<key>requested): limits.(?P<resource_type>[a-zA-Z]+)=(?P<quantity_expr>[a-zA-Z0-9]+))`)
	// matches := re.FindAllStringSubmatch(err.Error(), -1)

	var temp []map[string]string

	for i, mat := range matches {
		temp[i] = make(map[string]string)
		for j, name := range re.SubexpNames() {
			if j != 0 && name != "" {
				temp[i][name] = mat[j]
			}
		}
	}

	var requestedResources v1.ResourceList
	for _, part := range temp {
		if part["key"] != "requested" {
			continue
		}
		resourceName := v1.ResourceName(part["resource_type"])
		resourceQuantity := resource.MustParse(part["quantity_expr"])
		requestedResources[resourceName] = resourceQuantity
	}

	return requestedResources
}

// TODO ssingh: clean it and move to its right place
func IsBackoffError(err error) bool {
	code, found := stdErrors.GetErrorCode(err)
	if found && code == "BackOff" {
		return true
	}

	return false
}

func (e *PluginManager) getBackOffHandler(key string) *ResourceAwareBackOffHandler {
	if handler, found := e.backOffHandlers.Get(key); found {
		return handler
	}

	// TODO ssingh: make it threadsafe
	e.backOffHandlers.Set(key, &ResourceAwareBackOffHandler{
		SimpleBackOffBlocker: SimpleBackOffBlocker{
			backOffExponent:  0,
			nextEligibleTime: time.Now(),
		},
		// TODO changhong: initialize this field with proper value
		ResourceCeilings: ResourceCeilings{
			resourceCeilings: v1.ResourceList{},
		},
	})
	h, _ := e.backOffHandlers.Get(key)
	return h
}

func (e *PluginManager) LaunchResource(ctx context.Context, tCtx pluginsCore.TaskExecutionContext) (pluginsCore.Transition, error) {

	o, err := e.plugin.BuildResource(ctx, tCtx)
	if err != nil {
		return pluginsCore.UnknownTransition, err
	}

	AddObjectMetadata(tCtx.TaskExecutionMetadata(), o, config.GetK8sPluginConfig())
	logger.Infof(ctx, "Creating Object: Type:[%v], Object:[%v/%v]", o.GroupVersionKind(), o.GetNamespace(), o.GetName())

	key := fmt.Sprintf("%v,%v", o.GroupVersionKind().String(), o.GetNamespace())

	pod, casted := o.(*v1.Pod)
	if !casted || pod.Spec.Containers == nil {
		// return a proper error here
	}

	var podRequestedResources v1.ResourceList

	// Collect the resource requests from all the containers in the pod whose creation is to be attempted
	// to decide whether we should try the pod creation during the back off period
	for _, container := range pod.Spec.Containers {
		for k, v := range container.Resources.Limits {
			podRequestedResources[k].Add(v)
		}
	}

	err = e.getBackOffHandler(key).handle(ctx, func() error {
		return e.kubeClient.GetClient().Create(ctx, o)
	}, podRequestedResources)

	if err != nil && !k8serrors.IsAlreadyExists(err) {
		if IsBackoffError(err) {
			// TODO: Quota errors are retried forever, it would be good to have support for backoff strategy.
			logger.Warnf(ctx, "Failed to launch job, resource quota exceeded. err: %v", err)
			return pluginsCore.DoTransition(pluginsCore.PhaseInfoWaitingForResources(time.Now(), pluginsCore.DefaultPhaseVersion, "failed to launch job, resource quota exceeded.")), nil
		} else if k8serrors.IsForbidden(err) {
			return pluginsCore.DoTransition(pluginsCore.PhaseInfoRetryableFailure("RuntimeFailure", err.Error(), nil)), nil
		} else if k8serrors.IsBadRequest(err) || k8serrors.IsInvalid(err) {
			logger.Errorf(ctx, "Badly formatted resource for plugin [%s], err %s", e.id, err)
			// return pluginsCore.DoTransition(pluginsCore.PhaseInfoFailure("BadTaskFormat", err.Error(), nil)), nil
		} else if k8serrors.IsRequestEntityTooLargeError(err) {
			logger.Errorf(ctx, "Badly formatted resource for plugin [%s], err %s", e.id, err)
			return pluginsCore.DoTransition(pluginsCore.PhaseInfoFailure("EntityTooLarge", err.Error(), nil)), nil
		}
		reason := k8serrors.ReasonForError(err)
		logger.Errorf(ctx, "Failed to launch job, system error. err: %v", err)
		return pluginsCore.UnknownTransition, errors.Wrapf(stdErrors.ErrorCode(reason), err, "failed to create resource")
	}

	return pluginsCore.DoTransition(pluginsCore.PhaseInfoQueued(time.Now(), pluginsCore.DefaultPhaseVersion, "task submitted to K8s")), nil
}

func (e *PluginManager) CheckResourcePhase(ctx context.Context, tCtx pluginsCore.TaskExecutionContext) (pluginsCore.Transition, error) {

	o, err := e.plugin.BuildIdentityResource(ctx, tCtx.TaskExecutionMetadata())
	if err != nil {
		logger.Errorf(ctx, "Failed to build the Resource with name: %v. Error: %v", tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName(), err)
		return pluginsCore.DoTransition(pluginsCore.PhaseInfoFailure("BadTaskDefinition", fmt.Sprintf("Failed to build resource, caused by: %s", err.Error()), nil)), nil
	}

	AddObjectMetadata(tCtx.TaskExecutionMetadata(), o, config.GetK8sPluginConfig())
	nsName := k8stypes.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}
	// Attempt to get resource from informer cache, if not found, retrieve it from API server.
	if err := e.kubeClient.GetClient().Get(ctx, nsName, o); err != nil {
		if IsK8sObjectNotExists(err) {
			// This happens sometimes because a node gets removed and K8s deletes the pod. This will result in a
			// Pod does not exist error. This should be retried using the retry policy
			logger.Warningf(ctx, "Failed to find the Resource with name: %v. Error: %v", nsName, err)
			failureReason := fmt.Sprintf("resource not found, name [%s]. reason: %s", nsName.String(), err.Error())
			return pluginsCore.DoTransition(pluginsCore.PhaseInfoRetryableFailure("Tachycardia", failureReason, nil)), nil
		}

		logger.Warningf(ctx, "Failed to retrieve Resource Details with name: %v. Error: %v", nsName, err)
		return pluginsCore.UnknownTransition, err
	}
	if o.GetDeletionTimestamp() != nil {
		e.metrics.ResourceDeleted.Inc(ctx)
	}

	pCtx := newPluginContext(tCtx)
	p, err := e.plugin.GetTaskPhase(ctx, pCtx, o)
	if err != nil {
		logger.Warnf(ctx, "failed to check status of resource in plugin [%s], with error: %s", e.GetID(), err.Error())
		return pluginsCore.UnknownTransition, err
	}

	if p.Phase() == pluginsCore.PhaseSuccess {
		var opReader io.OutputReader
		if pCtx.ow == nil {
			logger.Infof(ctx, "Plugin [%s] returned no outputReader, assuming file based outputs", e.id)
			opReader = ioutils.NewRemoteFileOutputReader(ctx, tCtx.DataStore(), tCtx.OutputWriter(), tCtx.MaxDatasetSizeBytes())
		} else {
			logger.Infof(ctx, "Plugin [%s] returned outputReader", e.id)
			opReader = pCtx.ow.GetReader()
		}
		err := tCtx.OutputWriter().Put(ctx, opReader)
		if err != nil {
			return pluginsCore.UnknownTransition, err
		}

		return pluginsCore.DoTransition(p), nil
	}

	if !p.Phase().IsTerminal() && o.GetDeletionTimestamp() != nil {
		// If the object has been deleted, that is, it has a deletion timestamp, but is not in a terminal state, we should
		// mark the task as a retryable failure.  We've seen this happen when a kubelet disappears - all pods running on
		// the node are marked with a deletionTimestamp, but our finalizers prevent the pod from being deleted.
		// This can also happen when a user deletes a Pod directly.
		failureReason := fmt.Sprintf("object [%s] terminated in the background, manually", nsName.String())
		return pluginsCore.DoTransition(pluginsCore.PhaseInfoRetryableFailure("tachycardia", failureReason, nil)), nil
	}

	return pluginsCore.DoTransition(p), nil
}

func (e PluginManager) Handle(ctx context.Context, tCtx pluginsCore.TaskExecutionContext) (pluginsCore.Transition, error) {
	ps := PluginState{}
	if v, err := tCtx.PluginStateReader().Get(&ps); err != nil {
		if v != pluginStateVersion {
			return pluginsCore.DoTransition(pluginsCore.PhaseInfoRetryableFailure(errors.CorruptedPluginState, fmt.Sprintf("plugin state version mismatch expected [%d] got [%d]", pluginStateVersion, v), nil)), nil
		}
		return pluginsCore.UnknownTransition, errors.Wrapf(errors.CorruptedPluginState, err, "Failed to read unmarshal custom state")
	}
	if ps.Phase == PluginPhaseNotStarted {
		t, err := e.LaunchResource(ctx, tCtx)
		if err == nil && t.Info().Phase() == pluginsCore.PhaseQueued {
			if err := tCtx.PluginStateWriter().Put(pluginStateVersion, &PluginState{Phase: PluginPhaseStarted}); err != nil {
				return pluginsCore.UnknownTransition, err
			}
		}
		return t, err
	}
	return e.CheckResourcePhase(ctx, tCtx)
}

func (e PluginManager) Abort(ctx context.Context, tCtx pluginsCore.TaskExecutionContext) error {
	logger.Infof(ctx, "KillTask invoked for %v, nothing to be done.", tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName())
	return nil
}

func (e *PluginManager) ClearFinalizers(ctx context.Context, o k8s.Resource) error {
	if len(o.GetFinalizers()) > 0 {
		o.SetFinalizers([]string{})
		err := e.kubeClient.GetClient().Update(ctx, o)
		if err != nil && !IsK8sObjectNotExists(err) {
			logger.Warningf(ctx, "Failed to clear finalizers for Resource with name: %v/%v. Error: %v",
				o.GetNamespace(), o.GetName(), err)
			return err
		}
	} else {
		logger.Debugf(ctx, "Finalizers are already empty for Resource with name: %v/%v",
			o.GetNamespace(), o.GetName())
	}
	return nil
}

func (e *PluginManager) Finalize(ctx context.Context, tCtx pluginsCore.TaskExecutionContext) error {
	// If you change InjectFinalizer on the
	if config.GetK8sPluginConfig().InjectFinalizer {
		o, err := e.plugin.BuildIdentityResource(ctx, tCtx.TaskExecutionMetadata())
		if err != nil {
			// This will recurrent, so we will skip further finalize
			logger.Errorf(ctx, "Failed to build the Resource with name: %v. Error: %v, when finalizing.", tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName(), err)
			return nil
		}
		AddObjectMetadata(tCtx.TaskExecutionMetadata(), o, config.GetK8sPluginConfig())
		nsName := k8stypes.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}
		// Attempt to get resource from informer cache, if not found, retrieve it from API server.
		if err := e.kubeClient.GetClient().Get(ctx, nsName, o); err != nil {
			if IsK8sObjectNotExists(err) {
				return nil
			}
			// This happens sometimes because a node gets removed and K8s deletes the pod. This will result in a
			// Pod does not exist error. This should be retried using the retry policy
			logger.Warningf(ctx, "Failed in finalizing get Resource with name: %v. Error: %v", nsName, err)
			return err
		}

		// This must happen after sending admin event. It's safe against partial failures because if the event failed, we will
		// simply retry in the next round. If the event succeeded but this failed, we will try again the next round to send
		// the same event (idempotent) and then come here again...
		err = e.ClearFinalizers(ctx, o)
		if err != nil {
			return err
		}
	}
	return nil
}

// Creates a K8s generic task executor. This provides an easier way to build task executors that create K8s resources.
func NewPluginManager(ctx context.Context, iCtx pluginsCore.SetupContext, entry k8s.PluginEntry) (*PluginManager, error) {
	if iCtx.EnqueueOwner() == nil {
		return nil, errors.Errorf(errors.PluginInitializationFailed, "Failed to initialize plugin, enqueue Owner cannot be nil or empty.")
	}

	if iCtx.KubeClient() == nil {
		return nil, errors.Errorf(errors.PluginInitializationFailed, "Failed to initialize K8sResource Plugin, Kubeclient cannot be nil!")
	}

	logger.Infof(ctx, "Initializing K8s plufgin [%s]", entry.ID)
	src := source.Kind{
		Type: entry.ResourceToWatch,
	}

	ownerKind := iCtx.OwnerKind()
	workflowParentPredicate := func(o metav1.Object) bool {
		ownerReference := metav1.GetControllerOf(o)
		if ownerReference != nil {
			if ownerReference.Kind == ownerKind {
				return true
			}
		}
		return false
	}

	if err := src.InjectCache(iCtx.KubeClient().GetCache()); err != nil {
		logger.Errorf(ctx, "failed to set informers for ObjectType %s", src.String())
		return nil, err
	}

	metricsScope := iCtx.MetricsScope().NewSubScope(entry.ID)
	updateCount := labeled.NewCounter("informer_update", "Update events from informer", metricsScope)
	droppedUpdateCount := labeled.NewCounter("informer_update_dropped", "Update events from informer that have the same resource version", metricsScope)
	genericCount := labeled.NewCounter("informer_generic", "Generic events from informer", metricsScope)

	enqueueOwner := iCtx.EnqueueOwner()
	err := src.Start(
		// Handlers
		handler.Funcs{
			CreateFunc: func(evt event.CreateEvent, q2 workqueue.RateLimitingInterface) {
				logger.Debugf(context.Background(), "Create received for %s, ignoring.", evt.Meta.GetName())
			},
			UpdateFunc: func(evt event.UpdateEvent, q2 workqueue.RateLimitingInterface) {
				if evt.MetaNew == nil {
					logger.Warn(context.Background(), "Received an Update event with nil MetaNew.")
				} else if evt.MetaOld == nil || evt.MetaOld.GetResourceVersion() != evt.MetaNew.GetResourceVersion() {
					newCtx := contextutils.WithNamespace(context.Background(), evt.MetaNew.GetNamespace())
					logger.Debugf(ctx, "Enqueueing owner for updated object [%v/%v]", evt.MetaNew.GetNamespace(), evt.MetaNew.GetName())
					if err := enqueueOwner(k8stypes.NamespacedName{Name: evt.MetaNew.GetName(), Namespace: evt.MetaNew.GetNamespace()}); err != nil {
						logger.Warnf(context.Background(), "Failed to handle Update event for object [%v]", evt.MetaNew.GetName())
					}
					updateCount.Inc(newCtx)
				} else {
					newCtx := contextutils.WithNamespace(context.Background(), evt.MetaNew.GetNamespace())
					droppedUpdateCount.Inc(newCtx)
				}
			},
			DeleteFunc: func(evt event.DeleteEvent, q2 workqueue.RateLimitingInterface) {
				logger.Debugf(context.Background(), "Delete received for %s, ignoring.", evt.Meta.GetName())
			},
			GenericFunc: func(evt event.GenericEvent, q2 workqueue.RateLimitingInterface) {
				logger.Debugf(context.Background(), "Generic received for %s, ignoring.", evt.Meta.GetName())
				genericCount.Inc(ctx)
			},
		},
		// Queue
		// TODO: a more unique workqueue name
		workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(),
			entry.ResourceToWatch.GetObjectKind().GroupVersionKind().Kind),
		// Predicates
		predicate.Funcs{
			CreateFunc: func(createEvent event.CreateEvent) bool {
				return false
			},
			UpdateFunc: func(updateEvent event.UpdateEvent) bool {
				// TODO we should filter out events in case there are no updates observed between the old and new?
				return workflowParentPredicate(updateEvent.MetaNew)
			},
			DeleteFunc: func(deleteEvent event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(genericEvent event.GenericEvent) bool {
				return workflowParentPredicate(genericEvent.Meta)
			},
		})

	if err != nil {
		return nil, err
	}

	return &PluginManager{
		id:              entry.ID,
		plugin:          entry.Plugin,
		resourceToWatch: entry.ResourceToWatch,
		metrics:         newPluginMetrics(metricsScope),
		kubeClient:      iCtx.KubeClient(),
	}, nil
}

func init() {
	labeled.SetMetricKeys(contextutils.ProjectKey, contextutils.DomainKey, contextutils.WorkflowIDKey, contextutils.TaskIDKey)
}
