package bundlec

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	ctrlLogz "github.com/atlassian/ctrl/logz"
	smith_v1 "github.com/atlassian/smith/pkg/apis/smith/v1"
	smithClient_v1 "github.com/atlassian/smith/pkg/client/clientset_generated/clientset/typed/smith/v1"
	"github.com/atlassian/smith/pkg/plugin"
	"github.com/atlassian/smith/pkg/resources"
	"github.com/atlassian/smith/pkg/store"
	"github.com/atlassian/smith/pkg/util/graph"
	"github.com/atlassian/smith/pkg/util/logz"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type bundleSyncTask struct {

	// Inputs

	logger           *zap.Logger
	bundleClient     smithClient_v1.BundlesGetter
	smartClient      SmartClient
	rc               ReadyChecker
	store            Store
	specCheck        SpecCheck
	bundle           *smith_v1.Bundle
	pluginContainers map[smith_v1.PluginName]plugin.PluginContainer
	scheme           *runtime.Scheme
	catalog          *store.Catalog

	// Outputs

	processedResources map[smith_v1.ResourceName]*resourceInfo
	objectsToDelete    map[objectRef]runtime.Object
	newFinalizers      []string
}

// Parse bundle, build resource graph, traverse graph, assert each resource exists.
// For each resource ensure its dependencies (if any) are in READY state before creating it.
// If at least one dependency is not READY - skip the resource. Rebuild will/should be called once the dependency
// updates it's state (noticed via watching).

// READY state might mean something different for each resource type. For a Custom Resource it may mean
// that a field "State" in the Status of the resource is set to "Ready". It is customizable via
// annotations with some defaults.
func (st *bundleSyncTask) processNormal() (retriableError bool, e error) {
	// If the "deleteResources" finalizer is missing, add it and finish the processing iteration
	if !hasDeleteResourcesFinalizer(st.bundle) {
		st.newFinalizers = addDeleteResourcesFinalizer(st.bundle.GetFinalizers())
		return false, nil
	}

	// Build resource map by name
	resourceMap := make(map[smith_v1.ResourceName]smith_v1.Resource, len(st.bundle.Spec.Resources))
	for _, res := range st.bundle.Spec.Resources {
		if _, exist := resourceMap[res.Name]; exist {
			return false, errors.Errorf("bundle contains two resources with the same name %q", res.Name)
		}
		resourceMap[res.Name] = res
	}

	// Build the graph and topologically sort it
	_, sorted, sortErr := sortBundle(st.bundle)
	if sortErr != nil {
		return false, errors.Wrap(sortErr, "topological sort of resources failed")
	}

	st.processedResources = make(map[smith_v1.ResourceName]*resourceInfo, len(st.bundle.Spec.Resources))

	// Visit vertices in sorted order
	for _, resName := range sorted {
		// Process the resource
		resourceName := resName.(smith_v1.ResourceName)
		logger := st.logger.With(logz.Resource(resourceName))
		res := resourceMap[resourceName]
		rst := resourceSyncTask{
			logger:             logger,
			smartClient:        st.smartClient,
			rc:                 st.rc,
			store:              st.store,
			specCheck:          st.specCheck,
			bundle:             st.bundle,
			processedResources: st.processedResources,
			pluginContainers:   st.pluginContainers,
			scheme:             st.scheme,
			catalog:            st.catalog,
		}
		resInfo := rst.processResource(&res)
		retriable, resErr := resInfo.fetchError()
		if resErr != nil {
			if api_errors.IsConflict(errors.Cause(resErr)) {
				// Short circuit on conflict
				return retriable, resErr
			}
			logger.Error("Done processing resource", zap.Bool("ready", resInfo.isReady()), zap.Error(resErr))
		} else {
			logger.Info("Done processing resource", zap.Bool("ready", resInfo.isReady()))
		}
		st.processedResources[resourceName] = &resInfo
	}
	err := st.findObjectsToDelete()
	if err != nil {
		return false, err
	}
	if st.isBundleReady() {
		// Delete objects which were removed from the bundle
		retriable, err := st.deleteRemovedResources()
		if err != nil {
			return retriable, err
		}
	}

	return false, nil
}

