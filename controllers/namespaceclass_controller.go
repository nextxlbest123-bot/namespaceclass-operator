package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	akuityv1 "github.com/lixu/namespaceclass-operator/api/v1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	metrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	NamespaceClassLabel     = "namespaceclass.akuity.io/name"
	ManagedByLabel          = "namespaceclass.akuity.io/managed-by"
	SourceClassLabel        = "namespaceclass.akuity.io/source-class"
	InventoryAnnotation     = "namespaceclass.akuity.io/inventory"
	AttachedClassAnnotation = "namespaceclass.akuity.io/attached-class"
	ControllerName          = "namespace-class-controller"
	NamespaceClassFinalizer = "namespaceclass.core.akuity.io/finalizer"
)

// Metrics for NamespaceClass controller
var (
	appliedResourcesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "namespaceclass_applied_resources_total",
			Help: "Total number of resources applied by namespaceclass controller",
		},
		[]string{"namespace", "class", "kind"},
	)
	prunedResourcesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "namespaceclass_pruned_resources_total",
			Help: "Total number of resources pruned by namespaceclass controller",
		},
		[]string{"namespace", "class", "kind"},
	)
	reconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "namespaceclass_reconcile_errors_total",
			Help: "Total reconcile errors",
		},
		[]string{"namespace", "phase"},
	)
	reconcileDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "namespaceclass_reconcile_duration_seconds",
			Help:    "Duration of reconcile loops",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"namespace", "class"},
	)
)

func init() {
	metrics.Registry.MustRegister(appliedResourcesTotal, prunedResourcesTotal, reconcileErrorsTotal, reconcileDurationSeconds)
}

type NamespaceReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	Recorder                record.EventRecorder
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=akuity.io,resources=namespaceclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=*,resources=*,verbs=*

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	start := time.Now()
	className := ns.Labels[NamespaceClassLabel]
	defer func() {
		reconcileDurationSeconds.WithLabelValues(ns.Name, className).Observe(time.Since(start).Seconds())
	}()

	// Check if Namespace is being deleted
	if !ns.DeletionTimestamp.IsZero() {
		// Kubernetes Garbage Collector will clean up resources
		// since we set OwnerReference to Namespace in applyClassResources
		return ctrl.Result{}, nil
	}

	if className == "" {
		// Case: Label missing/removed
		// Check for existing Inventory annotation to determine if cleanup is needed
		if ann := ns.GetAnnotations(); ann != nil && ann[AttachedClassAnnotation] != "" {
			prevClass := ann[AttachedClassAnnotation]
			logger.Info("Class label removed, cleaning up resources", "previousClass", prevClass)
			if err := r.cleanUpResources(ctx, &ns, prevClass); err != nil {
				logger.Error(err, "failed to cleanup resources")
				reconcileErrorsTotal.WithLabelValues(ns.Name, "cleanup").Inc()
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Get NamespaceClass definition
	var nsClass akuityv1.NamespaceClass
	if err := r.Get(ctx, types.NamespacedName{Name: className}, &nsClass); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Referenced NamespaceClass not found", "class", className)
			r.Recorder.Eventf(&ns, corev1.EventTypeWarning, "ClassMissing", "NamespaceClass %s not found", className)
			reconcileErrorsTotal.WithLabelValues(ns.Name, "class-missing").Inc()
			return ctrl.Result{}, nil // No retry - wait for Class creation or label modification
		}
		return ctrl.Result{}, err
	}

	// Read old inventory
	oldInventory, err := r.getNamespaceInventory(ctx, &ns)
	if err != nil {
		reconcileErrorsTotal.WithLabelValues(ns.Name, "read-inventory").Inc()
		return ctrl.Result{}, err
	}

	// Apply resources
	appliedInventory, err := r.applyClassResources(ctx, &ns, &nsClass)
	if err != nil {
		logger.Error(err, "Failed to apply resources")
		reconcileErrorsTotal.WithLabelValues(ns.Name, "apply-resources").Inc()
		r.Recorder.Eventf(&ns, corev1.EventTypeWarning, "ApplyFailed", "Failed to apply resources: %v", err)
		return ctrl.Result{}, err
	}

	// Clean up orphaned resources
	if err := r.pruneOrphanedResources(ctx, ns.Name, oldInventory, appliedInventory, className); err != nil {
		reconcileErrorsTotal.WithLabelValues(ns.Name, "prune").Inc()
		return ctrl.Result{}, err
	}

	// Update inventory
	if err := r.setNamespaceInventory(ctx, &ns, className, appliedInventory); err != nil {
		reconcileErrorsTotal.WithLabelValues(ns.Name, "persist-inventory").Inc()
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled namespace", "class", className)
	return ctrl.Result{}, nil
}

// +kubebuilder:rbac:groups=akuity.io,resources=namespaceclasses,verbs=get;list;watch;update

type NamespaceClassReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	MaxConcurrentReconciles int
}

