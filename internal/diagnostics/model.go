package diagnostics

const (
	StatusSchema = "scrapefs.status/v1"
	DoctorSchema = "scrapefs.doctor/v1"
)

type State string

const (
	StateReady       State = "ready"
	StateDegraded    State = "degraded"
	StateUnavailable State = "unavailable"
)

type Severity string

const (
	SeverityHealthy  Severity = "healthy"
	SeverityWarning  Severity = "warning"
	SeverityBlocking Severity = "blocking"
)

const (
	CodeDatabaseOK             = "DATABASE_OK"
	CodeDatabaseUnavailable    = "DATABASE_UNAVAILABLE"
	CodeDatabaseSchemaMismatch = "DATABASE_SCHEMA_MISMATCH"
	CodeDatabaseCorrupt        = "DATABASE_CORRUPT"
	CodeNamespaceOK            = "NAMESPACE_OK"
	CodeInvalidNamespace       = "INVALID_NAMESPACE"
	CodeLibrariesOK            = "LIBRARIES_OK"
	CodeInvalidLibrary         = "INVALID_LIBRARY"
	CodeProjectionsOK          = "PROJECTIONS_OK"
	CodeBrokenProjection       = "BROKEN_PROJECTION"
	CodeStaleProjection        = "STALE_PROJECTION"
	CodeObjectStoreOK          = "OBJECT_STORE_OK"
	CodeObjectStoreUnavailable = "OBJECT_STORE_UNAVAILABLE"
	CodeMissingObject          = "MISSING_OBJECT"
	CodeOrphanObject           = "ORPHAN_OBJECT"
	CodeInvalidObjectStore     = "INVALID_OBJECT_STORE"
	CodeIndexOK                = "INDEX_OK"
	CodeIndexUnavailable       = "INDEX_UNAVAILABLE"
	CodeIndexDivergence        = "INDEX_DIVERGENCE"
	CodeRuntimeOK              = "RUNTIME_OK"
	CodeRuntimeUnavailable     = "RUNTIME_UNAVAILABLE"
	CodeUnsafeRuntime          = "UNSAFE_RUNTIME"
)

type ComponentStatus struct {
	State State  `json:"state"`
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
}

type DaemonStatus struct {
	Version string `json:"version"`
	Ready   bool   `json:"ready"`
}

type DatabaseStatus struct {
	ComponentStatus
	SchemaVersion          int `json:"schema_version"`
	SupportedSchemaVersion int `json:"supported_schema_version"`
}

type NamespaceStatus struct {
	ComponentStatus
	Version      int64 `json:"version"`
	Nodes        int   `json:"nodes"`
	Projections  int   `json:"projections"`
	OverlayFiles int   `json:"overlay_files"`
}

type IndexStatus struct {
	ComponentStatus
	Libraries int `json:"libraries"`
	Nodes     int `json:"nodes"`
	Objects   int `json:"objects"`
}

type ObjectStoreStatus struct {
	ComponentStatus
	StartupIssues int `json:"startup_issues"`
}

type LibrarySummary struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	RootNodeID     int64  `json:"root_node_id"`
	ProfileName    string `json:"profile_name"`
	ProfileVersion int64  `json:"profile_version"`
}

type LibraryStatus struct {
	ComponentStatus
	Active int              `json:"active"`
	Items  []LibrarySummary `json:"items"`
}

type StatusResult struct {
	Schema      string            `json:"schema"`
	State       State             `json:"state"`
	Daemon      DaemonStatus      `json:"daemon"`
	Database    DatabaseStatus    `json:"database"`
	Namespace   NamespaceStatus   `json:"namespace"`
	Index       IndexStatus       `json:"index"`
	Mount       ComponentStatus   `json:"mount"`
	Socket      ComponentStatus   `json:"socket"`
	ObjectStore ObjectStoreStatus `json:"object_store"`
	Libraries   LibraryStatus     `json:"libraries"`
}

type Finding struct {
	Severity    Severity `json:"severity"`
	Code        string   `json:"code"`
	Component   string   `json:"component"`
	Message     string   `json:"message"`
	Remediation string   `json:"remediation,omitempty"`
	Library     string   `json:"library,omitempty"`
	Target      string   `json:"target,omitempty"`
	NodeID      int64    `json:"node_id,omitempty"`
	NodeIDs     []int64  `json:"node_ids,omitempty"`
	ObjectID    int64    `json:"object_id,omitempty"`
}

type FindingSummary struct {
	Healthy  int `json:"healthy"`
	Warning  int `json:"warning"`
	Blocking int `json:"blocking"`
}

type DoctorResult struct {
	Schema   string         `json:"schema"`
	Severity Severity       `json:"severity"`
	Status   StatusResult   `json:"status"`
	Summary  FindingSummary `json:"summary"`
	Findings []Finding      `json:"findings"`
}
