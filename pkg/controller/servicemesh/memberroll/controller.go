package memberroll

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	pkgerrors "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/maistra/istio-operator/pkg/apis/maistra"
	v1 "github.com/maistra/istio-operator/pkg/apis/maistra/v1"
	"github.com/maistra/istio-operator/pkg/controller/common"
)

const (
	controllerName = "servicemeshmemberroll-controller"

	statusAnnotationConfiguredMemberCount = "configuredMemberCount"
)

// Add creates a new ServiceMeshMemberRoll Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	kialiReconciler := defaultKialiReconciler{Client: mgr.GetClient()}
	cniConfig, err := common.InitCNIConfig(mgr)
	if err != nil {
		return err
	}
	return add(mgr, newReconciler(mgr.GetClient(), mgr.GetScheme(), mgr.GetRecorder(controllerName), newNamespaceReconciler, &kialiReconciler, cniConfig))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(cl client.Client, scheme *runtime.Scheme, eventRecorder record.EventRecorder, namespaceReconcilerFactory NamespaceReconcilerFactory, kialiReconciler KialiReconciler, cniConfig common.CNIConfig) *MemberRollReconciler {
	return &MemberRollReconciler{
		ControllerResources: common.ControllerResources{
			Client:        cl,
			Scheme:        scheme,
			EventRecorder: eventRecorder,
			PatchFactory:  common.NewPatchFactory(cl),
		},
		cniConfig:                  cniConfig,
		namespaceReconcilerFactory: namespaceReconcilerFactory,
		kialiReconciler:            kialiReconciler,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r *MemberRollReconciler) error {
	log := createLogger()
	ctx := common.NewContextWithLog(common.NewContext(), log)

	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{MaxConcurrentReconciles: common.Options.MemberRollReconcilers, Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource ServiceMeshMemberRoll
	err = c.Watch(&source.Kind{Type: &v1.ServiceMeshMemberRoll{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO: should this be moved somewhere else?
	err = mgr.GetFieldIndexer().IndexField(&v1.ServiceMeshMemberRoll{}, "spec.members", func(obj runtime.Object) []string {
		roll := obj.(*v1.ServiceMeshMemberRoll)
		return roll.Spec.Members
	})
	if err != nil {
		return err
	}

	// watch namespaces and trigger reconcile requests as those that match a member roll come and go
	err = c.Watch(&source.Kind{Type: &corev1.Namespace{}}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(func(ns handler.MapObject) []reconcile.Request {
			list := &v1.ServiceMeshMemberRollList{}
			err := mgr.GetClient().List(ctx, client.MatchingField("spec.members", ns.Meta.GetName()), list)
			if err != nil {
				log.Error(err, "Could not list ServiceMeshMemberRolls")
			}

			var requests []reconcile.Request
			for _, smmr := range list.Items {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      smmr.Name,
						Namespace: smmr.Namespace,
					},
				})
			}
			return requests
		}),
	}, predicate.Funcs{
		UpdateFunc: func(_ event.UpdateEvent) bool {
			// we don't need to process the member roll on updates
			return false
		},
		GenericFunc: func(_ event.GenericEvent) bool {
			// we don't need to process the member roll on generic events
			return false
		},
	})
	if err != nil {
		return err
	}

	// watch control planes and trigger reconcile requests as they come and go
	err = c.Watch(&source.Kind{Type: &v1.ServiceMeshControlPlane{}}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(func(smcpMap handler.MapObject) []reconcile.Request {
			if smcp, ok := smcpMap.Object.(*v1.ServiceMeshControlPlane); !ok {
				return nil
			} else if installCondition := smcp.Status.GetCondition(v1.ConditionTypeReconciled); installCondition.Status != v1.ConditionStatusTrue {
				// skip processing if the smcp is not fully reconciled (e.g. it's installing or updating)
				return nil
			}
			list := &v1.ServiceMeshMemberRollList{}
			err := mgr.GetClient().List(ctx, client.InNamespace(smcpMap.Meta.GetNamespace()), list)
			if err != nil {
				log.Error(err, "Could not list ServiceMeshMemberRolls")
			}

			var requests []reconcile.Request
			for _, smmr := range list.Items {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      smmr.Name,
						Namespace: smmr.Namespace,
					},
				})
			}
			return requests
		}),
	}, predicate.Funcs{
		DeleteFunc: func(_ event.DeleteEvent) bool {
			// we don't need to process the member roll on deletions (we add an owner reference, so it gets deleted automatically)
			return false
		},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &MemberRollReconciler{}

type NamespaceReconcilerFactory func(ctx context.Context, cl client.Client, meshNamespace string, meshVersion string, isCNIEnabled bool) (NamespaceReconciler, error)

// MemberRollReconciler reconciles a ServiceMeshMemberRoll object
type MemberRollReconciler struct {
	common.ControllerResources
	cniConfig common.CNIConfig

	namespaceReconcilerFactory NamespaceReconcilerFactory
	kialiReconciler            KialiReconciler
}

// Reconcile reads that state of the cluster for a ServiceMeshMemberRoll object and makes changes based on the state read
// and what is in the ServiceMeshMemberRoll.Spec
func (r *MemberRollReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := createLogger().WithValues("ServiceMeshMemberRoll", request)
	ctx := common.NewReconcileContext(reqLogger)

	reqLogger.Info("Processing ServiceMeshMemberRoll")
	defer func() {
		reqLogger.Info("processing complete")
	}()

	// Fetch the ServiceMeshMemberRoll instance
	instance := &v1.ServiceMeshMemberRoll{}
	err := r.Client.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) || errors.IsGone(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			reqLogger.Info("ServiceMeshMemberRoll deleted")
			return reconcile.Result{}, nil
		}
		// Error reading the object
		return reconcile.Result{}, err
	}

	deleted := instance.GetDeletionTimestamp() != nil
	finalizers := sets.NewString(instance.Finalizers...)
	if deleted {
		if !finalizers.Has(common.FinalizerName) {
			reqLogger.Info("ServiceMeshMemberRoll deleted")
			return reconcile.Result{}, nil
		}
		reqLogger.Info("Deleting ServiceMeshMemberRoll")

		configuredNamespaces, err := r.findConfiguredNamespaces(ctx, instance.Namespace)
		if err != nil {
			reqLogger.Error(err, "error listing mesh member namespaces")
			return reconcile.Result{}, err
		}

		configuredMembers, err, nsErrors := r.reconcileNamespaces(ctx, nil, nameSet(&configuredNamespaces), instance.Namespace, maistra.DefaultVersion.String())
		if err != nil {
			return reconcile.Result{}, err
		}
		instance.Status.ConfiguredMembers = configuredMembers
		if len(nsErrors) > 0 {
			return reconcile.Result{}, utilerrors.NewAggregate(nsErrors)
		}

		err = r.kialiReconciler.reconcileKiali(ctx, instance.Namespace, []string{})
		if err != nil {
			return reconcile.Result{}, err
		}

		// get fresh SMMR from cache to minimize the chance of a conflict during update (the SMMR might have been updated during the execution of Reconcile())
		instance = &v1.ServiceMeshMemberRoll{}
		if err := r.Client.Get(ctx, request.NamespacedName, instance); err == nil {
			finalizers = sets.NewString(instance.Finalizers...)
			finalizers.Delete(common.FinalizerName)
			instance.SetFinalizers(finalizers.List())
			if err := r.Client.Update(ctx, instance); err == nil {
				reqLogger.Info("Removed finalizer")
			} else if !(errors.IsGone(err) || errors.IsNotFound(err)) {
				return reconcile.Result{}, pkgerrors.Wrap(err, "Error removing ServiceMeshMemberRoll finalizer")
			}
		} else if !(errors.IsGone(err) || errors.IsNotFound(err)) {
			return reconcile.Result{}, pkgerrors.Wrap(err, "Error getting ServiceMeshMemberRoll prior to removing finalizer")
		}

		return reconcile.Result{}, nil
	} else if !finalizers.Has(common.FinalizerName) {
		reqLogger.Info("Adding finalizer to ServiceMeshMemberRoll", "finalizer", common.FinalizerName)
		finalizers.Insert(common.FinalizerName)
		instance.SetFinalizers(finalizers.List())
		err = r.Client.Update(ctx, instance)
		return reconcile.Result{}, err
	}

	reqLogger.Info("Reconciling ServiceMeshMemberRoll")

	meshList := &v1.ServiceMeshControlPlaneList{}
	err = r.Client.List(ctx, client.InNamespace(instance.Namespace), meshList)
	if err != nil {
		return reconcile.Result{}, pkgerrors.Wrap(err, "Error retrieving ServiceMeshControlPlane resources")
	}
	meshCount := len(meshList.Items)
	if meshCount != 1 {
		if meshCount > 0 {
			reqLogger.Info("Skipping reconciliation of SMMR, because multiple ServiceMeshControlPlane resources exist in the project", "project", instance.Namespace)
		} else {
			reqLogger.Info("Skipping reconciliation of SMMR, because no ServiceMeshControlPlane exists in the project.", "project", instance.Namespace)
		}
		// when a control plane is created/deleted our watch will pick it up and issue a new reconcile event
		return reconcile.Result{}, nil
	}

	mesh := &meshList.Items[0]

	// verify owner reference, so member roll gets deleted with control plane
	addOwner := true
	for _, ownerRef := range instance.GetOwnerReferences() {
		if ownerRef.UID == mesh.GetUID() {
			addOwner = false
			break
		}
	}
	if addOwner {
		// add owner reference to the mesh so we can clean up if the mesh gets deleted
		reqLogger.Info("Adding OwnerReference to ServiceMeshMemberRoll")
		owner := metav1.NewControllerRef(mesh, v1.SchemeGroupVersion.WithKind("ServiceMeshControlPlane"))
		owner.Controller = nil
		owner.BlockOwnerDeletion = nil
		instance.SetOwnerReferences([]metav1.OwnerReference{*owner})
		err = r.Client.Update(ctx, instance)
		if err != nil {
			return reconcile.Result{}, pkgerrors.Wrap(err, "error adding ownerReference to ServiceMeshMemberRoll")
		}
		return reconcile.Result{}, nil
	}

	if mesh.Status.ObservedGeneration == 0 {
		reqLogger.Info("Initial service mesh installation has not completed")
		// a new reconcile request will be issued when the control plane resource is updated
		return reconcile.Result{}, nil
	} else if meshReconcileStatus := mesh.Status.GetCondition(v1.ConditionTypeReconciled); meshReconcileStatus.Status != v1.ConditionStatusTrue {
		// a new reconcile request will be issued when the control plane resource is updated
		reqLogger.Info("skipping reconciliation because mesh is not in a known good state")
		return reconcile.Result{}, nil
	}

	var newConfiguredMembers []string
	var nsErrors []error
	allNamespaces, err := r.getAllNamespaces(ctx)
	if err != nil {
		return reconcile.Result{}, pkgerrors.Wrap(err, "could not list all namespaces")
	}
	requiredMembers := sets.NewString(instance.Spec.Members...)
	configuredMembers := sets.NewString(instance.Status.ConfiguredMembers...)
	deletedMembers := configuredMembers.Difference(allNamespaces)
	unconfiguredMembers := allNamespaces.Intersection(requiredMembers.Difference(configuredMembers))

	// never include the mesh namespace in unconfigured list
	delete(unconfiguredMembers, instance.Namespace)

	meshVersion := mesh.Spec.Version
	if len(meshVersion) == 0 {
		meshVersion = maistra.LegacyVersion.String()
	}

	// this must be checked first to ensure the correct cni network is attached to the members
	if mesh.Status.GetReconciledVersion() != instance.Status.ServiceMeshReconciledVersion { // service mesh has been updated
		reqLogger.Info("Reconciling ServiceMeshMemberRoll namespaces with new generation of ServiceMeshControlPlane")

		instance.Status.ConfiguredMembers = make([]string, 0, len(instance.Spec.Members))
		newConfiguredMembers, err, nsErrors = r.reconcileNamespaces(ctx, requiredMembers, nil, instance.Namespace, meshVersion)
		if err != nil {
			return reconcile.Result{}, err
		}
		instance.Status.ConfiguredMembers = newConfiguredMembers
		instance.Status.ServiceMeshGeneration = mesh.Status.ObservedGeneration
		instance.Status.ServiceMeshReconciledVersion = mesh.Status.GetReconciledVersion()
	} else if instance.Generation != instance.Status.ObservedGeneration { // member roll has been updated

		reqLogger.Info("Reconciling new generation of ServiceMeshMemberRoll")

		instance.Status.ConfiguredMembers = make([]string, 0, len(instance.Spec.Members))

		// setup namespaces
		configuredNamespaces, err := r.findConfiguredNamespaces(ctx, mesh.Namespace)
		if err != nil {
			reqLogger.Error(err, "error listing mesh member namespaces")
			return reconcile.Result{}, err
		}

		existingMembers := nameSet(&configuredNamespaces)
		namespacesToRemove := existingMembers.Difference(requiredMembers)
		newConfiguredMembers, err, nsErrors = r.reconcileNamespaces(ctx, requiredMembers, namespacesToRemove, instance.Namespace, meshVersion)
		if err != nil {
			return reconcile.Result{}, err
		}
		instance.Status.ConfiguredMembers = newConfiguredMembers
		instance.Status.ServiceMeshGeneration = mesh.Status.ObservedGeneration
		instance.Status.ServiceMeshReconciledVersion = mesh.Status.GetReconciledVersion()
	} else if len(unconfiguredMembers) > 0 { // required namespace that was missing has been created
		reqLogger.Info("Reconciling newly created namespaces associated with this ServiceMeshMemberRoll")

		newConfiguredMembers, err, nsErrors = r.reconcileNamespaces(ctx, requiredMembers, nil, instance.Namespace, meshVersion)
		if err != nil {
			return reconcile.Result{}, err
		}
		instance.Status.ConfiguredMembers = newConfiguredMembers
		// we don't update the ServiceMeshGeneration in case the other members need to be updated
	} else if len(deletedMembers) > 0 { // namespace that was configured has been deleted
		// nothing to do, but we need to update the ConfiguredMembers field
		reqLogger.Info("Removing deleted namespaces from ConfiguredMembers")
		instance.Status.ConfiguredMembers = make([]string, 0, len(instance.Spec.Members))
		for _, member := range instance.Spec.Members {
			if member == instance.Namespace {
				// we never operate on the control plane namespace
				continue
			}
			if allNamespaces.Has(member) {
				instance.Status.ConfiguredMembers = append(instance.Status.ConfiguredMembers, member)
			}
		}
	} else {
		// nothing to do
		reqLogger.Info("nothing to reconcile")
		return reconcile.Result{}, nil
	}

	if requiredMembers.Equal(sets.NewString(instance.Status.ConfiguredMembers...)) {
		instance.Status.SetCondition(v1.ServiceMeshMemberRollCondition{
			Type:    v1.ConditionTypeMemberRollReady,
			Status:  corev1.ConditionTrue,
			Reason:  v1.ConditionReasonConfigured,
			Message: "All namespaces have been configured successfully",
		})
	} else {
		instance.Status.SetCondition(v1.ServiceMeshMemberRollCondition{
			Type:    v1.ConditionTypeMemberRollReady,
			Status:  corev1.ConditionFalse,
			Reason:  v1.ConditionReasonNamespaceMissing,
			Message: "A namespace listed in .spec.members does not exist",
		})
	}

	if instance.Status.Annotations == nil {
		instance.Status.Annotations = map[string]string{}
	}
	instance.Status.Annotations[statusAnnotationConfiguredMemberCount] = fmt.Sprintf("%d/%d", len(instance.Status.ConfiguredMembers), requiredMembers.Len())

	err = utilerrors.NewAggregate(nsErrors)
	if err == nil {
		instance.Status.ObservedGeneration = instance.GetGeneration()
		err = r.Client.Status().Update(ctx, instance)
		if err != nil {
			reqLogger.Error(err, "error updating status for ServiceMeshMemberRoll")
		}
	} else {
		return reconcile.Result{}, err
	}

	// tell Kiali about all the namespaces in the mesh
	kialiErr := r.kialiReconciler.reconcileKiali(ctx, instance.Namespace, instance.Status.ConfiguredMembers)

	if err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, kialiErr
}

