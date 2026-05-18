/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"encoding/json"
	"strings"

	vitistackcrdsv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	phaseReady   = "Ready"
	reasonSynced = "Synced"
	reasonError  = "Error"

	provisioningPhaseReady   = "Ready"
	provisioningPhaseError   = "Error"
	provisioningPhasePending = "Pending"
)

// NetworkNamespaceReconciler reconciles NetworkNamespace resources that use
// manual provisioning. It copies user-supplied network parameters from spec
// to status and sets provisioningPhase=Ready so downstream operators can
// proceed without waiting for a NAM response.
type NetworkNamespaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func NewNetworkNamespaceReconciler(c client.Client, scheme *runtime.Scheme) *NetworkNamespaceReconciler {
	return &NetworkNamespaceReconciler{
		Client: c,
		Scheme: scheme,
	}
}

// +kubebuilder:rbac:groups=vitistack.io,resources=networknamespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=vitistack.io,resources=networknamespaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vitistack.io,resources=networknamespaces/finalizers,verbs=update

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkNamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vitistackcrdsv1alpha1.NetworkNamespace{}).
		Named("manual-ip-provisioning").
		Complete(r)
}

func (r *NetworkNamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var nn vitistackcrdsv1alpha1.NetworkNamespace
	if err := r.Get(ctx, req.NamespacedName, &nn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only act on NetworkNamespaces with manual provisioning.
	// Detection: v1alpha1 uses ipAllocation.provider="static-ip-operator" to
	// indicate manual provisioning; v1alpha2 sets networkProvisioning.provider="manual"
	// which is converted to the same v1alpha1 representation.
	if !isManualProvisioning(&nn) {
		// Not our responsibility — skip silently.
		return ctrl.Result{}, nil
	}

	// Deleting — nothing to clean up for manual provisioning.
	if nn.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, nil
	}

	// Skip if already in Error and spec hasn't changed.
	if nn.Status.Phase == reasonError &&
		nn.Status.ObservedGeneration >= nn.GetGeneration() {
		return ctrl.Result{}, nil
	}

	// Extract manual network config from ipAllocation.static (v1alpha1 representation).
	var ipv4Prefix, ipv4Gateway string
	var vlanID int
	if nn.Spec.IPAllocation != nil && nn.Spec.IPAllocation.Static != nil {
		ipv4Prefix = nn.Spec.IPAllocation.Static.IPv4CIDR
		ipv4Gateway = nn.Spec.IPAllocation.Static.IPv4Gateway
		vlanID = nn.Spec.IPAllocation.Static.VlanID
	}

	// Validate required fields.
	if ipv4Prefix == "" {
		logger.Info("manual provisioning missing ipv4CIDR",
			"networkNamespace", nn.Name)
		_ = r.setStatus(ctx, &nn, vitistackcrdsv1alpha1.NetworkNamespaceStatus{
			Phase:              reasonError,
			Status:             "InvalidSpec",
			Message:            "manual provisioning requires ipAllocation.static.ipv4CIDR to be set",
			ObservedGeneration: nn.GetGeneration(),
		})
		return ctrl.Result{}, nil
	}

	dc := strings.TrimSpace(nn.Spec.DatacenterIdentifier)
	sv := strings.TrimSpace(nn.Spec.SupervisorIdentifier)

	// Preserve the Created timestamp from the first successful reconciliation.
	createdTimestamp := nn.Status.Created
	if createdTimestamp.IsZero() {
		createdTimestamp = metav1.Now()
	}

	logger.Info("manual provisioning: copying spec to status",
		"networkNamespace", nn.Name, "ipv4Prefix", ipv4Prefix,
		"vlanId", vlanID, "gateway", ipv4Gateway)

	_ = r.setStatus(ctx, &nn, vitistackcrdsv1alpha1.NetworkNamespaceStatus{
		Phase:                phaseReady,
		Status:               reasonSynced,
		Message:              "NetworkNamespace manually provisioned",
		Created:              createdTimestamp,
		ObservedGeneration:   nn.GetGeneration(),
		DataCenterIdentifier: dc,
		SupervisorIdentifier: sv,
		IPv4Prefix:           ipv4Prefix,
		VlanID:               vlanID,
	})
	return ctrl.Result{}, nil
}

