package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
	"github.com/zendext/scrape-fs/internal/sourcefile"
	"golang.org/x/sys/unix"
)

type databaseReader interface {
	SchemaVersion(context.Context) (int, error)
	IntegrityCheck(context.Context) ([]string, error)
}

type repositoryReader interface {
	Inspect(context.Context) (namespace.RepositoryInspection, error)
	Libraries(context.Context) ([]namespace.Library, error)
	Projections(context.Context, string) ([]namespace.Projection, error)
}

type indexReader interface {
	Stats() namespace.IndexStats
	Inspect(context.Context) (namespace.IndexInspection, error)
}

type objectStoreReader interface {
	RecoveryReport() objectstore.RecoveryReport
	Inspect(context.Context) (objectstore.Inspection, error)
}

type sourceVerifier func(string, sourcefile.Identity) error

type Service struct {
	database     databaseReader
	repository   repositoryReader
	index        indexReader
	objectStore  objectStoreReader
	runtime      *Runtime
	version      string
	verifySource sourceVerifier
}

func NewService(
	opened databaseReader,
	repository repositoryReader,
	index indexReader,
	store objectStoreReader,
	runtime *Runtime,
	version string,
) *Service {
	return newService(
		opened,
		repository,
		index,
		store,
		runtime,
		version,
		nil,
	)
}

func newService(
	opened databaseReader,
	repository repositoryReader,
	index indexReader,
	store objectStoreReader,
	runtime *Runtime,
	version string,
	verify sourceVerifier,
) *Service {
	if verify == nil {
		verify = verifyProjectionSource
	}
	return &Service{
		database:     opened,
		repository:   repository,
		index:        index,
		objectStore:  store,
		runtime:      runtime,
		version:      version,
		verifySource: verify,
	}
}

func (service *Service) Status(ctx context.Context) StatusResult {
	result := StatusResult{
		Schema: StatusSchema,
		Daemon: DaemonStatus{Version: service.version},
		Database: DatabaseStatus{
			ComponentStatus:        unavailable("database diagnostics unavailable"),
			SupportedSchemaVersion: database.SupportedSchemaVersion(),
		},
		Namespace: NamespaceStatus{
			ComponentStatus: unavailable("namespace diagnostics unavailable"),
		},
		Index: IndexStatus{
			ComponentStatus: unavailable("namespace index diagnostics unavailable"),
		},
		Mount:  unavailable("FUSE mount is unavailable"),
		Socket: unavailable("control socket is unavailable"),
		ObjectStore: ObjectStoreStatus{
			ComponentStatus: unavailable("object-store diagnostics unavailable"),
		},
		Libraries: LibraryStatus{
			ComponentStatus: unavailable("Library diagnostics unavailable"),
			Items:           []LibrarySummary{},
		},
	}

	if service.database != nil {
		version, err := service.database.SchemaVersion(ctx)
		if err != nil {
			result.Database.Error = err.Error()
		} else {
			result.Database.SchemaVersion = version
			result.Database.State = StateReady
			result.Database.Error = ""
			if version != result.Database.SupportedSchemaVersion {
				result.Database.State = StateDegraded
			}
		}
	}

	if service.repository != nil {
		inspection, err := service.repository.Inspect(ctx)
		if err != nil {
			result.Namespace.Error = err.Error()
			libraries, libraryErr := service.repository.Libraries(ctx)
			if libraryErr != nil {
				result.Libraries.Error = libraryErr.Error()
			} else {
				result.Libraries = libraryStatus(libraries)
			}
		} else {
			result.Namespace = NamespaceStatus{
				ComponentStatus: ready(),
				Version:         inspection.NamespaceVersion,
				Nodes:           inspection.Nodes,
				Projections:     inspection.Projections,
				OverlayFiles:    inspection.OverlayFiles,
			}
			result.Libraries = libraryStatus(inspection.Libraries)
		}
	}

	if service.index != nil {
		stats := service.index.Stats()
		result.Index = IndexStatus{
			ComponentStatus: ready(),
			Libraries:       stats.Libraries,
			Nodes:           stats.Nodes,
			Objects:         stats.Objects,
		}
	}
	if service.objectStore != nil {
		recovery := service.objectStore.RecoveryReport()
		state := StateReady
		if len(recovery.Issues) > 0 {
			state = StateDegraded
		}
		result.ObjectStore = ObjectStoreStatus{
			ComponentStatus: ComponentStatus{State: state},
			StartupIssues:   len(recovery.Issues),
		}
	}

	runtime := service.runtime.snapshot()
	result.Mount = runtimeStatus(runtime.mount, "FUSE mount is unavailable")
	result.Socket = runtimeStatus(runtime.socket, "control socket is unavailable")
	result.Daemon.Ready = runtime.ready &&
		result.Mount.State == StateReady &&
		result.Socket.State == StateReady
	result.State = aggregateState(result)
	return result
}