// Process the bundle marked with DeletionTimestamp
// TODO: remove this method after https://github.com/kubernetes/kubernetes/issues/59850 is fixed
func (st *bundleSyncTask) processDeleted() (retriableError bool, e error) {
	if hasDeleteResourcesFinalizer(st.bundle) {
		if !resources.HasFinalizer(st.bundle, meta_v1.FinalizerDeleteDependents) {
			// If "foregroundDeletion" finalizer was not set, perform manual cascade deletion
			retrieable, err := st.deleteAllResources()
			if err != nil {
				return retrieable, err
			}
		}

		// If the "foregroundDeletion" finalizer is set, or the manual deletion
		// of resources has succeeded, remove the "deleteResources" finalizer
		st.newFinalizers = removeDeleteResourcesFinalizer(st.bundle.GetFinalizers())
	}
	return false, nil
}

func (st *bundleSyncTask) deleteAllResources() (retriableError bool, e error) {
	objs, err := st.store.ObjectsControlledBy(st.bundle.Namespace, st.bundle.UID)
	if err != nil {
		return false, err
	}
	st.objectsToDelete = make(map[objectRef]runtime.Object, len(objs))

	var firstErr error
	retriable := true
	policy := meta_v1.DeletePropagationForeground
	for _, obj := range objs {
		m := obj.(meta_v1.Object)
		gvk := obj.GetObjectKind().GroupVersionKind()
		name := m.GetName()
		ref := objectRef{
			GroupVersionKind: gvk,
			Name:             name,
		}
		st.objectsToDelete[ref] = obj

		logger := st.logger.With(ctrlLogz.ObjectGk(gvk.GroupKind()), ctrlLogz.ObjectName(name))
		if m.GetDeletionTimestamp() != nil {
			logger.Debug("Object is marked for deletion already")
			continue
		}
		uid := m.GetUID()

		logger.Info("Deleting object")
		resClient, err := st.smartClient.ForGVK(gvk, st.bundle.Namespace)
		if err != nil {
			if firstErr == nil {
				retriable = false
				firstErr = err
			} else {
				logger.Error("Failed to get client for object", zap.Error(err))
			}
			continue
		}

		err = resClient.Delete(name, &meta_v1.DeleteOptions{
			Preconditions: &meta_v1.Preconditions{
				UID: &uid,
			},
			PropagationPolicy: &policy,
		})
		if err != nil && !api_errors.IsNotFound(err) && !api_errors.IsConflict(err) {
			// not found means object has been deleted already
			// conflict means it has been deleted and re-created (UID does not match)
			if firstErr == nil {
				firstErr = err
			} else {
				logger.Warn("Failed to delete object", zap.Error(err))
			}
			continue
		}
	}
	return retriable, firstErr
}

// findObjectsToDelete initializes objectsToDelete field with objects that have controller owner references to
// the Bundle being processed but are not defined in it.
func (st *bundleSyncTask) findObjectsToDelete() error {
	objs, err := st.store.ObjectsControlledBy(st.bundle.Namespace, st.bundle.UID)
	if err != nil {
		return err
	}
	st.objectsToDelete = make(map[objectRef]runtime.Object, len(objs))
	for _, obj := range objs {
		m := obj.(meta_v1.Object)
		ref := objectRef{
			GroupVersionKind: obj.GetObjectKind().GroupVersionKind(),
			Name:             m.GetName(),
		}
		st.objectsToDelete[ref] = obj
	}
	for _, res := range st.bundle.Spec.Resources {
		var gvk schema.GroupVersionKind
		var name string
		if res.Spec.Object != nil {
			gvk = res.Spec.Object.GetObjectKind().GroupVersionKind()
			name = res.Spec.Object.(meta_v1.Object).GetName()
		} else if res.Spec.Plugin != nil {
			gvk = st.pluginContainers[res.Spec.Plugin.Name].Plugin.Describe().GVK
			name = res.Spec.Plugin.ObjectName
		} else {
			// neither "object" nor "plugin" field is specified. This shouldn't really happen (schema), but we
			// ignore the error and continue collecting objects. Even if not caught by the schema, this error
			// must have been reported earlier while processing this resource.
			continue
		}
		delete(st.objectsToDelete, objectRef{
			GroupVersionKind: gvk,
			Name:             name,
		})
	}
	return nil
}

