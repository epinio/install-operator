package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	epiniov1alpha1 "apps.example.com/install-epinio/api/v1alpha1"
)

func TestReconcileSetsDegradedConditionWhenJobCreationFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	jobCreateErr := errors.New("job create boom")
	reconciler, k8sClient := newUnitTestReconciler(t,
		jobCreateErr,
		nil,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "system"}},
		&epiniov1alpha1.InstallEpinio{
			ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "system"},
			Spec: epiniov1alpha1.InstallEpinioSpec{
				Domain:          "demo.example.test",
				TargetNamespace: "epinio",
				Version:         "1.2.3",
			},
		},
	)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "demo", Namespace: "system"}})
	if err != nil {
		t.Fatalf("first reconcile should only add the finalizer: %v", err)
	}

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "demo", Namespace: "system"}})
	if !errors.Is(err, jobCreateErr) {
		t.Fatalf("expected job creation error, got %v", err)
	}

	var install epiniov1alpha1.InstallEpinio
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "demo", Namespace: "system"}, &install); err != nil {
		t.Fatalf("get install: %v", err)
	}

	condition := meta.FindStatusCondition(install.Status.Conditions, conditionDegraded)
	if condition == nil {
		t.Fatalf("expected degraded condition after job creation failure")
	}
	if condition.Reason != "JobCreationFailed" {
		t.Fatalf("unexpected degraded reason: %s", condition.Reason)
	}
}

func TestReconcileReturnsJoinedErrorWhenStatusUpdateFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	jobCreateErr := errors.New("job create boom")
	statusUpdateErr := errors.New("status update boom")
	reconciler, _ := newUnitTestReconciler(t,
		jobCreateErr,
		statusUpdateErr,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "system"}},
		&epiniov1alpha1.InstallEpinio{
			ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "system"},
			Spec: epiniov1alpha1.InstallEpinioSpec{
				Domain:          "demo.example.test",
				TargetNamespace: "epinio",
			},
		},
	)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "demo", Namespace: "system"}})
	if err != nil {
		t.Fatalf("first reconcile should only add the finalizer: %v", err)
	}

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "demo", Namespace: "system"}})
	if !errors.Is(err, jobCreateErr) {
		t.Fatalf("expected joined error containing job creation failure, got %v", err)
	}
	if !errors.Is(err, statusUpdateErr) {
		t.Fatalf("expected joined error containing status update failure, got %v", err)
	}
	if err == nil || err.Error() != "job create boom\nstatus update boom" {
		t.Fatalf("expected joined error with status update failure, got %v", err)
	}
}

func TestReconcileDeleteCleansCrossNamespaceResources(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := metav1.NewTime(time.Now())
	reconciler, k8sClient := newUnitTestReconciler(t,
		nil,
		nil,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "system"}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: installJobName("default", "demo"), Namespace: "system"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configMapName("default", "demo"), Namespace: "system"}},
		&epiniov1alpha1.InstallEpinio{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "demo",
				Namespace:         "default",
				Finalizers:        []string{installCleanupFinalizer},
				DeletionTimestamp: &now,
			},
		},
	)

	var install epiniov1alpha1.InstallEpinio
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "demo", Namespace: "default"}, &install); err != nil {
		t.Fatalf("get install: %v", err)
	}

	if _, err := reconciler.reconcileDelete(ctx, &install, "system"); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKey{Name: installJobName("default", "demo"), Namespace: "system"}, &batchv1.Job{}); client.IgnoreNotFound(err) != nil {
		t.Fatalf("get cleaned job: %v", err)
	} else if err == nil {
		t.Fatalf("expected install job to be deleted during cleanup")
	}

	if err := k8sClient.Get(ctx, client.ObjectKey{Name: configMapName("default", "demo"), Namespace: "system"}, &corev1.ConfigMap{}); client.IgnoreNotFound(err) != nil {
		t.Fatalf("get cleaned configmap: %v", err)
	} else if err == nil {
		t.Fatalf("expected configmap to be deleted during cleanup")
	}
}

type interceptingClient struct {
	client.Client
	createJobErr    error
	statusUpdateErr error
}

func (c *interceptingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.createJobErr != nil {
		if _, ok := obj.(*batchv1.Job); ok {
			return c.createJobErr
		}
	}

	return c.Client.Create(ctx, obj, opts...)
}

func (c *interceptingClient) Status() client.SubResourceWriter {
	return &interceptingStatusWriter{
		SubResourceWriter: c.Client.Status(),
		updateErr:         c.statusUpdateErr,
	}
}

type interceptingStatusWriter struct {
	client.SubResourceWriter
	updateErr error
}

func (w *interceptingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if w.updateErr != nil {
		return w.updateErr
	}

	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func newUnitTestReconciler(t *testing.T, createJobErr, statusUpdateErr error, objects ...client.Object) (*InstallEpinioReconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := epiniov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add epinio scheme: %v", err)
	}

	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&epiniov1alpha1.InstallEpinio{}).
		WithObjects(objects...).
		Build()

	k8sClient := &interceptingClient{
		Client:          baseClient,
		createJobErr:    createJobErr,
		statusUpdateErr: statusUpdateErr,
	}

	return &InstallEpinioReconciler{
		Client:                  k8sClient,
		Scheme:                  scheme,
		ControllerNamespace:     "system",
		InstallerServiceAccount: "installer-sa",
		InstallerHelmImage:      "example.com/helm:test",
	}, k8sClient
}
