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

	epiniov1alpha1 "github.com/epinio/install-operator/api/v1alpha1"
)

func TestInstallFailureMessageFromContainerStatuses(t *testing.T) {
	t.Parallel()

	if got := installFailureMessageFromContainerStatuses(nil); got != "" {
		t.Fatalf("empty statuses: got %q", got)
	}
	msg := installFailureMessageFromContainerStatuses([]corev1.ContainerStatus{
		{Name: "a", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "helm oops"}}},
	})
	if msg != "Install failed: helm oops" {
		t.Fatalf("message: got %q", msg)
	}
}

func TestInstallFailureMessageFromPodInitBeforeMain(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "init", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "init pull failed"}}},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "main never ran"}}},
			},
		},
	}
	if got := installFailureMessageFromPod(pod); got != "Install failed: init pull failed" {
		t.Fatalf("expected init message first, got %q", got)
	}
}

func TestJobFailureMessagePrefersNewestPod(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	oldTime := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	newTime := metav1.NewTime(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))

	objs := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "old", Namespace: "system", Labels: map[string]string{"job-name": "thejob"},
				CreationTimestamp: oldTime,
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "from older pod"}}},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "new", Namespace: "system", Labels: map[string]string{"job-name": "thejob"},
				CreationTimestamp: newTime,
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "from newer pod"}}},
				},
			},
		},
	}

	r := &InstallEpinioReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
	}
	if got := r.jobFailureMessage(ctx, "system", "thejob"); got != "Install failed: from newer pod" {
		t.Fatalf("jobFailureMessage: got %q", got)
	}
}

func TestJobTerminalHelpers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   batchv1.JobStatus
		wantFail bool
		wantOK   bool
	}{
		{
			name:     "failed counter",
			status:   batchv1.JobStatus{Failed: 1},
			wantFail: true,
		},
		{
			name: "failed condition",
			status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
				},
			},
			wantFail: true,
		},
		{
			name:   "succeeded counter only",
			status: batchv1.JobStatus{Succeeded: 1},
			wantOK: true,
		},
		{
			name: "complete condition",
			status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
			wantOK: true,
		},
		{
			name:     "failed counter wins over succeeded counter",
			status:   batchv1.JobStatus{Succeeded: 1, Failed: 1},
			wantFail: true,
		},
		{
			name: "failed condition wins over succeeded counter",
			status: batchv1.JobStatus{
				Succeeded: 1,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
				},
			},
			wantFail: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := jobTerminalFailed(tt.status); got != tt.wantFail {
				t.Fatalf("jobTerminalFailed() = %v, want %v", got, tt.wantFail)
			}
			if got := jobTerminalSucceeded(tt.status); got != tt.wantOK {
				t.Fatalf("jobTerminalSucceeded() = %v, want %v", got, tt.wantOK)
			}
		})
	}
}

func TestReconcileMarksDegradedWhenJobFailed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	jobName := installJobName("system", "test-resource")
	cmName := configMapName("system", "test-resource")
	now := metav1.Now()
	reconciler, k8sClient := newUnitTestReconciler(t,
		nil,
		nil,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "system"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: "system"}},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "system"},
			Status: batchv1.JobStatus{
				StartTime: &now,
				Failed:    1,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now, Reason: "BackoffLimitExceeded"},
				},
			},
		},
		&epiniov1alpha1.InstallEpinio{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-resource",
				Namespace:  "system",
				Finalizers: []string{installCleanupFinalizer},
			},
			Spec: epiniov1alpha1.InstallEpinioSpec{
				Domain:          "demo.example.test",
				TargetNamespace: "epinio",
				Version:         "1.0.0",
			},
		},
	)

	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: "test-resource", Namespace: "system"}}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var install epiniov1alpha1.InstallEpinio
	if err := k8sClient.Get(ctx, req.NamespacedName, &install); err != nil {
		t.Fatalf("get install: %v", err)
	}
	cond := meta.FindStatusCondition(install.Status.Conditions, conditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "InstallFailed" {
		t.Fatalf("expected Degraded InstallFailed condition, got %+v", cond)
	}
	if meta.FindStatusCondition(install.Status.Conditions, conditionProgressing) != nil {
		t.Fatalf("expected Progressing removed when degraded")
	}

	if err := k8sClient.Get(ctx, client.ObjectKey{Name: jobName, Namespace: "system"}, &batchv1.Job{}); client.IgnoreNotFound(err) != nil {
		t.Fatalf("get job: %v", err)
	} else if err == nil {
		t.Fatalf("expected install job to be deleted after terminal failure")
	}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: cmName, Namespace: "system"}, &corev1.ConfigMap{}); client.IgnoreNotFound(err) != nil {
		t.Fatalf("get configmap: %v", err)
	} else if err == nil {
		t.Fatalf("expected install script configmap to be deleted after terminal failure")
	}
}

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

func TestReconcileTerminalStateCleansArtifactsAndDoesNotRecreateJob(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	jobName := installJobName("system", "demo")
	cmName := configMapName("system", "demo")
	reconciler, k8sClient := newUnitTestReconciler(t,
		nil,
		nil,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "system"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: "system"}},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "system"},
			Status:     batchv1.JobStatus{Succeeded: 1},
		},
		&epiniov1alpha1.InstallEpinio{
			ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "system"},
			Spec: epiniov1alpha1.InstallEpinioSpec{
				Domain:          "demo.example.test",
				TargetNamespace: "epinio",
			},
		},
	)

	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: "demo", Namespace: "system"}}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile should only add the finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile should process succeeded job: %v", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKey{Name: jobName, Namespace: "system"}, &batchv1.Job{}); client.IgnoreNotFound(err) != nil {
		t.Fatalf("get cleaned job: %v", err)
	} else if err == nil {
		t.Fatalf("expected install job to be deleted after terminal state")
	}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: cmName, Namespace: "system"}, &corev1.ConfigMap{}); client.IgnoreNotFound(err) != nil {
		t.Fatalf("get cleaned configmap: %v", err)
	} else if err == nil {
		t.Fatalf("expected install script configmap to be deleted after terminal state")
	}

	// Third reconcile should not recreate a new installer Job for the same generation.
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("third reconcile should be a terminal no-op: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: jobName, Namespace: "system"}, &batchv1.Job{}); client.IgnoreNotFound(err) != nil {
		t.Fatalf("get job after terminal no-op: %v", err)
	} else if err == nil {
		t.Fatalf("expected no job recreation once install is terminal for current generation")
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
		WithStatusSubresource(&epiniov1alpha1.InstallEpinio{}, &batchv1.Job{}).
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
