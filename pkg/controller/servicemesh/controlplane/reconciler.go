package controlplane

import (
	"context"
	"fmt"
	"io/ioutil"
	"path"
	"reflect"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/helm/pkg/manifest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/maistra/istio-operator/pkg/apis/maistra"
	v1 "github.com/maistra/istio-operator/pkg/apis/maistra/v1"
	"github.com/maistra/istio-operator/pkg/bootstrap"
	"github.com/maistra/istio-operator/pkg/controller/common"
	"github.com/maistra/istio-operator/pkg/controller/hacks"
)

type controlPlaneInstanceReconciler struct {
	common.ControllerResources
	Instance       *v1.ServiceMeshControlPlane
	Status         *v1.ControlPlaneStatus
	ownerRefs      []metav1.OwnerReference
	meshGeneration string
	renderings     map[string][]manifest.Manifest
	lastComponent  string
	cniConfig      common.CNIConfig
}

// ensure controlPlaneInstanceReconciler implements ControlPlaneInstanceReconciler
var _ ControlPlaneInstanceReconciler = &controlPlaneInstanceReconciler{}

// these components have to be installed in the specified order
var orderedCharts = []string{
	"istio", // core istio resources
	"istio/charts/security",
	"istio/charts/prometheus",
	"istio/charts/tracing",
	"istio/charts/galley",
	"istio/charts/mixer",
	"istio/charts/pilot",
	"istio/charts/gateways",
	"istio/charts/sidecarInjectorWebhook",
	"istio/charts/grafana",
	"istio/charts/kiali",
}

const (
	// Event reasons
	eventReasonInstalling              = "Installing"
	eventReasonPausingInstall          = "PausingInstall"
	eventReasonPausingUpdate           = "PausingUpdate"
	eventReasonInstalled               = "Installed"
	eventReasonUpdating                = "Updating"
	eventReasonUpdated                 = "Updated"
	eventReasonDeleting                = "Deleting"
	eventReasonDeleted                 = "Deleted"
	eventReasonPruning                 = "Pruning"
	eventReasonFailedRemovingFinalizer = "FailedRemovingFinalizer"
	eventReasonFailedDeletingResources = "FailedDeletingResources"
	eventReasonNotReady                = "NotReady"
	eventReasonReady                   = "Ready"
)

func NewControlPlaneInstanceReconciler(controllerResources common.ControllerResources, newInstance *v1.ServiceMeshControlPlane, cniConfig common.CNIConfig) ControlPlaneInstanceReconciler {
	return &controlPlaneInstanceReconciler{
		ControllerResources: controllerResources,
		Instance:            newInstance,
		Status:              newInstance.Status.DeepCopy(),
		cniConfig:           cniConfig,
	}
}