func (service *Service) Doctor(ctx context.Context) DoctorResult {
	status := service.Status(ctx)
	findings := make([]Finding, 0)

	findings = append(findings, service.databaseFindings(ctx, status.Database)...)

	if status.Namespace.State == StateUnavailable {
		findings = append(findings, blocking(
			CodeInvalidNamespace,
			"namespace",
			status.Namespace.Error,
		))
	} else {
		findings = append(findings, healthy(
			CodeNamespaceOK,
			"namespace",
			fmt.Sprintf("namespace version %d is structurally valid", status.Namespace.Version),
		))
	}
	if status.Libraries.State == StateUnavailable {
		findings = append(findings, blocking(
			CodeInvalidLibrary,
			"libraries",
			status.Libraries.Error,
		))
	} else {
		findings = append(findings, healthy(
			CodeLibrariesOK,
			"libraries",
			fmt.Sprintf("%d active Libraries have valid root relationships", status.Libraries.Active),
		))
	}

	findings = append(findings, service.projectionFindings(ctx, status.Namespace.State)...)
	findings = append(findings, service.objectStoreFindings(ctx)...)
	findings = append(findings, service.indexFindings(ctx)...)
	findings = append(findings, runtimeFindings(status)...)
	sortFindings(findings)

	result := DoctorResult{
		Schema:   DoctorSchema,
		Severity: SeverityHealthy,
		Status:   status,
		Findings: findings,
	}
	for _, finding := range findings {
		switch finding.Severity {
		case SeverityBlocking:
			result.Summary.Blocking++
			result.Severity = SeverityBlocking
		case SeverityWarning:
			result.Summary.Warning++
			if result.Severity == SeverityHealthy {
				result.Severity = SeverityWarning
			}
		case SeverityHealthy:
			result.Summary.Healthy++
		}
	}
	return result
}

func (service *Service) databaseFindings(
	ctx context.Context,
	status DatabaseStatus,
) []Finding {
	switch status.State {
	case StateUnavailable:
		return []Finding{blocking(
			CodeDatabaseUnavailable,
			"database",
			status.Error,
		)}
	case StateDegraded:
		return []Finding{blocking(
			CodeDatabaseSchemaMismatch,
			"database",
			fmt.Sprintf(
				"database schema version is %d; supported version is %d",
				status.SchemaVersion,
				status.SupportedSchemaVersion,
			),
		)}
	}
	results, err := service.database.IntegrityCheck(ctx)
	if err != nil {
		return []Finding{blocking(
			CodeDatabaseUnavailable,
			"database",
			err.Error(),
		)}
	}
	if len(results) != 1 || results[0] != "ok" {
		return []Finding{blocking(
			CodeDatabaseCorrupt,
			"database",
			"SQLite quick_check reported: "+strings.Join(results, "; "),
		)}
	}
	return []Finding{healthy(
		CodeDatabaseOK,
		"database",
		fmt.Sprintf("database schema version %d and SQLite quick_check are healthy", status.SchemaVersion),
	)}
}

