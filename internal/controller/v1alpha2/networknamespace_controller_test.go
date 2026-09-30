package v1alpha2

import (
	"context"
	"testing"

	vitistackcrdsv1alpha2 "github.com/vitistack/common/pkg/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	nnName    = "test-nn"
	nnNS      = "default"
	testCIDR  = "192.168.10.0/24"
	testVlan  = 30
	testIPv6  = "2001:db8:10::/64"
	manualKey = "manual"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := vitistackcrdsv1alpha2.AddToScheme(s); err != nil {
		t.Fatalf("add v1alpha2 to scheme: %v", err)
	}
	return s
}

func req() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: nnName, Namespace: nnNS}}
}

func nnWith(provider string, manual *vitistackcrdsv1alpha2.ManualProvisioningConfig) *vitistackcrdsv1alpha2.NetworkNamespace {
	return &vitistackcrdsv1alpha2.NetworkNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: nnName, Namespace: nnNS},
		Spec: vitistackcrdsv1alpha2.NetworkNamespaceSpec{
			NetworkProvisioning: &vitistackcrdsv1alpha2.NetworkProvisioning{
				Provider: vitistackcrdsv1alpha2.NetworkProvisioningType(provider),
				Manual:   manual,
			},
		},
	}
}

func reconcileAndGet(t *testing.T, nn *vitistackcrdsv1alpha2.NetworkNamespace) vitistackcrdsv1alpha2.NetworkNamespace {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(nn).WithStatusSubresource(nn).Build()
	r := NewNetworkNamespaceReconciler(c, s)
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	var got vitistackcrdsv1alpha2.NetworkNamespace
	if err := c.Get(context.Background(), req().NamespacedName, &got); err != nil {
		t.Fatalf("get error: %v", err)
	}
	return got
}

func TestReconcile_ManualProvisionsToStatus(t *testing.T) {
	got := reconcileAndGet(t, nnWith(manualKey, &vitistackcrdsv1alpha2.ManualProvisioningConfig{
		IPv4CIDR: testCIDR,
		IPv6CIDR: testIPv6,
		VlanID:   testVlan,
	}))

	if got.Status.Phase != phaseReady {
		t.Errorf("Phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.ProvisioningPhase != vitistackcrdsv1alpha2.ProvisioningPhaseReady {
		t.Errorf("ProvisioningPhase = %q, want Ready", got.Status.ProvisioningPhase)
	}
	if got.Status.IPv4Prefix != testCIDR {
		t.Errorf("IPv4Prefix = %q, want %q", got.Status.IPv4Prefix, testCIDR)
	}
	if got.Status.IPv6Prefix != testIPv6 {
		t.Errorf("IPv6Prefix = %q, want %q", got.Status.IPv6Prefix, testIPv6)
	}
	if got.Status.VlanID != testVlan {
		t.Errorf("VlanID = %d, want %d", got.Status.VlanID, testVlan)
	}
}

func TestReconcile_SkipsNonManualProvider(t *testing.T) {
	got := reconcileAndGet(t, nnWith("nam", nil))
	if got.Status.Phase != "" {
		t.Errorf("nam-provisioned NN must not be touched, got Phase = %q", got.Status.Phase)
	}
}

func TestReconcile_ManualMissingCIDRErrors(t *testing.T) {
	got := reconcileAndGet(t, nnWith(manualKey, nil))
	if got.Status.Phase != phaseError {
		t.Errorf("Phase = %q, want Error", got.Status.Phase)
	}
	if got.Status.Status != reasonInvalid {
		t.Errorf("Status = %q, want %q", got.Status.Status, reasonInvalid)
	}
}
