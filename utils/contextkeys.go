package utils

type contextKey string

const (
	ClusterNameKey    contextKey = "cluster"
	SafekeeperNameKey contextKey = "safekeeper"
	PageserverNameKey contextKey = "pageserver"
	ProjectNameKey    contextKey = "project"
	BranchNameKey     contextKey = "branch"
	EndpointNameKey   contextKey = "endpoint"
	RoleNameKey       contextKey = "role"
	DatabaseNameKey   contextKey = "database"
	OperationNameKey  contextKey = "operation"
)
