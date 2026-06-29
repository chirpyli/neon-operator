package controlplane

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// =============================================================================
// API 路由注册
// =============================================================================

func addAPIRoutes(mux *http.ServeMux, svc *apiService, log *slog.Logger) {
	api := &apiHandler{svc: svc, log: log}

	// ---------- Projects ----------
	mux.Handle("POST /api/v2/projects", logRequests(log, http.HandlerFunc(api.createProject)))
	mux.Handle("GET /api/v2/projects", logRequests(log, http.HandlerFunc(api.listProjects)))
	mux.Handle("GET /api/v2/projects/{project_id}", logRequests(log, http.HandlerFunc(api.getProject)))
	mux.Handle("DELETE /api/v2/projects/{project_id}", logRequests(log, http.HandlerFunc(api.deleteProject)))

	// ---------- Branches ----------
	mux.Handle("POST /api/v2/projects/{project_id}/branches", logRequests(log, http.HandlerFunc(api.createBranch)))
	mux.Handle("GET /api/v2/projects/{project_id}/branches", logRequests(log, http.HandlerFunc(api.listBranches)))
	mux.Handle("GET /api/v2/projects/{project_id}/branches/{branch_id}", logRequests(log, http.HandlerFunc(api.getBranch)))
	mux.Handle("DELETE /api/v2/projects/{project_id}/branches/{branch_id}", logRequests(log, http.HandlerFunc(api.deleteBranch)))

	// ---------- Endpoints ----------
	mux.Handle("POST /api/v2/projects/{project_id}/endpoints", logRequests(log, http.HandlerFunc(api.createEndpoint)))
	mux.Handle("GET /api/v2/projects/{project_id}/endpoints", logRequests(log, http.HandlerFunc(api.listEndpoints)))
	mux.Handle("GET /api/v2/projects/{project_id}/endpoints/{endpoint_id}", logRequests(log, http.HandlerFunc(api.getEndpoint)))
	mux.Handle("DELETE /api/v2/projects/{project_id}/endpoints/{endpoint_id}", logRequests(log, http.HandlerFunc(api.deleteEndpoint)))

	// ---------- Roles ----------
	mux.Handle("POST /api/v2/projects/{project_id}/branches/{branch_id}/roles", logRequests(log, http.HandlerFunc(api.createRole)))
	mux.Handle("GET /api/v2/projects/{project_id}/branches/{branch_id}/roles", logRequests(log, http.HandlerFunc(api.listRoles)))
	mux.Handle("DELETE /api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}", logRequests(log, http.HandlerFunc(api.deleteRole)))
	mux.Handle("POST /api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}/reset_password", logRequests(log, http.HandlerFunc(api.resetRolePassword)))

	// ---------- Databases ----------
	mux.Handle("POST /api/v2/projects/{project_id}/branches/{branch_id}/databases", logRequests(log, http.HandlerFunc(api.createDatabase)))
	mux.Handle("GET /api/v2/projects/{project_id}/branches/{branch_id}/databases", logRequests(log, http.HandlerFunc(api.listDatabases)))
	mux.Handle("DELETE /api/v2/projects/{project_id}/branches/{branch_id}/databases/{database_name}", logRequests(log, http.HandlerFunc(api.deleteDatabase)))

	// ---------- Operations ----------
	mux.Handle("GET /api/v2/projects/{project_id}/operations", logRequests(log, http.HandlerFunc(api.listOperations)))
	mux.Handle("GET /api/v2/projects/{project_id}/operations/{operation_id}", logRequests(log, http.HandlerFunc(api.getOperation)))
}

// =============================================================================
// apiHandler — HTTP handler 绑定
// =============================================================================

type apiHandler struct {
	svc *apiService
	log *slog.Logger
}

func (h *apiHandler) handleAPIError(w http.ResponseWriter, err error) {
	if apiErr, ok := err.(*apiError); ok {
		status := http.StatusBadRequest
		switch apiErr.Code {
		case "PROJECT_NOT_FOUND", "BRANCH_NOT_FOUND", "ENDPOINT_NOT_FOUND",
			"ROLE_NOT_FOUND", "DATABASE_NOT_FOUND", "OPERATION_NOT_FOUND",
			"DEFAULT_BRANCH_NOT_FOUND":
			status = http.StatusNotFound
		case "DEFAULT_BRANCH_DELETE", "PROTECTED_BRANCH", "PROTECTED_ROLE":
			status = http.StatusConflict
		case "INVALID_NAME":
			status = http.StatusBadRequest
		}
		writeAPIError(w, status, apiErr.Code, apiErr.Message)
		return
	}
	h.log.Error("unhandled error", "error", err)
	writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
}

// =============================================================================
// Project Handlers
// =============================================================================

func (h *apiHandler) createProject(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := r.Body.Close(); err != nil {
			h.log.Error("failed to close request body", "error", err)
		}
	}()

	var req ProjectCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON request body")
		return
	}

	resp, err := h.svc.CreateProject(r.Context(), req)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	if err := writeJSON(w, http.StatusCreated, resp); err != nil {
		h.log.Error("failed to encode response", "error", err)
	}
}