func (service *Service) projectionFindings(
	ctx context.Context,
	namespaceState State,
) []Finding {
	if service.repository == nil || namespaceState == StateUnavailable {
		return nil
	}
	projections, err := service.repository.Projections(ctx, "")
	if err != nil {
		return []Finding{blocking(
			CodeBrokenProjection,
			"projection",
			fmt.Sprintf("list Projections: %v", err),
		)}
	}
	findings := make([]Finding, 0)
	for _, projection := range projections {
		target := projection.LibraryName + ":/" + projection.Target
		if projection.Node.Source == nil {
			findings = append(findings, Finding{
				Severity:  SeverityBlocking,
				Code:      CodeBrokenProjection,
				Component: "projection",
				Message:   "Projection has no persisted source metadata",
				Library:   projection.LibraryName,
				Target:    target,
				NodeID:    projection.Node.ID,
			})
			continue
		}
		source := *projection.Node.Source
		identity := sourcefile.Identity{
			DeviceID: source.DeviceID,
			Inode:    source.Inode,
			Size:     source.Size,
			MtimeNS:  source.MtimeNS,
		}
		if source.HasFilesystem {
			identity.Filesystem = &sourcefile.FilesystemIdentity{
				Type: source.Filesystem.Type,
				ID:   source.Filesystem.ID,
			}
		}
		err := service.verifySource(source.Path, identity)
		if err == nil {
			continue
		}
		code := CodeBrokenProjection
		message := fmt.Sprintf("Projection source %q is unavailable: %v", source.Path, err)
		if errors.Is(err, unix.ESTALE) {
			code = CodeStaleProjection
			message = fmt.Sprintf("Projection source %q no longer matches its persisted identity", source.Path)
		}
		findings = append(findings, Finding{
			Severity:    SeverityBlocking,
			Code:        code,
			Component:   "projection",
			Message:     message,
			Remediation: "migrate a legacy identity, rebind, or remove the Projection explicitly",
			Library:     projection.LibraryName,
			Target:      target,
			NodeID:      projection.Node.ID,
		})
	}
	if len(findings) == 0 {
		findings = append(findings, healthy(
			CodeProjectionsOK,
			"projection",
			fmt.Sprintf("%d Projection sources match persisted identities", len(projections)),
		))
	}
	return findings
}

func (service *Service) objectStoreFindings(ctx context.Context) []Finding {
	if service.objectStore == nil {
		return []Finding{blocking(
			CodeObjectStoreUnavailable,
			"object_store",
			"object-store diagnostics unavailable",
		)}
	}
	inspection, err := service.objectStore.Inspect(ctx)
	if err != nil {
		return []Finding{blocking(
			CodeObjectStoreUnavailable,
			"object_store",
			err.Error(),
		)}
	}
	findings := make([]Finding, 0, len(inspection.Issues))
	for _, issue := range inspection.Issues {
		finding := Finding{
			Severity:  SeverityBlocking,
			Code:      CodeInvalidObjectStore,
			Component: "object_store",
			Message:   issue.Message,
			NodeIDs:   append([]int64(nil), issue.NodeIDs...),
			ObjectID:  issue.ObjectID,
		}
		switch issue.Code {
		case objectstore.IntegrityMissingReferencedObject:
			finding.Code = CodeMissingObject
			finding.Remediation = "restore the missing object from a trusted backup or remove the referencing OverlayFile"
		case objectstore.IntegrityOrphanObject, objectstore.IntegrityUnexpectedObjectFile:
			finding.Severity = SeverityWarning
			finding.Code = CodeOrphanObject
			finding.Remediation = "review the object and run authorized garbage collection when appropriate"
		}
		findings = append(findings, finding)
	}
	if len(findings) == 0 {
		findings = append(findings, healthy(
			CodeObjectStoreOK,
			"object_store",
			fmt.Sprintf(
				"%d object records and %d object files are internally consistent",
				inspection.Records,
				inspection.StagingFiles+inspection.LiveFiles,
			),
		))
	}
	return findings
}

func (service *Service) indexFindings(ctx context.Context) []Finding {
	if service.index == nil {
		return []Finding{blocking(
			CodeIndexUnavailable,
			"index",
			"namespace index diagnostics unavailable",
		)}
	}
	inspection, err := service.index.Inspect(ctx)
	if err != nil {
		return []Finding{blocking(CodeIndexUnavailable, "index", err.Error())}
	}
	if inspection.Diverged {
		return []Finding{blocking(
			CodeIndexDivergence,
			"index",
			fmt.Sprintf(
				"in-memory index counts are %+v; Repository counts are %+v",
				inspection.Index,
				inspection.Repository,
			),
		)}
	}
	return []Finding{healthy(
		CodeIndexOK,
		"index",
		fmt.Sprintf("%d indexed nodes match the Repository snapshot", inspection.Index.Nodes),
	)}
}