func (r *MemberRollReconciler) findConfiguredNamespaces(ctx context.Context, meshNamespace string) (corev1.NamespaceList, error) {
	list := corev1.NamespaceList{}
	labelSelector := map[string]string{common.MemberOfKey: meshNamespace}
	err := r.Client.List(ctx, client.MatchingLabels(labelSelector).InNamespace(""), &list)
	return list, err
}

func (r *MemberRollReconciler) reconcileNamespaces(ctx context.Context, namespacesToReconcile, namespacesToRemove sets.String, controlPlaneNamespace string, controlPlaneVersion string) (configuredMembers []string, err error, nsErrors []error) {
	reqLogger := common.LogFromContext(ctx)
	// current configuredNamespaces are namespacesToRemove minus control plane namespace
	configured := sets.NewString(namespacesToRemove.List()...)
	configured.Delete(controlPlaneNamespace)
	// create reconciler
	reconciler, err := r.namespaceReconcilerFactory(ctx, r.Client, controlPlaneNamespace, controlPlaneVersion, r.cniConfig.Enabled)
	if err != nil {
		return nil, err, nil
	}

	for ns := range namespacesToRemove {
		if ns == controlPlaneNamespace {
			// we never operate on the control plane namespace
			continue
		}
		err = reconciler.removeNamespaceFromMesh(ctx, ns)
		if err != nil {
			nsErrors = append(nsErrors, err)
		} else {
			configured.Delete(ns)
		}
	}
	for ns := range namespacesToReconcile {
		if ns == controlPlaneNamespace {
			// we never operate on the control plane namespace
			reqLogger.Info("ignoring control plane namespace in members list of ServiceMeshMemberRoll")
			continue
		}
		err = reconciler.reconcileNamespaceInMesh(ctx, ns)
		if err != nil {
			if errors.IsNotFound(err) || errors.IsGone(err) { // TODO: this check should be performed inside reconcileNamespaceInMesh
				reqLogger.Info("namespace to configure with mesh is missing", "namespace", ns)
			} else {
				nsErrors = append(nsErrors, err)
			}
		} else {
			configured.Insert(ns)
		}
	}
	configuredMembers = configured.List()
	return configuredMembers, nil, nsErrors
}

