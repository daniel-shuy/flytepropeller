package workflowstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/flyteorg/flytepropeller/pkg/apis/flyteworkflow/v1alpha1"

	"github.com/flyteorg/flytestdlib/fastcheck"
	"github.com/flyteorg/flytestdlib/promutils"
	"github.com/flyteorg/flytestdlib/promutils/labeled"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TODO - optimization maybe? we can move this to predicate check, before we add it to the queue?
type resourceVersionMetrics struct {
	workflowStaleCount            labeled.Counter
	workflowEvictedCount          labeled.Counter
	workflowRedundantUpdatesCount labeled.Counter
}

// Simple function that covnerts the namespace and name to a string
func resourceVersionKey(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

// A specialized store that stores a inmemory cache of all the workflows that are currently executing and their last observed version numbers
// If the version numbers between the last update and the next Get have not been updated then the Get returns a nil (ignores the workflow)
// Propeller round will then just ignore the workflow
type resourceVersionCaching struct {
	w                               FlyteWorkflow
	metrics                         *resourceVersionMetrics
	lastUpdatedResourceVersionCache sync.Map
	terminatedFilter                fastcheck.Filter
}

func (r *resourceVersionCaching) updateRevisionCache(ctx context.Context, namespace, name, resourceVersion string, isTerminated bool) {
	if isTerminated {
		r.metrics.workflowEvictedCount.Inc(ctx)
		r.lastUpdatedResourceVersionCache.Delete(resourceVersionKey(namespace, name))
	} else {
		r.lastUpdatedResourceVersionCache.Store(resourceVersionKey(namespace, name), resourceVersion)
	}
}

func (r *resourceVersionCaching) isResourceVersionSameAsPrevious(ctx context.Context, namespace, name, resourceVersion string) bool {
	if v, ok := r.lastUpdatedResourceVersionCache.Load(resourceVersionKey(namespace, name)); ok {
		strV := v.(string)
		if strV == resourceVersion {
			r.metrics.workflowStaleCount.Inc(ctx)
			return true
		}
	}

	return false
}

func (r *resourceVersionCaching) Get(ctx context.Context, namespace, name string) (*v1alpha1.FlyteWorkflow, error) {
	// Check if the resource version key has already been stored in a terminal phase. Processing
	// terminated FlyteWorkflows can occur when workflow updates are reported after a workflow
	// has already completed.
	if r.terminatedFilter.Contains(ctx, []byte(resourceVersionKey(namespace, name))) {
		return nil, ErrWorkflowTerminated
	}

	w, err := r.w.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	if w != nil {
		if r.isResourceVersionSameAsPrevious(ctx, namespace, name, w.ResourceVersion) {
			return nil, ErrStaleWorkflowError
		}
	}

	return w, nil
}

func (r *resourceVersionCaching) UpdateStatus(ctx context.Context, workflow *v1alpha1.FlyteWorkflow, priorityClass PriorityClass) (
	newWF *v1alpha1.FlyteWorkflow, err error) {
	newWF, err = r.w.UpdateStatus(ctx, workflow, priorityClass)
	if err != nil {
		return nil, err
	}

	if newWF != nil {
		// If the update succeeded AND a resource version has changed (indicating the new WF was actually changed),
		// cache the old.  The behavior this code is trying to accomplish is this.  Normally, if the CRD has not changed,
		// the code will look at the workflow at the normal frequency.  As soon as something has changed, and we get
		// confirmation that we have written the newer workflow to the api server, and receive a different ResourceVersion,
		// we cache the old ResourceVersion number.  This means that we will never process that exact version again
		// (as long as the cache is up) thus saving us from things like sending duplicate events.
		if newWF.ResourceVersion != workflow.ResourceVersion {
			r.updateRevisionCache(ctx, workflow.Namespace, workflow.Name, workflow.ResourceVersion, workflow.Status.IsTerminated())
		} else {
			r.metrics.workflowRedundantUpdatesCount.Inc(ctx)
		}

		if newWF.GetExecutionStatus().IsTerminated() {
			r.terminatedFilter.Add(ctx, []byte(resourceVersionKey(workflow.Namespace, workflow.Name)))
		}
	}

	return newWF, nil
}

func (r *resourceVersionCaching) Update(ctx context.Context, workflow *v1alpha1.FlyteWorkflow, priorityClass PriorityClass) (
	newWF *v1alpha1.FlyteWorkflow, err error) {
	// If the workflow has any managed fields setting the array to one empty ManagedField clears them in the CRD.
	// FlyteWorkflow CRDs are only managed by a single FlytePropeller instance and therefore the managed fields paradigm
	// does not add useful functionality. Clearing them reduces CRD size, improving etcd I/O performance.
	if len(workflow.ObjectMeta.ManagedFields) > 0 {
		workflow.ObjectMeta.ManagedFields = workflow.ObjectMeta.ManagedFields[:1]
		workflow.ObjectMeta.ManagedFields[0] = metav1.ManagedFieldsEntry{}
	}

	newWF, err = r.w.Update(ctx, workflow, priorityClass)
	if err != nil {
		return nil, err
	}

	if newWF != nil {
		// If the update succeeded AND a resource version has changed (indicating the new WF was actually changed),
		// cache the old
		if newWF.ResourceVersion != workflow.ResourceVersion {
			r.updateRevisionCache(ctx, workflow.Namespace, workflow.Name, workflow.ResourceVersion, workflow.Status.IsTerminated())
		} else {
			r.metrics.workflowRedundantUpdatesCount.Inc(ctx)
		}

		if newWF.GetExecutionStatus().IsTerminated() {
			r.terminatedFilter.Add(ctx, []byte(resourceVersionKey(workflow.Namespace, workflow.Name)))
		}
	}

	return newWF, nil
}

func NewResourceVersionCachingStore(_ context.Context, scope promutils.Scope, workflowStore FlyteWorkflow) (FlyteWorkflow, error) {
	filter, err := fastcheck.NewLRUCacheFilter(1000, scope.NewSubScope("terminated_filter"))
	if err != nil {
		return nil, err
	}

	return &resourceVersionCaching{
		w: workflowStore,
		metrics: &resourceVersionMetrics{
			workflowStaleCount:            labeled.NewCounter("wf_stale", "Found stale workflow in cache", scope, labeled.EmitUnlabeledMetric),
			workflowEvictedCount:          labeled.NewCounter("wf_evict", "Removed workflow from resource version cache", scope, labeled.EmitUnlabeledMetric),
			workflowRedundantUpdatesCount: labeled.NewCounter("wf_redundant", "Workflow Update called but ectd. detected no actual update to the workflow.", scope, labeled.EmitUnlabeledMetric),
		},
		lastUpdatedResourceVersionCache: sync.Map{},
		terminatedFilter:                filter,
	}, nil
}