func (st *bundleSyncTask) deleteRemovedResources() (retriableError bool, e error) {
	var firstErr error
	retriable := true
	policy := meta_v1.DeletePropagationForeground
	for ref, obj := range st.objectsToDelete {
		logger := st.logger.With(ctrlLogz.ObjectGk(ref.GroupVersionKind.GroupKind()), ctrlLogz.ObjectName(ref.Name))
		m := obj.(meta_v1.Object)
		if m.GetDeletionTimestamp() != nil {
			logger.Debug("Object is marked for deletion already")
			continue
		}
		logger.Info("Deleting object")
		resClient, err := st.smartClient.ForGVK(ref.GroupVersionKind, st.bundle.Namespace)
		if err != nil {
			if firstErr == nil {
				retriable = false
				firstErr = err
			} else {
				logger.Error("Failed to get client for object", zap.Error(err))
			}
			continue
		}

		uid := m.GetUID()
		err = resClient.Delete(ref.Name, &meta_v1.DeleteOptions{
			Preconditions: &meta_v1.Preconditions{
				UID: &uid,
			},
			PropagationPolicy: &policy,
		})
		if err != nil && !api_errors.IsNotFound(err) && !api_errors.IsConflict(err) {
			// not found means object has been deleted already
			// conflict means it has been deleted and re-created (UID does not match)
			if firstErr == nil {
				firstErr = err
			} else {
				logger.Warn("Failed to delete object", zap.Error(err))
			}
			continue
		}
	}
	return retriable, firstErr
}

func (st *bundleSyncTask) updateBundle() error {
	bundleUpdated, err := st.bundleClient.Bundles(st.bundle.Namespace).Update(st.bundle)
	if err != nil {
		return errors.Wrap(err, "failed to update bundle")
	}
	st.logger.Sugar().Debugf("Set bundle status to %s", &bundleUpdated.Status)
	st.logger.Sugar().Debugf("Set bundle finalizers to %s", bundleUpdated.Finalizers)
	return nil
}

