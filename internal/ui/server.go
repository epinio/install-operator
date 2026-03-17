package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	epiniov1alpha1 "apps.example.com/install-epinio/api/v1alpha1"
)

type Server struct {
	client              client.Client
	controllerNamespace string
}

type installRequest struct {
	Domain          string `json:"domain"`
	TargetNamespace string `json:"targetNamespace"`
	Version         string `json:"version"`
}

type installResponse struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	Domain          string `json:"domain"`
	TargetNamespace string `json:"targetNamespace"`
	Version         string `json:"version,omitempty"`
	Message         string `json:"message"`
}

func NewServer(k8sClient client.Client, controllerNamespace string) (*ServerWithHTTP, error) {
	mux := http.NewServeMux()
	s := &Server{
		client:              k8sClient,
		controllerNamespace: controllerNamespace,
	}
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/api/install", s.handleInstall)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return &ServerWithHTTP{Server: s, httpServer: srv}, nil
}

type ServerWithHTTP struct {
	*Server
	httpServer *http.Server
}

func (s *ServerWithHTTP) ListenAndServe(bindAddress string) error {
	s.httpServer.Addr = bindAddress
	if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *ServerWithHTTP) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	logger := log.FromContext(ctx).WithName("ui")

	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	req.Domain = strings.TrimSpace(req.Domain)
	if req.Domain == "" {
		req.Domain = "epinio.local"
	}

	req.TargetNamespace = strings.TrimSpace(req.TargetNamespace)
	if req.TargetNamespace == "" {
		req.TargetNamespace = "epinio"
	}

	req.Version = strings.TrimSpace(req.Version)

	existing, err := s.findActiveInstall(ctx, req.TargetNamespace)
	if err != nil {
		http.Error(w, "failed to inspect current installs", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusConflict, installResponse{
			Name:            existing.Name,
			Namespace:       existing.Namespace,
			Domain:          existing.Spec.Domain,
			TargetNamespace: existing.Spec.TargetNamespace,
			Version:         existing.Spec.Version,
			Message:         "An install for this target namespace is already in progress or completed",
		})
		return
	}

	resource := &epiniov1alpha1.InstallEpinio{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "install-epinio-",
			Namespace:    s.controllerNamespace,
		},
		Spec: epiniov1alpha1.InstallEpinioSpec{
			Domain:          req.Domain,
			TargetNamespace: req.TargetNamespace,
			Version:         req.Version,
		},
	}
	if err := s.client.Create(ctx, resource); err != nil {
		http.Error(w, "failed to create install request", http.StatusInternalServerError)
		return
	}

	logger.Info("Created InstallEpinio resource from UI", "name", resource.Name, "namespace", resource.Namespace)
	writeJSON(w, http.StatusAccepted, installResponse{
		Name:            resource.Name,
		Namespace:       resource.Namespace,
		Domain:          resource.Spec.Domain,
		TargetNamespace: resource.Spec.TargetNamespace,
		Version:         resource.Spec.Version,
		Message:         "Install request accepted. The operator is now reconciling it.",
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, "Epinio install API is running. Demo UI is in the repository under demo/index.html.")
}

func (s *Server) findActiveInstall(ctx context.Context, targetNamespace string) (*epiniov1alpha1.InstallEpinio, error) {
	var installs epiniov1alpha1.InstallEpinioList
	if err := s.client.List(ctx, &installs, client.InNamespace(s.controllerNamespace)); err != nil {
		return nil, err
	}

	for i := range installs.Items {
		item := &installs.Items[i]
		if !strings.EqualFold(item.Spec.TargetNamespace, targetNamespace) {
			continue
		}
		available := findCondition(item.Status.Conditions, "Available")
		progressing := findCondition(item.Status.Conditions, "Progressing")
		if conditionTrue(available) || conditionTrue(progressing) {
			return item, nil
		}
	}

	return nil, nil
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func conditionTrue(condition *metav1.Condition) bool {
	return condition != nil && condition.Status == metav1.ConditionTrue
}

func writeJSON(w http.ResponseWriter, statusCode int, payload installResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func WaitForInstall(ctx context.Context, k8sClient client.Client, namespace, name string) (*epiniov1alpha1.InstallEpinio, error) {
	var install epiniov1alpha1.InstallEpinio
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &install); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &install, nil
}