// isManualProvisioning returns true if the NetworkNamespace should be handled
// by the manual-ip-provisioning-operator rather than by nms-operator.
func isManualProvisioning(nn *vitistackcrdsv1alpha1.NetworkNamespace) bool {
	if nn.Spec.IPAllocation == nil {
		return false
	}
	if !vitistackcrdsv1alpha1.IsProviderSet(nn.Spec.IPAllocation.Provider) {
		return false
	}
	return vitistackcrdsv1alpha1.MatchesProvider(
		nn.Spec.IPAllocation.Provider,
		vitistackcrdsv1alpha1.ProviderNameStaticIP,
	)
}

// setStatus updates the NetworkNamespace status with the provided fields.
// It derives provisioningPhase from Phase and writes it via a JSON merge patch
// since provisioningPhase is a v1alpha2 field not present on the v1alpha1 Go struct.
func (r *NetworkNamespaceReconciler) setStatus(
	ctx context.Context,
	nn *vitistackcrdsv1alpha1.NetworkNamespace,
	newStatus vitistackcrdsv1alpha1.NetworkNamespaceStatus,
) error {
	// Derive provisioningPhase from phase.
	provisioningPhase := provisioningPhasePending
	switch newStatus.Phase {
	case phaseReady:
		provisioningPhase = provisioningPhaseReady
	case reasonError:
		provisioningPhase = provisioningPhaseError
	}

	// Skip no-op updates.
	if statusEqual(nn.Status, newStatus) {
		return nil
	}

	// Build and upsert Ready condition based on phase.
	if newStatus.Phase != "" {
		condition := buildReadyCondition(newStatus.Phase, newStatus.Message, nn.GetGeneration())
		newStatus.Conditions = upsertCondition(nn.Status.Conditions, condition)
	}

	// Use a merge patch so we can write provisioningPhase (a v1alpha2 field)
	// alongside the typed v1alpha1 status fields.
	patch := map[string]interface{}{
		"status": map[string]interface{}{
			"phase":                newStatus.Phase,
			"status":               newStatus.Status,
			"message":              newStatus.Message,
			"created":              newStatus.Created,
			"observedGeneration":   newStatus.ObservedGeneration,
			"dataCenterIdentifier": newStatus.DataCenterIdentifier,
			"supervisorIdentifier": newStatus.SupervisorIdentifier,
			"ipv4Prefix":           newStatus.IPv4Prefix,
			"vlanId":               newStatus.VlanID,
			"provisioningPhase":    provisioningPhase,
			"conditions":           newStatus.Conditions,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return r.Status().Patch(ctx, nn, client.RawPatch(types.MergePatchType, patchBytes))
}

// statusEqual returns true if the relevant status fields are unchanged.
func statusEqual(old, new vitistackcrdsv1alpha1.NetworkNamespaceStatus) bool {
	return old.Phase == new.Phase &&
		old.Status == new.Status &&
		old.Message == new.Message &&
		old.Created.Equal(&new.Created) &&
		old.ObservedGeneration == new.ObservedGeneration &&
		old.DataCenterIdentifier == new.DataCenterIdentifier &&
		old.SupervisorIdentifier == new.SupervisorIdentifier &&
		old.IPv4Prefix == new.IPv4Prefix &&
		old.VlanID == new.VlanID
}

// buildReadyCondition creates a Ready condition based on the phase.
func buildReadyCondition(phase, message string, generation int64) metav1.Condition {
	c := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionUnknown,
		Reason:             "Reconciling",
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: generation,
	}
	if message == "" {
		c.Message = "Reconciling resource"
	}
	switch strings.ToLower(phase) {
	case "ready", "available", "synced":
		c.Status = metav1.ConditionTrue
		c.Reason = "Synced"
		if message == "" {
			c.Message = "Resource is in Ready state"
		}
	case "error", "failed":
		c.Status = metav1.ConditionFalse
		c.Reason = "Error"
		if message == "" {
			c.Message = "Reconciliation failed"
		}
	}
	return c
}

// upsertCondition adds or updates a condition in the conditions slice.
func upsertCondition(conditions []metav1.Condition, cond metav1.Condition) []metav1.Condition {
	for i, existing := range conditions {
		if existing.Type == cond.Type {
			if existing.Status != cond.Status ||
				existing.Reason != cond.Reason ||
				existing.Message != cond.Message {
				cond.LastTransitionTime = metav1.Now()
				conditions[i] = cond
			}
			return conditions
		}
	}
	return append(conditions, cond)
}