func (st *bundleSyncTask) handleProcessResult(retriable bool, processErr error) (bool /*retriable*/, error) {
	if processErr != nil && api_errors.IsConflict(errors.Cause(processErr)) {
		return retriable, processErr
	}
	if processErr == context.Canceled || processErr == context.DeadlineExceeded {
		return false, processErr
	}

	bundleUpdated := false

	if st.newFinalizers != nil {
		// Update finalizers
		st.bundle.Finalizers = st.newFinalizers
		// Set status to "InProgress: True"
		// (there will be one more iteration for resource sync that will set an appropriate status)
		// TODO: Setting status to "InProgress" here might be unnecessary
		// (if the Bundle spec wasn't updated during the last update, the status should be kept with no changes)
		// The better solution could be storing "ObservedGeneration" in the status to reflect
		// whether the status is up-to-date with the spec or stale
		st.bundle.Status.Conditions = []smith_v1.BundleCondition{
			{Type: smith_v1.BundleInProgress, Status: smith_v1.ConditionTrue},
			{Type: smith_v1.BundleReady, Status: smith_v1.ConditionFalse},
			{Type: smith_v1.BundleError, Status: smith_v1.ConditionFalse},
		}
		_, err := st.updateObjectsToDeleteStatus()
		if err != nil {
			// Just log the error and continue. Bundle will be reprocessed anyway.
			st.logger.Error("Error updating ObjectsToDelete status field", zap.Error(err))
		}
		bundleUpdated = true
	} else if st.bundle.DeletionTimestamp == nil {
		// Construct resource conditions and check if there were any resource errors
		resourceStatuses := make([]smith_v1.ResourceStatus, 0, len(st.processedResources))
		var failedResources []smith_v1.ResourceName
		retriableResourceErr := true
		for _, res := range st.bundle.Spec.Resources { // Deterministic iteration order
			blockedCond, inProgressCond, readyCond, errorCond := st.resourceConditions(res)

			if errorCond.Status == smith_v1.ConditionTrue {
				failedResources = append(failedResources, res.Name)
				retriableResourceErr = retriableResourceErr && errorCond.Reason == smith_v1.ResourceReasonRetriableError // Must not continue if at least one error is not retriable
			}

			bundleUpdated = updateResourceCondition(st.bundle, res.Name, &blockedCond) || bundleUpdated
			bundleUpdated = updateResourceCondition(st.bundle, res.Name, &inProgressCond) || bundleUpdated
			bundleUpdated = updateResourceCondition(st.bundle, res.Name, &readyCond) || bundleUpdated
			bundleUpdated = updateResourceCondition(st.bundle, res.Name, &errorCond) || bundleUpdated
			resourceStatuses = append(resourceStatuses, smith_v1.ResourceStatus{
				Name:       res.Name,
				Conditions: []smith_v1.ResourceCondition{blockedCond, inProgressCond, readyCond, errorCond},
			})
		}

		if processErr == nil && len(failedResources) > 0 {
			processErr = errors.Errorf("error processing resource(s): %q", failedResources)
			retriable = retriableResourceErr
		}

		// Bundle conditions
		inProgressCond := smith_v1.BundleCondition{Type: smith_v1.BundleInProgress, Status: smith_v1.ConditionFalse}
		readyCond := smith_v1.BundleCondition{Type: smith_v1.BundleReady, Status: smith_v1.ConditionFalse}
		errorCond := smith_v1.BundleCondition{Type: smith_v1.BundleError, Status: smith_v1.ConditionFalse}

		if processErr == nil {
			if st.isBundleReady() {
				readyCond.Status = smith_v1.ConditionTrue
			} else {
				inProgressCond.Status = smith_v1.ConditionTrue
			}
		} else {
			errorCond.Status = smith_v1.ConditionTrue
			errorCond.Message = processErr.Error()
			if retriable {
				errorCond.Reason = smith_v1.BundleReasonRetriableError
				inProgressCond.Status = smith_v1.ConditionTrue
			} else {
				errorCond.Reason = smith_v1.BundleReasonTerminalError
			}
		}

		bundleUpdated = updateBundleCondition(st.bundle, &inProgressCond) || bundleUpdated
		bundleUpdated = updateBundleCondition(st.bundle, &readyCond) || bundleUpdated
		bundleUpdated = updateBundleCondition(st.bundle, &errorCond) || bundleUpdated

		// Plugin statuses
		pluginStatuses := st.pluginStatuses()
		bundleUpdated = bundleUpdated || !reflect.DeepEqual(st.bundle.Status.PluginStatuses, pluginStatuses)
		st.bundle.Status.PluginStatuses = pluginStatuses

		// Update the bundle status
		if bundleUpdated {
			st.bundle.Status.ResourceStatuses = resourceStatuses
			st.bundle.Status.Conditions = []smith_v1.BundleCondition{inProgressCond, readyCond, errorCond}
		}

		obj2deleteUpdated, err := st.updateObjectsToDeleteStatus()
		if err != nil {
			// Just log the error and continue
			st.logger.Error("Error updating ObjectsToDelete status field", zap.Error(err))
		} else {
			bundleUpdated = obj2deleteUpdated || bundleUpdated
		}
	}

	if bundleUpdated {
		ex := st.updateBundle()
		if processErr == nil {
			processErr = ex
			retriable = true
		}
	}

	return retriable, processErr
}

func (st *bundleSyncTask) updateObjectsToDeleteStatus() (bool /* bundleUpdated */, error) {
	if st.objectsToDelete == nil {
		err := st.findObjectsToDelete()
		if err != nil {
			return false, err
		}
	}
	newToDelete := make([]smith_v1.ObjectToDelete, 0, len(st.objectsToDelete))
	for ref := range st.objectsToDelete {
		newToDelete = append(newToDelete, smith_v1.ObjectToDelete{
			Group:   ref.Group,
			Version: ref.Version,
			Kind:    ref.Kind,
			Name:    ref.Name,
		})
	}
	// Sort them to ensure map iteration order and the order of informers we got the date from does not influence the result.
	sort.Slice(newToDelete, func(i, j int) bool {
		a := newToDelete[i]
		b := newToDelete[j]
		if a.Group < b.Group {
			return true
		}
		if a.Group > b.Group {
			return false
		}
		if a.Version < b.Version {
			return true
		}
		if a.Version > b.Version {
			return false
		}
		if a.Kind < b.Kind {
			return true
		}
		if a.Kind > b.Kind {
			return false
		}
		if a.Name < b.Name {
			return true
		}
		if a.Name > b.Name {
			return false
		}
		// Should be unreachable because data is coming from map keys
		return false
	})
	if !reflect.DeepEqual(st.bundle.Status.ObjectsToDelete, newToDelete) {
		st.bundle.Status.ObjectsToDelete = newToDelete
		return true, nil
	}
	return false, nil
}