func (r *controlPlaneInstanceReconciler) Reconcile(ctx context.Context) (result reconcile.Result, err error) {
	log := common.LogFromContext(ctx)
	log.Info("Reconciling ServiceMeshControlPlane", "Status", r.Instance.Status.StatusType)
	if r.Status.GetCondition(v1.ConditionTypeReconciled).Status != v1.ConditionStatusFalse {
		r.initializeReconcileStatus()
		err := r.PostStatus(ctx)
		return reconcile.Result{}, err // ensure that the new reconcile status is posted immediately. Reconciliation will resume when the status update comes back into the operator
	}

	var ready bool
	// make sure status gets updated on exit
	reconciledCondition := r.Status.GetCondition(v1.ConditionTypeReconciled)
	reconciliationMessage := reconciledCondition.Message
	reconciliationReason := reconciledCondition.Reason
	reconciliationComplete := false
	defer func() {
		// this ensures we're updating status (if necessary) and recording events on exit
		if statusErr := r.postReconciliationStatus(ctx, reconciliationReason, reconciliationMessage, err); statusErr != nil {
			if err == nil {
				err = statusErr
			} else {
				log.Error(statusErr, "Error posting reconciliation status")
			}
		}
		if reconciliationComplete {
			hacks.ReduceLikelihoodOfRepeatedReconciliation(ctx)
		}
	}()

	if r.renderings == nil {
		// error handling
		defer func() {
			if err != nil {
				r.renderings = nil
				r.lastComponent = ""
				updateReconcileStatus(&r.Status.StatusType, err)
			}
		}()

		// Render the templates
		err = r.renderCharts(ctx)
		if err != nil {
			// we can't progress here
			reconciliationReason = v1.ConditionReasonReconcileError
			reconciliationMessage = "Error rendering helm charts"
			err = errors.Wrap(err, reconciliationMessage)
			return
		}

		// install istio

		// set the auto-injection flag
		// update injection label on namespace
		// XXX: this should probably only be done when installing a control plane
		// e.g. spec.pilot.enabled || spec.mixer.enabled || spec.galley.enabled || spec.sidecarInjectorWebhook.enabled || ....
		// which is all we're supporting atm.  if the scope expands to allow
		// installing custom gateways, etc., we should revisit this.
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.Instance.Namespace}}
		err = r.Client.Get(ctx, client.ObjectKey{Name: r.Instance.Namespace}, namespace)
		if err == nil {
			updateLabels := false
			if namespace.Labels == nil {
				namespace.Labels = map[string]string{}
			}
			// make sure injection is disabled for the control plane
			if label, ok := namespace.Labels["maistra.io/ignore-namespace"]; !ok || label != "ignore" {
				log.Info("Adding maistra.io/ignore-namespace=ignore label to Request.Namespace")
				namespace.Labels["maistra.io/ignore-namespace"] = "ignore"
				updateLabels = true
			}
			// make sure the member-of label is specified, so networking works correctly
			if label, ok := namespace.Labels[common.MemberOfKey]; !ok || label != namespace.GetName() {
				log.Info(fmt.Sprintf("Adding %s label to Request.Namespace", common.MemberOfKey))
				namespace.Labels[common.MemberOfKey] = namespace.GetName()
				updateLabels = true
			}
			if updateLabels {
				err = r.Client.Update(ctx, namespace)
			}
		}
		if err != nil {
			// bail if there was an error updating the namespace
			reconciliationReason = v1.ConditionReasonReconcileError
			reconciliationMessage = "Error updating labels on mesh namespace"
			err = errors.Wrap(err, reconciliationMessage)
			return
		}

		// initialize new Status
		componentStatuses := make([]*v1.ComponentStatus, 0, len(r.Status.ComponentStatus))
		for chartName := range r.renderings {
			componentName := componentFromChartName(chartName)
			componentStatus := r.Status.FindComponentByName(componentName)
			if componentStatus == nil {
				componentStatus = v1.NewComponentStatus()
				componentStatus.Resource = componentName
			}
			componentStatus.SetCondition(v1.Condition{
				Type:   v1.ConditionTypeReconciled,
				Status: v1.ConditionStatusFalse,
			})
			componentStatuses = append(componentStatuses, componentStatus)
		}
		r.Status.ComponentStatus = componentStatuses

		// initialize common data
		owner := metav1.NewControllerRef(r.Instance, v1.SchemeGroupVersion.WithKind("ServiceMeshControlPlane"))
		r.ownerRefs = []metav1.OwnerReference{*owner}
		r.meshGeneration = v1.CurrentReconciledVersion(r.Instance.GetGeneration())

		// Ensure CRDs are installed
		chartsDir := common.Options.GetChartsDir(r.Instance.Spec.Version)
		if err = bootstrap.InstallCRDs(common.NewContextWithLog(ctx, log.WithValues("version", r.Instance.Spec.Version)), r.Client, chartsDir); err != nil {
			reconciliationReason = v1.ConditionReasonReconcileError
			reconciliationMessage = "Failed to install/update Istio CRDs"
			log.Error(err, reconciliationMessage)
			return
		}

		// Ensure Istio CNI is installed
		if r.cniConfig.Enabled {
			r.lastComponent = "cni"
			if err = bootstrap.InstallCNI(ctx, r.Client, r.cniConfig); err != nil {
				reconciliationReason = v1.ConditionReasonReconcileError
				reconciliationMessage = "Failed to install/update Istio CNI"
				log.Error(err, reconciliationMessage)
				return
			} else if ready, _ := r.isCNIReady(ctx); !ready {
				reconciliationReason = v1.ConditionReasonPausingInstall
				reconciliationMessage = fmt.Sprintf("Paused until %s becomes ready", "cni")
				return
			}
		}

		if err = r.reconcileRBAC(ctx); err != nil {
			reconciliationReason = v1.ConditionReasonReconcileError
			reconciliationMessage = "Failed to install/update Maistra RBAC resources"
			log.Error(err, reconciliationMessage)
			return
		}

	} else if r.lastComponent != "" {
		if readinessMap, readinessErr := r.calculateComponentReadiness(ctx); readinessErr == nil {
			// if we've already begun reconciling, make sure we weren't waiting for
			// the last component to become ready
			if ready, ok := readinessMap[r.lastComponent]; ok && !ready {
				// last component has not become ready yet
				log.Info(fmt.Sprintf("Paused until %s becomes ready", r.lastComponent))
				return
			}
		} else {
			// error calculating readiness
			reconciliationReason = v1.ConditionReasonProbeError
			reconciliationMessage = fmt.Sprintf("Error checking readiness of component %s", r.lastComponent)
			err = errors.Wrap(readinessErr, reconciliationMessage)
			log.Error(err, reconciliationMessage)
			return
		}
	}

	// create components
	for _, chartName := range orderedCharts {
		if ready, err = r.processComponentManifests(ctx, chartName); !ready {
			reconciliationReason, reconciliationMessage, err = r.pauseReconciliation(ctx, chartName, err)
			return
		}
	}

	// any other istio components
	for key := range r.renderings {
		if !strings.HasPrefix(key, "istio/") {
			continue
		}
		if ready, err = r.processComponentManifests(ctx, key); !ready {
			reconciliationReason, reconciliationMessage, err = r.pauseReconciliation(ctx, key, err)
			return
		}
	}

	// install 3scale and any other components
	for key := range r.renderings {
		if ready, err = r.processComponentManifests(ctx, key); !ready {
			reconciliationReason, reconciliationMessage, err = r.pauseReconciliation(ctx, key, err)
			return
		}
	}

	// we still need to prune if this is the first generation, e.g. if the operator was updated during the install,
	// it's possible that some resources in the original version may not be present in the new version.
	// delete unseen components
	reconciliationMessage = "Pruning obsolete resources"
	r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReasonPruning, reconciliationMessage)
	log.Info(reconciliationMessage)
	err = r.prune(ctx, r.meshGeneration)
	if err != nil {
		reconciliationReason = v1.ConditionReasonReconcileError
		reconciliationMessage = "Error pruning obsolete resources"
		err = errors.Wrap(err, reconciliationMessage)
		return
	}

	if r.isUpdating() {
		reconciliationReason = v1.ConditionReasonUpdateSuccessful
		reconciliationMessage = fmt.Sprintf("Successfully updated from version %s to version %s", r.Status.GetReconciledVersion(), r.meshGeneration)
		r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReasonUpdated, reconciliationMessage)
	} else {
		reconciliationReason = v1.ConditionReasonInstallSuccessful
		reconciliationMessage = fmt.Sprintf("Successfully installed version %s", r.meshGeneration)
		r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReasonInstalled, reconciliationMessage)
	}
	r.Status.ObservedGeneration = r.Instance.GetGeneration()
	r.Status.ReconciledVersion = r.meshGeneration
	updateReconcileStatus(&r.Status.StatusType, nil)

	_, err = r.updateReadinessStatus(ctx) // this only updates the local object instance; it doesn't post the status update; postReconciliationStatus (called using defer) actually does that

	reconciliationComplete = true
	log.Info("Completed ServiceMeshControlPlane reconcilation")
	return
}