func runtimeFindings(status StatusResult) []Finding {
	findings := make([]Finding, 0, 2)
	for _, component := range []struct {
		name  string
		value ComponentStatus
	}{
		{name: "mount", value: status.Mount},
		{name: "socket", value: status.Socket},
	} {
		switch component.value.State {
		case StateUnavailable:
			findings = append(findings, blocking(
				CodeRuntimeUnavailable,
				component.name,
				component.value.Error,
			))
		case StateDegraded:
			findings = append(findings, blocking(
				CodeUnsafeRuntime,
				component.name,
				component.value.Error,
			))
		}
	}
	if len(findings) == 0 && !status.Daemon.Ready {
		findings = append(findings, blocking(
			CodeRuntimeUnavailable,
			"runtime",
			"daemon readiness has not completed",
		))
	}
	if len(findings) == 0 {
		findings = append(findings, healthy(
			CodeRuntimeOK,
			"runtime",
			"daemon readiness, FUSE mount, and control socket metadata are valid",
		))
	}
	return findings
}

func verifyProjectionSource(path string, identity sourcefile.Identity) (err error) {
	opened, err := sourcefile.OpenVerified(path, identity)
	if err != nil {
		return err
	}
	return opened.Close()
}

func ready() ComponentStatus {
	return ComponentStatus{State: StateReady}
}

func libraryStatus(libraries []namespace.Library) LibraryStatus {
	items := make([]LibrarySummary, len(libraries))
	for index, library := range libraries {
		items[index] = LibrarySummary{
			ID:             library.ID,
			Name:           library.Name,
			Status:         string(library.Status),
			RootNodeID:     library.RootNodeID,
			ProfileName:    library.ProfileName,
			ProfileVersion: library.ProfileVersion,
		}
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].Name != items[right].Name {
			return items[left].Name < items[right].Name
		}
		return items[left].ID < items[right].ID
	})
	return LibraryStatus{
		ComponentStatus: ready(),
		Active:          len(items),
		Items:           items,
	}
}

func unavailable(message string) ComponentStatus {
	return ComponentStatus{State: StateUnavailable, Error: message}
}

func runtimeStatus(component runtimeComponent, unavailableMessage string) ComponentStatus {
	if !component.available {
		status := unavailable(unavailableMessage)
		status.Path = component.path
		return status
	}
	if component.check != nil {
		if err := component.check(); err != nil {
			return ComponentStatus{
				State: StateDegraded,
				Path:  component.path,
				Error: err.Error(),
			}
		}
	}
	return ComponentStatus{State: StateReady, Path: component.path}
}

func aggregateState(result StatusResult) State {
	if !result.Daemon.Ready {
		return StateUnavailable
	}
	state := StateReady
	for _, component := range []ComponentStatus{
		result.Database.ComponentStatus,
		result.Namespace.ComponentStatus,
		result.Index.ComponentStatus,
		result.Mount,
		result.Socket,
		result.ObjectStore.ComponentStatus,
		result.Libraries.ComponentStatus,
	} {
		switch component.State {
		case StateUnavailable:
			return StateUnavailable
		case StateDegraded:
			state = StateDegraded
		}
	}
	return state
}

func healthy(code, component, message string) Finding {
	return Finding{
		Severity:  SeverityHealthy,
		Code:      code,
		Component: component,
		Message:   message,
	}
}

func blocking(code, component, message string) Finding {
	return Finding{
		Severity:  SeverityBlocking,
		Code:      code,
		Component: component,
		Message:   message,
	}
}

func sortFindings(findings []Finding) {
	rank := func(severity Severity) int {
		switch severity {
		case SeverityBlocking:
			return 0
		case SeverityWarning:
			return 1
		default:
			return 2
		}
	}
	sort.Slice(findings, func(left, right int) bool {
		a := findings[left]
		b := findings[right]
		if rank(a.Severity) != rank(b.Severity) {
			return rank(a.Severity) < rank(b.Severity)
		}
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Library != b.Library {
			return a.Library < b.Library
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		if compared := slices.Compare(a.NodeIDs, b.NodeIDs); compared != 0 {
			return compared < 0
		}
		if a.ObjectID != b.ObjectID {
			return a.ObjectID < b.ObjectID
		}
		return a.Message < b.Message
	})
}