func (st *bundleSyncTask) isBundleReady() bool {
	for _, res := range st.bundle.Spec.Resources {
		res := st.processedResources[res.Name]
		if res == nil || !res.isReady() {
			return false
		}
	}
	return true
}

type objectRef struct {
	schema.GroupVersionKind
	Name string
}

// pluginStatuses visits each valid Plugin just once, collecting its PluginStatus.
func (st *bundleSyncTask) pluginStatuses() []smith_v1.PluginStatus {
	// Plugin statuses
	name2status := make(map[smith_v1.PluginName]struct{})
	// most likely will be of the same size as before
	pluginStatuses := make([]smith_v1.PluginStatus, 0, len(st.bundle.Status.PluginStatuses))
	for _, res := range st.bundle.Spec.Resources { // Deterministic iteration order
		if res.Spec.Plugin == nil {
			continue // Not a plugin
		}
		pluginName := res.Spec.Plugin.Name
		if _, ok := name2status[pluginName]; ok {
			continue // Already reported
		}
		name2status[pluginName] = struct{}{}
		var pluginStatus smith_v1.PluginStatus
		pluginContainer, ok := st.pluginContainers[pluginName]
		if ok {
			describe := pluginContainer.Plugin.Describe()
			pluginStatus = smith_v1.PluginStatus{
				Name:    pluginName,
				Group:   describe.GVK.Group,
				Version: describe.GVK.Version,
				Kind:    describe.GVK.Kind,
				Status:  smith_v1.PluginStatusOk,
			}
		} else {
			pluginStatus = smith_v1.PluginStatus{
				Name:   pluginName,
				Status: smith_v1.PluginStatusNoSuchPlugin,
			}
		}
		pluginStatuses = append(pluginStatuses, pluginStatus)
	}
	return pluginStatuses
}

// resourceConditions calculates conditions for a given Resource,
// which can be useful when determining whether to retry or not.
func (st *bundleSyncTask) resourceConditions(res smith_v1.Resource) (
	smith_v1.ResourceCondition, /* blockedCond */
	smith_v1.ResourceCondition, /* inProgressCond */
	smith_v1.ResourceCondition, /* readyCond */
	smith_v1.ResourceCondition, /* errorCond */
) {
	blockedCond := smith_v1.ResourceCondition{Type: smith_v1.ResourceBlocked, Status: smith_v1.ConditionFalse}
	inProgressCond := smith_v1.ResourceCondition{Type: smith_v1.ResourceInProgress, Status: smith_v1.ConditionFalse}
	readyCond := smith_v1.ResourceCondition{Type: smith_v1.ResourceReady, Status: smith_v1.ConditionFalse}
	errorCond := smith_v1.ResourceCondition{Type: smith_v1.ResourceError, Status: smith_v1.ConditionFalse}

	if resInfo, ok := st.processedResources[res.Name]; ok {
		// Resource was processed
		switch resStatus := resInfo.status.(type) {
		case resourceStatusDependenciesNotReady:
			blockedCond.Status = smith_v1.ConditionTrue
			blockedCond.Reason = smith_v1.ResourceReasonDependenciesNotReady
			blockedCond.Message = fmt.Sprintf("Not ready: %q", resStatus.dependencies)
		case resourceStatusInProgress:
			inProgressCond.Status = smith_v1.ConditionTrue
		case resourceStatusReady:
			readyCond.Status = smith_v1.ConditionTrue
		case resourceStatusError:
			errorCond.Status = smith_v1.ConditionTrue
			errorCond.Message = resStatus.err.Error()
			if resStatus.isRetriableError {
				errorCond.Reason = smith_v1.ResourceReasonRetriableError
				inProgressCond.Status = smith_v1.ConditionTrue
			} else {
				errorCond.Reason = smith_v1.ResourceReasonTerminalError
			}
		default:
			blockedCond.Status = smith_v1.ConditionUnknown
			inProgressCond.Status = smith_v1.ConditionUnknown
			readyCond.Status = smith_v1.ConditionUnknown
			errorCond.Status = smith_v1.ConditionTrue
			errorCond.Reason = smith_v1.ResourceReasonTerminalError
			errorCond.Message = fmt.Sprintf("internal error - unknown resource status type %T", resInfo.status)
		}
	} else {
		// Resource was not processed
		blockedCond.Status = smith_v1.ConditionUnknown
		inProgressCond.Status = smith_v1.ConditionUnknown
		readyCond.Status = smith_v1.ConditionUnknown
		errorCond.Status = smith_v1.ConditionUnknown
	}

	return blockedCond, inProgressCond, readyCond, errorCond
}