func (r *NamespaceClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var nsClass akuityv1.NamespaceClass
	if err := r.Get(ctx, req.NamespacedName, &nsClass); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle finalizer addition
	if nsClass.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&nsClass, NamespaceClassFinalizer) {
			controllerutil.AddFinalizer(&nsClass, NamespaceClassFinalizer)
			if err := r.Update(ctx, &nsClass); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("Added finalizer to NamespaceClass")
		}
		return ctrl.Result{}, nil
	}

	// Handle deletion logic
	if controllerutil.ContainsFinalizer(&nsClass, NamespaceClassFinalizer) {
		logger.Info("NamespaceClass is being deleted", "policy", nsClass.Spec.DeletionPolicy)

		// Default policy is Cascade
		policy := nsClass.Spec.DeletionPolicy
		if policy == "" {
			policy = akuityv1.DeletionPolicyCascade
		}

		if policy == akuityv1.DeletionPolicyCascade {
			// Find all Namespaces referencing this Class and remove the label
			// NamespaceReconciler will cleanUpResources
			var nsList corev1.NamespaceList
			if err := r.List(ctx, &nsList, client.MatchingLabels{NamespaceClassLabel: nsClass.Name}); err != nil {
				return ctrl.Result{}, err
			}

			for _, ns := range nsList.Items {
				// Remove label
				patch := client.MergeFrom(ns.DeepCopy())
				delete(ns.Labels, NamespaceClassLabel)
				if err := r.Patch(ctx, &ns, patch); err != nil {
					logger.Error(err, "Failed to remove label from namespace during cascade delete", "namespace", ns.Name)
					return ctrl.Result{}, err
				}
				logger.Info("Detached NamespaceClass from Namespace (Cascade)", "namespace", ns.Name)
			}
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(&nsClass, NamespaceClassFinalizer)
		if err := r.Update(ctx, &nsClass); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Removed finalizer and deleted NamespaceClass")
	}

	return ctrl.Result{}, nil
}

type inventoryItem struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

