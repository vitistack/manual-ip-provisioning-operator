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

package v1alpha2

import (
	"context"
	"strings"

	vitistackcrdsv1alpha2 "github.com/vitistack/common/pkg/v1alpha2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	phaseReady = "Ready"
	phaseError = "Error"

	reasonSynced  = "Synced"
	reasonInvalid = "InvalidSpec"
)

// NetworkNamespaceReconciler provisions NetworkNamespaces whose
// networkProvisioning.provider is "manual". It copies the user-supplied network
// segment (CIDR/VLAN/IPv6) from spec.networkProvisioning.manual into status and
// marks provisioningPhase=Ready, so IP-allocation operators (e.g. static-ip) can
// proceed. This mirrors what nms-operator does for the "nam" provider — both
// write the provisioned segment into status; only the source differs.
type NetworkNamespaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func NewNetworkNamespaceReconciler(c client.Client, scheme *runtime.Scheme) *NetworkNamespaceReconciler {
	return &NetworkNamespaceReconciler{Client: c, Scheme: scheme}
}

// +kubebuilder:rbac:groups=vitistack.io,resources=networknamespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=vitistack.io,resources=networknamespaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vitistack.io,resources=networknamespaces/finalizers,verbs=update

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkNamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vitistackcrdsv1alpha2.NetworkNamespace{}).
		Named("manual-ip-provisioning").
		Complete(r)
}

// provisioningProvider returns the network provisioning provider for the object.
// When networkProvisioning is unset the provider defaults to "nam" (owned by
// nms-operator), so this operator ignores it.
func provisioningProvider(nn *vitistackcrdsv1alpha2.NetworkNamespace) vitistackcrdsv1alpha2.NetworkProvisioningType {
	if nn.Spec.NetworkProvisioning != nil && nn.Spec.NetworkProvisioning.Provider != "" {
		return nn.Spec.NetworkProvisioning.Provider
	}
	return vitistackcrdsv1alpha2.NetworkProvisioningNAM
}

// Reconcile copies the manual network segment from spec to status and marks the
// NetworkNamespace as provisioned.
func (r *NetworkNamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var nn vitistackcrdsv1alpha2.NetworkNamespace
	if err := r.Get(ctx, req.NamespacedName, &nn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only act on manually-provisioned NetworkNamespaces; everything else
	// (nam, other providers) is owned by another operator.
	if provisioningProvider(&nn) != vitistackcrdsv1alpha2.NetworkProvisioningManual {
		return ctrl.Result{}, nil
	}

	// Manual provisioning creates no external resources, so deletion needs no
	// cleanup and we add no finalizer.
	if nn.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, nil
	}

	// Skip if already in Error and the spec hasn't changed, to avoid log spam.
	if nn.Status.Phase == phaseError && nn.Status.ObservedGeneration >= nn.GetGeneration() {
		return ctrl.Result{}, nil
	}

	manual := nn.Spec.NetworkProvisioning.Manual
	if manual == nil || strings.TrimSpace(manual.IPv4CIDR) == "" {
		logger.Info("manual provisioning missing ipv4CIDR", "networkNamespace", nn.Name)
		_ = r.setStatus(ctx, &nn, vitistackcrdsv1alpha2.NetworkNamespaceStatus{
			Phase:              phaseError,
			Status:             reasonInvalid,
			Message:            "manual provisioning requires networkProvisioning.manual.ipv4CIDR to be set",
			ObservedGeneration: nn.GetGeneration(),
		})
		return ctrl.Result{}, nil
	}

	// Preserve the Created timestamp from the first successful reconciliation.
	createdTimestamp := nn.Status.Created
	if createdTimestamp.IsZero() {
		createdTimestamp = metav1.Now()
	}

	// Log only on the transition into Ready, not on every steady-state reconcile.
	if nn.Status.Phase != phaseReady {
		logger.Info("manual provisioning: copying network config to status",
			"networkNamespace", nn.Name, "ipv4Prefix", manual.IPv4CIDR, "vlanId", manual.VlanID)
	}

	_ = r.setStatus(ctx, &nn, vitistackcrdsv1alpha2.NetworkNamespaceStatus{
		Phase:              phaseReady,
		Status:             reasonSynced,
		Message:            "NetworkNamespace manually provisioned",
		Created:            createdTimestamp,
		ObservedGeneration: nn.GetGeneration(),
		ProvisioningPhase:  vitistackcrdsv1alpha2.ProvisioningPhaseReady,
		IPv4Prefix:         strings.TrimSpace(manual.IPv4CIDR),
		IPv6Prefix:         strings.TrimSpace(manual.IPv6CIDR),
		VlanID:             manual.VlanID,
	})
	return ctrl.Result{}, nil
}

// setStatus updates the NetworkNamespace status, deriving provisioningPhase from
// phase, managing the Ready condition, and skipping no-op writes.
func (r *NetworkNamespaceReconciler) setStatus(
	ctx context.Context,
	nn *vitistackcrdsv1alpha2.NetworkNamespace,
	newStatus vitistackcrdsv1alpha2.NetworkNamespaceStatus,
) error {
	if newStatus.ProvisioningPhase == "" {
		switch newStatus.Phase {
		case phaseReady:
			newStatus.ProvisioningPhase = vitistackcrdsv1alpha2.ProvisioningPhaseReady
		case phaseError:
			newStatus.ProvisioningPhase = vitistackcrdsv1alpha2.ProvisioningPhaseError
		default:
			newStatus.ProvisioningPhase = vitistackcrdsv1alpha2.ProvisioningPhasePending
		}
	}

	if statusEqual(nn.Status, newStatus) {
		return nil
	}

	if newStatus.Phase != "" {
		newStatus.Conditions = upsertCondition(nn.Status.Conditions,
			buildReadyCondition(newStatus.Phase, newStatus.Message, nn.GetGeneration()))
	}

	nn.Status = newStatus
	return r.Status().Update(ctx, nn)
}

// statusEqual reports whether the relevant status fields are unchanged
// (Conditions are managed separately and carry timestamps).
func statusEqual(oldStatus, newStatus vitistackcrdsv1alpha2.NetworkNamespaceStatus) bool {
	return oldStatus.Phase == newStatus.Phase &&
		oldStatus.Status == newStatus.Status &&
		oldStatus.Message == newStatus.Message &&
		oldStatus.Created.Equal(&newStatus.Created) &&
		oldStatus.ObservedGeneration == newStatus.ObservedGeneration &&
		oldStatus.ProvisioningPhase == newStatus.ProvisioningPhase &&
		oldStatus.IPv4Prefix == newStatus.IPv4Prefix &&
		oldStatus.IPv6Prefix == newStatus.IPv6Prefix &&
		oldStatus.VlanID == newStatus.VlanID
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

// upsertCondition adds or updates a condition, only bumping LastTransitionTime
// when the status/reason/message actually changes.
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