type KialiReconciler interface {
	reconcileKiali(ctx context.Context, kialiCRNamespace string, configuredMembers []string) error
}

type defaultKialiReconciler struct {
	Client client.Client
}

func (r *defaultKialiReconciler) reconcileKiali(ctx context.Context, kialiCRNamespace string, configuredMembers []string) error {
	reqLogger := common.LogFromContext(ctx)
	reqLogger.Info("Attempting to get Kiali CR", "kialiCRNamespace", kialiCRNamespace)

	kialiCRName := "kiali"
	kialiCR := &unstructured.Unstructured{}
	kialiCR.SetAPIVersion("kiali.io/v1alpha1")
	kialiCR.SetKind("Kiali")
	kialiCR.SetNamespace(kialiCRNamespace)
	kialiCR.SetName(kialiCRName)
	err := r.Client.Get(ctx, client.ObjectKey{Name: kialiCRName, Namespace: kialiCRNamespace}, kialiCR)
	if err != nil {
		if meta.IsNoMatchError(err) || errors.IsNotFound(err) || errors.IsGone(err) {
			reqLogger.Info("Kiali CR does not exist, Kiali probably not enabled")
			return nil
		}
		return pkgerrors.Wrap(err, "error retrieving Kiali CR from mesh")
	}

	// just get an array of strings consisting of the list of namespaces to be accessible to Kiali
	var accessibleNamespaces []string
	if len(configuredMembers) == 0 {
		// no configured members available - just allow access only to the control plane namespace
		accessibleNamespaces = []string{kialiCRNamespace}
	} else {
		// we are in multitenency mode with some namespaces being made available to kiali
		accessibleNamespaces = make([]string, 0, len(configuredMembers))
		for _, cm := range configuredMembers {
			accessibleNamespaces = append(accessibleNamespaces, cm)
		}
	}

	if existingNamespaces, found, _ := unstructured.NestedStringSlice(kialiCR.UnstructuredContent(), "spec", "deployment", "accessible_namespaces"); found && sets.NewString(accessibleNamespaces...).Equal(sets.NewString(existingNamespaces...)) {
		reqLogger.Info("Kiali CR deployment.accessible_namespaces already up to date")
		return nil
	}

	reqLogger.Info("Updating Kiali CR deployment.accessible_namespaces", "accessibleNamespaces", accessibleNamespaces)

	err = unstructured.SetNestedStringSlice(kialiCR.UnstructuredContent(), accessibleNamespaces, "spec", "deployment", "accessible_namespaces")
	if err != nil {
		return pkgerrors.Wrapf(err, "cannot set deployment.accessible_namespaces in Kiali CR %s/%s", kialiCRNamespace, kialiCRName)
	}

	err = r.Client.Update(ctx, kialiCR)
	if err != nil {
		return pkgerrors.Wrapf(err, "cannot update Kiali CR %s/%s with new accessible namespaces", kialiCRNamespace, kialiCRName)
	}

	reqLogger.Info("Kiali CR deployment.accessible_namespaces updated", "accessibleNamespaces", accessibleNamespaces)
	return nil
}