// applyClassResources applies resources defined in NamespaceClass to target Namespace using Server-Side Apply
func (r *NamespaceReconciler) applyClassResources(ctx context.Context, ns *corev1.Namespace, nsClass *akuityv1.NamespaceClass) ([]inventoryItem, error) {
	logger := log.FromContext(ctx)
	var inventory []inventoryItem

	for _, tmpl := range nsClass.Spec.Resources {
		// Deserialize resource template
		obj := &unstructured.Unstructured{}
		if tmpl.Template.Object != nil {
			u, ok := tmpl.Template.Object.(*unstructured.Unstructured)
			if ok {
				obj = u.DeepCopy() // Make a copy to avoid mutating original template
			} else {
				continue
			}
		} else {
			if err := json.Unmarshal(tmpl.Template.Raw, obj); err != nil {
				return nil, fmt.Errorf("failed to unmarshal resource template: %w", err)
			}
		}

		// Configure object metadata
		obj.SetNamespace(ns.Name)
		labels := obj.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[ManagedByLabel] = ControllerName
		labels[SourceClassLabel] = nsClass.Name
		obj.SetLabels(labels)

		// Set OwnerReference to Namespace for garbage collection
		ownerRef := metav1.OwnerReference{
			APIVersion:         "v1",
			Kind:               "Namespace",
			Name:               ns.Name,
			UID:                ns.UID,
			BlockOwnerDeletion: pointer.Bool(true),
			Controller:         pointer.Bool(true),
		}
		obj.SetOwnerReferences([]metav1.OwnerReference{ownerRef})

		// Server-Side Apply (SSA)
		// Use Patch instead of Create to update resources when Class changes
		// Force=true means controller takes precedence in case of field conflicts
		force := true
		patchOpts := &client.PatchOptions{
			FieldManager: ControllerName,
			Force:        &force,
		}

		if err := r.Patch(ctx, obj, client.Apply, patchOpts); err != nil {
			return nil, fmt.Errorf("failed to apply resource %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}

		logger.V(1).Info("Applied resource", "kind", obj.GetKind(), "name", obj.GetName())
		appliedResourcesTotal.WithLabelValues(ns.Name, nsClass.Name, obj.GetKind()).Inc()

		inventory = append(inventory, inventoryItem{
			APIVersion: obj.GetAPIVersion(),
			Kind:       obj.GetKind(),
			Name:       obj.GetName(),
			Namespace:  obj.GetNamespace(),
		})
	}

	return inventory, nil
}

// pruneOrphanedResources deletes resources that exist in old inventory but not in keep inventory
func (r *NamespaceReconciler) pruneOrphanedResources(ctx context.Context, namespace string, old []inventoryItem, keep []inventoryItem, class string) error {
	logger := log.FromContext(ctx)
	keepMap := make(map[string]bool)
	for _, k := range keep {
		key := fmt.Sprintf("%s|%s|%s|%s", k.APIVersion, k.Kind, k.Namespace, k.Name)
		keepMap[key] = true
	}

	for _, item := range old {
		key := fmt.Sprintf("%s|%s|%s|%s", item.APIVersion, item.Kind, item.Namespace, item.Name)
		if keepMap[key] {
			continue
		}

		// Build object to delete
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(item.APIVersion)
		u.SetKind(item.Kind)
		u.SetName(item.Name)
		u.SetNamespace(item.Namespace)

		logger.Info("Pruning orphaned resource", "kind", item.Kind, "name", item.Name)
		if err := r.Delete(ctx, u); err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
		}
		prunedResourcesTotal.WithLabelValues(item.Namespace, class, item.Kind).Inc()
	}
	return nil
}

// cleanUpResources removes all managed resources from Namespace and clears inventory annotations
func (r *NamespaceReconciler) cleanUpResources(ctx context.Context, ns *corev1.Namespace, classFilter string) error {
	old, err := r.getNamespaceInventory(ctx, ns)
	if err != nil {
		return err
	}
	// Set keep list to nil to delete all resources
	if err := r.pruneOrphanedResources(ctx, ns.Name, old, nil, classFilter); err != nil {
		return err
	}
	// Clear annotations
	return r.setNamespaceInventory(ctx, ns, "", nil)
}

// getNamespaceInventory retrieves resource inventory from Namespace annotations
func (r *NamespaceReconciler) getNamespaceInventory(ctx context.Context, ns *corev1.Namespace) ([]inventoryItem, error) {
	ann := ns.GetAnnotations()
	if ann == nil {
		return nil, nil
	}
	raw, ok := ann[InventoryAnnotation]
	if !ok || raw == "" {
		return nil, nil
	}
	var items []inventoryItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	return items, nil
}

// setNamespaceInventory updates Namespace annotations with current resource inventory
func (r *NamespaceReconciler) setNamespaceInventory(ctx context.Context, ns *corev1.Namespace, className string, items []inventoryItem) error {
	patch := &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: ns.Name,
		},
	}

	if items == nil || len(items) == 0 {
		// 【关键点】在 SSA 中，如果想删除某些 Key
		// 我们可以将 Annotations 设置为一个空 map 并在 Patch 选项中指定
		// 或者更简单地，使用这种方式让 SSA 知道我们要清空这两个 key
		patch.Annotations = map[string]string{
			InventoryAnnotation:     "", // 在某些配置下 SSA 可能会保留 key，
			AttachedClassAnnotation: "", // 建议使用下面的 Extract 模式或直接用策略
		}
		// 对于“删除”操作，如果你想彻底从元数据中抹除 Key，
		// 在 SSA 复杂场景下通常建议直接 Patch NULL，
		// 或者保留 RetryOnConflict 用于删除，Patch 用于更新。

		// 但最简单且符合你逻辑的写法是：
		patch.Annotations = nil // 配合特殊 Patch 选项
	} else {
		b, err := json.Marshal(items)
		if err != nil {
			return err
		}
		patch.Annotations = map[string]string{
			InventoryAnnotation:     string(b),
			AttachedClassAnnotation: className,
		}
	}

	patchOpts := &client.PatchOptions{
		FieldManager: ControllerName,
	}
	//aligned with controller
	force := true
	patchOpts.Force = &force

	return r.Patch(ctx, patch, client.Apply, patchOpts, client.ForceOwnership)
}

// findNamespacesForClass returns reconcile requests for all Namespaces referencing a specific NamespaceClass
func (r *NamespaceReconciler) findNamespacesForClass(ctx context.Context, obj client.Object) []reconcile.Request {
	nsClass := obj.(*akuityv1.NamespaceClass)
	var nsList corev1.NamespaceList
	// Find all Namespaces with matching label
	if err := r.List(ctx, &nsList, client.MatchingLabels{NamespaceClassLabel: nsClass.Name}); err != nil {
		return []reconcile.Request{}
	}
	requests := make([]reconcile.Request, len(nsList.Items))
	for i, ns := range nsList.Items {
		requests[i] = reconcile.Request{NamespacedName: types.NamespacedName{Name: ns.Name}}
	}
	return requests
}

// SetupWithManager registers ns reconcilers with the controller manager
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor(ControllerName)

	// Register NamespaceReconciler
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.MaxConcurrentReconciles,
		}).
		Watches(
			&akuityv1.NamespaceClass{},
			handler.EnqueueRequestsFromMapFunc(r.findNamespacesForClass),
		).
		Complete(r)
}

// SetupWithManager registers ns class reconcilers with the controller manager
func (r *NamespaceClassReconciler) SetupWithManager(mgr ctrl.Manager) error {

	classReconciler := &NamespaceClassReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.MaxConcurrentReconciles,
		}).
		For(&akuityv1.NamespaceClass{}).
		Complete(classReconciler)
}