func (r *controlPlaneInstanceReconciler) pauseReconciliation(ctx context.Context, chartName string, err error) (v1.ConditionReason, string, error) {
	log := common.LogFromContext(ctx)
	var eventReason string
	var conditionReason v1.ConditionReason
	var reconciliationMessage string
	if r.isUpdating() {
		eventReason = eventReasonPausingUpdate
		conditionReason = v1.ConditionReasonPausingUpdate
	} else {
		eventReason = eventReasonPausingInstall
		conditionReason = v1.ConditionReasonPausingInstall
	}
	componentName := componentFromChartName(chartName)
	if err == nil {
		reconciliationMessage = fmt.Sprintf("Paused until %s becomes ready", componentName)
		r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReason, reconciliationMessage)
		log.Info(reconciliationMessage)
	} else {
		conditionReason = v1.ConditionReasonReconcileError
		reconciliationMessage = fmt.Sprintf("Error processing component %s", componentName)
		log.Error(err, reconciliationMessage)
	}
	return conditionReason, reconciliationMessage, errors.Wrapf(err, reconciliationMessage)
}

func (r *controlPlaneInstanceReconciler) isUpdating() bool {
	return r.Instance.Status.ObservedGeneration != 0
}

// mergeValues merges a map containing input values on top of a map containing
// base values, giving preference to the base values for conflicts
func mergeValues(base map[string]interface{}, input map[string]interface{}) map[string]interface{} {
	if base == nil {
		base = make(map[string]interface{}, 1)
	}

	for key, value := range input {
		//if the key doesn't already exist, add it
		if _, exists := base[key]; !exists {
			base[key] = value
			continue
		}

		// at this point, key exists in both input and base.
		// If both are maps, recurse.
		// If only input is a map, ignore it. We don't want to overrwrite base.
		// If both are values, again, ignore it since we don't want to overrwrite base.
		if baseKeyAsMap, baseOK := base[key].(map[string]interface{}); baseOK {
			if inputAsMap, inputOK := value.(map[string]interface{}); inputOK {
				base[key] = mergeValues(baseKeyAsMap, inputAsMap)
			}
		}
	}
	return base
}