func (h *apiHandler) getProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")

	resp, err := h.svc.GetProject(r.Context(), projectID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, map[string]interface{}{"project": resp})
}

func (h *apiHandler) listProjects(w http.ResponseWriter, r *http.Request) {
	resp, err := h.svc.ListProjects(r.Context())
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

func (h *apiHandler) deleteProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")

	resp, err := h.svc.DeleteProject(r.Context(), projectID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

// =============================================================================
// Branch Handlers
// =============================================================================

func (h *apiHandler) createBranch(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := r.Body.Close(); err != nil {
			h.log.Error("failed to close request body", "error", err)
		}
	}()

	projectID := r.PathValue("project_id")

	var req BranchCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON request body")
		return
	}

	resp, err := h.svc.CreateBranch(r.Context(), projectID, req)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusCreated, resp)
}

func (h *apiHandler) getBranch(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	branchID := r.PathValue("branch_id")

	resp, err := h.svc.GetBranch(r.Context(), projectID, branchID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, map[string]interface{}{"branch": resp})
}

func (h *apiHandler) listBranches(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")

	resp, err := h.svc.ListBranches(r.Context(), projectID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

func (h *apiHandler) deleteBranch(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	branchID := r.PathValue("branch_id")

	resp, err := h.svc.DeleteBranch(r.Context(), projectID, branchID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

// =============================================================================
// Endpoint Handlers
// =============================================================================

func (h *apiHandler) createEndpoint(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := r.Body.Close(); err != nil {
			h.log.Error("failed to close request body", "error", err)
		}
	}()

	projectID := r.PathValue("project_id")

	var req EndpointCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON request body")
		return
	}

	resp, err := h.svc.CreateEndpoint(r.Context(), projectID, req)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusCreated, resp)
}

func (h *apiHandler) getEndpoint(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	endpointID := r.PathValue("endpoint_id")

	resp, err := h.svc.GetEndpoint(r.Context(), projectID, endpointID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, map[string]interface{}{"endpoint": resp})
}

func (h *apiHandler) listEndpoints(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")

	resp, err := h.svc.ListEndpoints(r.Context(), projectID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

func (h *apiHandler) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	endpointID := r.PathValue("endpoint_id")

	resp, err := h.svc.DeleteEndpoint(r.Context(), projectID, endpointID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

// =============================================================================
// Role Handlers
// =============================================================================

func (h *apiHandler) createRole(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := r.Body.Close(); err != nil {
			h.log.Error("failed to close request body", "error", err)
		}
	}()

	projectID := r.PathValue("project_id")
	branchID := r.PathValue("branch_id")

	var req RoleCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON request body")
		return
	}

	resp, err := h.svc.CreateRole(r.Context(), projectID, branchID, req)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusCreated, map[string]interface{}{"role": resp})
}

func (h *apiHandler) listRoles(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	branchID := r.PathValue("branch_id")

	resp, err := h.svc.ListRoles(r.Context(), projectID, branchID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

func (h *apiHandler) deleteRole(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	branchID := r.PathValue("branch_id")
	roleName := r.PathValue("role_name")

	if err := h.svc.DeleteRole(r.Context(), projectID, branchID, roleName); err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": true})
}

func (h *apiHandler) resetRolePassword(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	branchID := r.PathValue("branch_id")
	roleName := r.PathValue("role_name")

	resp, err := h.svc.ResetPassword(r.Context(), projectID, branchID, roleName)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

// =============================================================================
// Database Handlers
// =============================================================================

func (h *apiHandler) createDatabase(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := r.Body.Close(); err != nil {
			h.log.Error("failed to close request body", "error", err)
		}
	}()

	projectID := r.PathValue("project_id")
	branchID := r.PathValue("branch_id")

	var req DatabaseCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON request body")
		return
	}

	resp, err := h.svc.CreateDatabase(r.Context(), projectID, branchID, req)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusCreated, map[string]interface{}{"database": resp})
}

func (h *apiHandler) listDatabases(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	branchID := r.PathValue("branch_id")

	resp, err := h.svc.ListDatabases(r.Context(), projectID, branchID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

func (h *apiHandler) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	branchID := r.PathValue("branch_id")
	dbName := r.PathValue("database_name")

	if err := h.svc.DeleteDatabase(r.Context(), projectID, branchID, dbName); err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": true})
}

// =============================================================================
// Operation Handlers
// =============================================================================

func (h *apiHandler) listOperations(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")

	resp, err := h.svc.ListOperations(r.Context(), projectID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, resp)
}

func (h *apiHandler) getOperation(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	operationID := r.PathValue("operation_id")

	resp, err := h.svc.GetOperation(r.Context(), projectID, operationID)
	if err != nil {
		h.handleAPIError(w, err)
		return
	}

	_ = writeJSON(w, http.StatusOK, map[string]interface{}{"operation": resp})
}