// updateBundleCondition updates passed condition by fetching information from an existing resource condition if present.
// Sets LastTransitionTime to now if the status has changed.
// Returns true if resource condition in the bundle does not match and needs to be updated.
func updateBundleCondition(b *smith_v1.Bundle, condition *smith_v1.BundleCondition) bool {
	now := meta_v1.Now()
	condition.LastTransitionTime = now

	// Try to find resource condition
	_, oldCondition := b.GetCondition(condition.Type)

	if oldCondition == nil {
		// New resource condition
		return true
	}

	// We are updating an existing condition, so we need to check if it has changed.
	if condition.Status == oldCondition.Status {
		condition.LastTransitionTime = oldCondition.LastTransitionTime
	}

	isEqual := condition.Status == oldCondition.Status &&
		condition.Reason == oldCondition.Reason &&
		condition.Message == oldCondition.Message &&
		condition.LastTransitionTime.Equal(&oldCondition.LastTransitionTime)

	if !isEqual {
		condition.LastUpdateTime = now
	}

	// Return true if one of the fields have changed.
	return !isEqual
}

// updateResourceCondition updates passed condition by fetching information from an existing resource condition if present.
// Sets LastTransitionTime to now if the status has changed.
// Returns true if resource condition in the bundle does not match and needs to be updated.
func updateResourceCondition(b *smith_v1.Bundle, resName smith_v1.ResourceName, condition *smith_v1.ResourceCondition) bool {
	now := meta_v1.Now()
	condition.LastTransitionTime = now
	// Try to find this resource status
	_, status := b.Status.GetResourceStatus(resName)

	if status == nil {
		// No status for this resource, hence it's a new resource condition
		return true
	}

	// Try to find resource condition
	_, oldCondition := status.GetCondition(condition.Type)

	if oldCondition == nil {
		// New resource condition
		return true
	}

	// We are updating an existing condition, so we need to check if it has changed.
	if condition.Status == oldCondition.Status {
		condition.LastTransitionTime = oldCondition.LastTransitionTime
	}

	isEqual := condition.Status == oldCondition.Status &&
		condition.Reason == oldCondition.Reason &&
		condition.Message == oldCondition.Message &&
		condition.LastTransitionTime.Equal(&oldCondition.LastTransitionTime)

	if !isEqual {
		condition.LastUpdateTime = now
	}

	// Return true if one of the fields have changed.
	return !isEqual
}

func sortBundle(bundle *smith_v1.Bundle) (*graph.Graph, []graph.V, error) {
	g := graph.NewGraph(len(bundle.Spec.Resources))

	for _, res := range bundle.Spec.Resources {
		g.AddVertex(graph.V(res.Name), nil)
	}

	for _, res := range bundle.Spec.Resources {
		for _, reference := range res.References {
			if err := g.AddEdge(res.Name, reference.Resource); err != nil {
				return nil, nil, err
			}
		}
	}

	sorted, err := g.TopologicalSort()
	if err != nil {
		return nil, nil, err
	}

	return g, sorted, nil
}