func (r *controlPlaneInstanceReconciler) getSMCPTemplate(name string, maistraVersion string) (v1.ControlPlaneSpec, error) {
	if strings.Contains(name, "/") {
		return v1.ControlPlaneSpec{}, fmt.Errorf("template name contains invalid character '/'")
	}

	templateContent, err := ioutil.ReadFile(path.Join(common.Options.GetUserTemplatesDir(), name))
	if err != nil {
		//if we can't read from the user template path, try from the default path
		//we use two paths because Kubernetes will not auto-update volume mounted
		//configmaps mounted in directories with pre-existing content
		defaultTemplateContent, defaultErr := ioutil.ReadFile(path.Join(common.Options.GetDefaultTemplatesDir(maistraVersion), name))
		if defaultErr != nil {
			return v1.ControlPlaneSpec{}, fmt.Errorf("template cannot be loaded from user or default directory. Error from user: %s. Error from default: %s", err, defaultErr)
		}
		templateContent = defaultTemplateContent
	}

	var template v1.ServiceMeshControlPlane
	if err = yaml.Unmarshal(templateContent, &template); err != nil {
		return v1.ControlPlaneSpec{}, fmt.Errorf("failed to parse template %s contents: %s", name, err)
	}
	return template.Spec, nil
}

//renderSMCPTemplates traverses and processes all of the references templates
func (r *controlPlaneInstanceReconciler) recursivelyApplyTemplates(ctx context.Context, smcp v1.ControlPlaneSpec, version string, visited sets.String) (v1.ControlPlaneSpec, error) {
	log := common.LogFromContext(ctx)
	if smcp.Template == "" {
		return smcp, nil
	}
	log.Info(fmt.Sprintf("processing smcp template %s", smcp.Template))

	if visited.Has(smcp.Template) {
		return smcp, fmt.Errorf("SMCP templates form cyclic dependency. Cannot proceed")
	}

	template, err := r.getSMCPTemplate(smcp.Template, version)
	if err != nil {
		return smcp, err
	}

	template, err = r.recursivelyApplyTemplates(ctx, template, version, visited)
	if err != nil {
		log.Info(fmt.Sprintf("error rendering SMCP templates: %s\n", err))
		return smcp, err
	}

	visited.Insert(smcp.Template)

	smcp.Istio = mergeValues(smcp.Istio, template.Istio)
	smcp.ThreeScale = mergeValues(smcp.ThreeScale, template.ThreeScale)
	return smcp, nil
}

func (r *controlPlaneInstanceReconciler) applyTemplates(ctx context.Context, smcpSpec v1.ControlPlaneSpec) (v1.ControlPlaneSpec, error) {
	log := common.LogFromContext(ctx)
	log.Info("updating servicemeshcontrolplane with templates")
	if smcpSpec.Template == "" {
		smcpSpec.Template = v1.DefaultTemplate
		log.Info("No template provided. Using default")
	}

	spec, err := r.recursivelyApplyTemplates(ctx, smcpSpec, smcpSpec.Version, sets.NewString())
	log.Info("finished updating ServiceMeshControlPlane", "Spec", spec)

	return spec, err
}