func (r *MemberRollReconciler) getAllNamespaces(ctx context.Context) (sets.String, error) {
	namespaceList := &corev1.NamespaceList{}
	err := r.Client.List(ctx, nil, namespaceList)
	if err != nil {
		return nil, err
	}
	allNamespaces := sets.NewString()
	for _, namespace := range namespaceList.Items {
		allNamespaces.Insert(namespace.Name)
	}
	return allNamespaces, nil
}

type NamespaceReconciler interface {
	reconcileNamespaceInMesh(ctx context.Context, namespace string) error
	removeNamespaceFromMesh(ctx context.Context, namespace string) error
}

func nameSet(list runtime.Object) sets.String {
	set := sets.NewString()
	err := meta.EachListItem(list, func(obj runtime.Object) error {
		o, err := meta.Accessor(obj)
		if err != nil {
			return err
		}
		set.Insert(o.GetName())
		return nil
	})
	if err != nil {
		// meta.EachListItem only returns an error if you pass in something that's not a ResourceList, so
		// it we don't expect it to ever return an error.
		panic(err)
	}
	return set
}

// Don't use this function to obtain a logger. Get it by invoking
// common.LogFromContext(ctx) to ensure that the logger has the
// correct context info and logs it.
func createLogger() logr.Logger {
	return logf.Log.WithName(controllerName)
}
