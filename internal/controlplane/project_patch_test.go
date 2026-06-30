package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// createAPIHandler creates an apiHandler backed by a fake K8s client.
func createAPIHandler(objs ...client.Object) *apiHandler {
	scheme := createTestScheme()
	logger := createTestLogger()

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()

	svc := &apiService{
		log:       logger,
		k8sClient: k8sClient,
		namespace: "neon",
	}

	return &apiHandler{svc: svc, log: logger}
}

func TestPatchProject_NameUpdate(t *testing.T) {
	project := &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-project",
			Namespace: "neon",
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        "Old Name",
			PGVersion:   16,
			ClusterName: "default",
			TenantID:    "deadbeef-cafe-babe-1234-567890abcdef",
		},
	}

	handler := createAPIHandler(project)

	body := `{"project":{"name":"New Name"}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("project_id", "my-project")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	proj := resp["project"].(map[string]interface{})
	if proj["name"] != "New Name" {
		t.Errorf("expected name 'New Name', got '%v'", proj["name"])
	}
}

func TestPatchProject_HistoryRetentionSeconds(t *testing.T) {
	project := &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-project",
			Namespace: "neon",
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        "Test Project",
			PGVersion:   16,
			ClusterName: "default",
			TenantID:    "deadbeef-cafe-babe-1234-567890abcdef",
		},
	}

	handler := createAPIHandler(project)

	body := `{"project":{"history_retention_seconds":2592000}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	proj := resp["project"].(map[string]interface{})
	if proj["history_retention_seconds"] != float64(2592000) {
		t.Errorf("expected 2592000, got %v", proj["history_retention_seconds"])
	}
}

func TestPatchProject_InvalidHistoryRetention(t *testing.T) {
	project := &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-project",
			Namespace: "neon",
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        "Test Project",
			PGVersion:   16,
			ClusterName: "default",
		},
	}

	handler := createAPIHandler(project)

	body := `{"project":{"history_retention_seconds":-1}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchProject_IPAllowUpsert(t *testing.T) {
	project := &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-project",
			Namespace: "neon",
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        "Test Project",
			PGVersion:   16,
			ClusterName: "default",
		},
	}

	handler := createAPIHandler(project)

	body := `{"project":{"ip_allow":{"primary_branch_only":true,"source_ranges":["10.0.0.0/8"]}}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	proj := resp["project"].(map[string]interface{})
	ipAllow := proj["ip_allow"].(map[string]interface{})
	if ipAllow["primary_branch_only"] != true {
		t.Errorf("expected primary_branch_only=true, got %v", ipAllow["primary_branch_only"])
	}
}

func TestPatchProject_IPAllowRemove(t *testing.T) {
	project := &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-project",
			Namespace: "neon",
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        "Test Project",
			PGVersion:   16,
			ClusterName: "default",
			IPAllow: &neonv1alpha1.IPAllowConfig{
				PrimaryBranchOnly: true,
				SourceRanges:      []string{"10.0.0.0/8"},
			},
		},
	}

	handler := createAPIHandler(project)

	body := `{"project":{"ip_allow":null}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	proj := resp["project"].(map[string]interface{})
	if proj["ip_allow"] != nil {
		t.Errorf("expected ip_allow=nil after Remove, got %v", proj["ip_allow"])
	}
}

func TestPatchProject_DefaultEndpointSettings(t *testing.T) {
	project := &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-project",
			Namespace: "neon",
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        "Test Project",
			PGVersion:   16,
			ClusterName: "default",
		},
	}

	handler := createAPIHandler(project)

	body := `{"project":{"default_endpoint_settings":{"resources":{"cpu":"2","memory":"4Gi"}}}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	proj := resp["project"].(map[string]interface{})
	ds := proj["default_endpoint_settings"].(map[string]interface{})
	resources := ds["resources"].(map[string]interface{})
	if resources["cpu"] != "2" {
		t.Errorf("expected cpu=2, got %v", resources["cpu"])
	}
	if resources["memory"] != "4Gi" {
		t.Errorf("expected memory=4Gi, got %v", resources["memory"])
	}
}

func TestPatchProject_Noop(t *testing.T) {
	project := &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-project",
			Namespace: "neon",
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        "No Change",
			PGVersion:   16,
			ClusterName: "default",
		},
	}

	handler := createAPIHandler(project)

	// Empty project fields = no change
	body := `{"project":{}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent noop), got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	proj := resp["project"].(map[string]interface{})
	if proj["name"] != "No Change" {
		t.Errorf("expected name unchanged, got %v", proj["name"])
	}
}

func TestPatchProject_NotFound(t *testing.T) {
	handler := createAPIHandler() // no objects in fake client

	body := `{"project":{"name":"Whatever"}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/nonexistent", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchProject_InvalidName(t *testing.T) {
	project := &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-project",
			Namespace: "neon",
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        "Valid Name",
			PGVersion:   16,
			ClusterName: "default",
		},
	}

	handler := createAPIHandler(project)

	body := `{"project":{"name":""}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchProject_InvalidJSON(t *testing.T) {
	handler := createAPIHandler()

	body := `{not valid json`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchProject_MultiField(t *testing.T) {
	project := &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-project",
			Namespace: "neon",
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        "Old Name",
			PGVersion:   16,
			ClusterName: "default",
		},
	}

	handler := createAPIHandler(project)

	body := `{
		"project": {
			"name": "Multi-Update",
			"history_retention_seconds": 1209600,
			"ip_allow": {
				"primary_branch_only": false,
				"source_ranges": ["10.0.0.0/8", "192.168.1.0/24"]
			},
			"default_endpoint_settings": {
				"resources": {
					"cpu": "1",
					"memory": "2Gi"
				}
			}
		}
	}`

	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/my-project", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.patchProject(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	proj := resp["project"].(map[string]interface{})

	if proj["name"] != "Multi-Update" {
		t.Errorf("name: expected 'Multi-Update', got %v", proj["name"])
	}
	if proj["history_retention_seconds"] != float64(1209600) {
		t.Errorf("history_retention_seconds: expected 1209600, got %v", proj["history_retention_seconds"])
	}
	if proj["ip_allow"] == nil {
		t.Error("ip_allow should not be nil")
	}
	if proj["default_endpoint_settings"] == nil {
		t.Error("default_endpoint_settings should not be nil")
	}
}

func TestNullableJSON(t *testing.T) {
	t.Run("Upsert", func(t *testing.T) {
		var n Nullable[IPAllowConfigUpdate]
		data := []byte(`{"primary_branch_only":true,"source_ranges":["10.0.0.0/8"]}`)
		if err := json.Unmarshal(data, &n); err != nil {
			t.Fatal(err)
		}
		if !n.IsUpsert() {
			t.Error("expected Upsert")
		}
		if n.Value.PrimaryBranchOnly != true {
			t.Error("expected primary_branch_only=true")
		}
	})

	t.Run("Remove", func(t *testing.T) {
		var n Nullable[IPAllowConfigUpdate]
		data := []byte(`null`)
		if err := json.Unmarshal(data, &n); err != nil {
			t.Fatal(err)
		}
		if !n.IsRemove() {
			t.Error("expected Remove")
		}
	})

	t.Run("NoopWhenNotInJSON", func(t *testing.T) {
		type wrapper struct {
			Field Nullable[string] `json:"field"`
		}
		var w wrapper
		data := []byte(`{}`)
		if err := json.Unmarshal(data, &w); err != nil {
			t.Fatal(err)
		}
		if !w.Field.IsNoop() {
			t.Error("expected Noop when field is absent")
		}
	})
}