func (r *controlPlaneInstanceReconciler) validateSMCPSpec(spec v1.ControlPlaneSpec) error {
	if spec.Istio == nil {
		return fmt.Errorf("ServiceMeshControlPlane missing Istio section")
	}

	if _, ok := spec.Istio["global"].(map[string]interface{}); !ok {
		return fmt.Errorf("ServiceMeshControlPlane missing global section")
	}
	return nil
}

func (r *controlPlaneInstanceReconciler) renderCharts(ctx context.Context) error {
	log := common.LogFromContext(ctx)
	//Generate the spec
	r.Status.LastAppliedConfiguration = r.Instance.Spec
	if len(r.Status.LastAppliedConfiguration.Version) == 0 {
		// this must be from a 1.0 operator
		r.Status.LastAppliedConfiguration.Version = maistra.LegacyVersion.String()
	}

	spec, err := r.applyTemplates(ctx, r.Status.LastAppliedConfiguration)
	if err != nil {
		log.Error(err, "warning: failed to apply ServiceMeshControlPlane templates")

		return err
	}
	r.Status.LastAppliedConfiguration = spec

	if err := r.validateSMCPSpec(r.Status.LastAppliedConfiguration); err != nil {
		return err
	}

	if globalValues, ok := r.Status.LastAppliedConfiguration.Istio["global"].(map[string]interface{}); ok {
		globalValues["operatorNamespace"] = r.OperatorNamespace
	}

	var CNIValues map[string]interface{}
	var ok bool
	if CNIValues, ok = r.Status.LastAppliedConfiguration.Istio["istio_cni"].(map[string]interface{}); !ok {
		CNIValues = make(map[string]interface{})
		r.Status.LastAppliedConfiguration.Istio["istio_cni"] = CNIValues
	}
	CNIValues["enabled"] = r.cniConfig.Enabled
	CNIValues["istio_cni_network"], ok = common.GetCNINetworkName(r.Status.LastAppliedConfiguration.Version)
	if !ok {
		return fmt.Errorf("unknown maistra version: %s", r.Status.LastAppliedConfiguration.Version)
	}

	//Render the charts
	allErrors := []error{}
	var threeScaleRenderings map[string][]manifest.Manifest
	log.Info("rendering helm charts")
	log.V(2).Info("rendering Istio charts")
	istioRenderings, _, err := common.RenderHelmChart(path.Join(common.Options.GetChartsDir(r.Status.LastAppliedConfiguration.Version), "istio"), r.Instance.GetNamespace(), r.Status.LastAppliedConfiguration.Istio)
	if err != nil {
		allErrors = append(allErrors, err)
	}
	if isEnabled(r.Instance.Spec.ThreeScale) {
		log.V(2).Info("rendering 3scale charts")
		threeScaleRenderings, _, err = common.RenderHelmChart(path.Join(common.Options.GetChartsDir(r.Status.LastAppliedConfiguration.Version), "maistra-threescale"), r.Instance.GetNamespace(), r.Status.LastAppliedConfiguration.ThreeScale)
		if err != nil {
			allErrors = append(allErrors, err)
		}
	} else {
		threeScaleRenderings = map[string][]manifest.Manifest{}
	}

	if len(allErrors) > 0 {
		return utilerrors.NewAggregate(allErrors)
	}

	// merge the rendernings
	r.renderings = map[string][]manifest.Manifest{}
	for key, value := range istioRenderings {
		r.renderings[key] = value
	}
	for key, value := range threeScaleRenderings {
		r.renderings[key] = value
	}
	return nil
}

func (r *controlPlaneInstanceReconciler) PostStatus(ctx context.Context) error {
	log := common.LogFromContext(ctx)
	instance := &v1.ServiceMeshControlPlane{}
	log.Info("Posting status update", "conditions", r.Status.Conditions)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: r.Instance.Name, Namespace: r.Instance.Namespace}, instance); err == nil {
		instance.Status = *r.Status.DeepCopy()
		if err = r.Client.Status().Update(ctx, instance); err != nil && !(apierrors.IsGone(err) || apierrors.IsNotFound(err)) {
			return errors.Wrap(err, "error updating ServiceMeshControlPlane status")
		}
	} else if !(apierrors.IsGone(err) || apierrors.IsNotFound(err)) {
		return errors.Wrap(err, "error getting ServiceMeshControlPlane prior to updating status")
	}

	return nil
}

