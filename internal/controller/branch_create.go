package controller

// Branch resources (ConfigMap, Deployment, Service) are no longer created here.
// Per Neon design, branches are pure timeline containers.
// Compute access is provided by Endpoints (read_write / read_only).
// See endpoint_create.go for endpoint resource creation.