func (r *controlPlaneInstanceReconciler) postReconciliationStatus(ctx context.Context, reconciliationReason v1.ConditionReason, reconciliationMessage string, processingErr error) error {
	var reason string
	if r.isUpdating() {
		reason = eventReasonUpdating
	} else {
		reason = eventReasonInstalling
	}
	reconciledCondition := r.Status.GetCondition(v1.ConditionTypeReconciled)
	reconciledCondition.Reason = reconciliationReason
	if processingErr == nil {
		reconciledCondition.Message = reconciliationMessage
	} else {
		// grab the cause, as it's likely the error includes the reconciliation message
		reconciledCondition.Message = fmt.Sprintf("%s: error: %s", reconciliationMessage, errors.Cause(processingErr))
		r.EventRecorder.Event(r.Instance, corev1.EventTypeWarning, reason, reconciledCondition.Message)
	}
	r.Status.SetCondition(reconciledCondition)

	// we should only post status updates if condition status has changed
	if r.skipStatusUpdate() {
		return nil
	}

	return r.PostStatus(ctx)
}

func (r *controlPlaneInstanceReconciler) skipStatusUpdate() bool {
	// make sure we're using the same type in reflect.DeepEqual()
	var currentStatus, existingStatus *v1.ControlPlaneStatus
	currentStatus = r.Status
	existingStatus = &r.Instance.Status
	// only update status if it changed
	return reflect.DeepEqual(currentStatus, existingStatus)
}

func (r *controlPlaneInstanceReconciler) initializeReconcileStatus() {
	var readyMessage string
	var eventReason string
	var conditionReason v1.ConditionReason
	if r.isUpdating() {
		if r.Status.ObservedGeneration == r.Instance.GetGeneration() {
			fromVersion := r.Status.GetReconciledVersion()
			toVersion := v1.CurrentReconciledVersion(r.Instance.GetGeneration())
			readyMessage = fmt.Sprintf("Upgrading mesh from version %s to version %s", fromVersion[strings.LastIndex(fromVersion, "-")+1:], toVersion[strings.LastIndex(toVersion, "-")+1:])
		} else {
			readyMessage = fmt.Sprintf("Updating mesh from generation %d to generation %d", r.Status.ObservedGeneration, r.Instance.GetGeneration())
		}
		eventReason = eventReasonUpdating
		conditionReason = v1.ConditionReasonSpecUpdated
	} else {
		readyMessage = fmt.Sprintf("Installing mesh generation %d", r.Instance.GetGeneration())
		eventReason = eventReasonInstalling
		conditionReason = v1.ConditionReasonResourceCreated

		r.Status.SetCondition(v1.Condition{
			Type:    v1.ConditionTypeInstalled,
			Status:  v1.ConditionStatusFalse,
			Reason:  conditionReason,
			Message: readyMessage,
		})
	}
	r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReason, readyMessage)
	r.Status.SetCondition(v1.Condition{
		Type:    v1.ConditionTypeReconciled,
		Status:  v1.ConditionStatusFalse,
		Reason:  conditionReason,
		Message: readyMessage,
	})
	r.Status.SetCondition(v1.Condition{
		Type:    v1.ConditionTypeReady,
		Status:  v1.ConditionStatusFalse,
		Reason:  conditionReason,
		Message: readyMessage,
	})
}

func (r *controlPlaneInstanceReconciler) SetInstance(newInstance *v1.ServiceMeshControlPlane) {
	if newInstance.GetGeneration() != r.Instance.GetGeneration() {
		// we need to regenerate the renderings
		r.renderings = nil
		r.lastComponent = ""
		// reset reconcile status
		r.Status.SetCondition(v1.Condition{Type: v1.ConditionTypeReconciled, Status: v1.ConditionStatusUnknown})
	}
	r.Instance = newInstance
}

func (r *controlPlaneInstanceReconciler) IsFinished() bool {
	return r.Status.GetCondition(v1.ConditionTypeReconciled).Status == v1.ConditionStatusTrue
}

func componentFromChartName(chartName string) string {
	_, componentName := path.Split(chartName)
	return componentName
}

func isEnabled(spec v1.HelmValuesType) bool {
	if enabledVal, ok := spec["enabled"]; ok {
		if enabled, ok := enabledVal.(bool); ok {
			return enabled
		}
	}
	return false
}
