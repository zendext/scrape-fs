package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/sourcefile"
	"golang.org/x/sys/unix"
)

func TestStatusReportsStableComponentAndLibrarySnapshot(t *testing.T) {
	runtime := NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock")
	runtime.SetMount(nil)
	runtime.SetSocket(nil)
	runtime.SetReady(true)
	service := newService(
		fakeDatabase{version: 4},
		fakeRepository{
			inspection: namespace.RepositoryInspection{
				NamespaceVersion: 7,
				ActiveLibraries:  2,
				Nodes:            8,
				Projections:      3,
				OverlayFiles:     1,
				Libraries: []namespace.Library{
					{
						ID:             2,
						Name:           "plex",
						Status:         namespace.LibraryActive,
						RootNodeID:     3,
						ProfileName:    "plex",
						ProfileVersion: 1,
					},
					{
						ID:             1,
						Name:           "jellyfin",
						Status:         namespace.LibraryActive,
						RootNodeID:     2,
						ProfileName:    "jellyfin",
						ProfileVersion: 1,
					},
				},
			},
		},
		fakeIndex{
			stats: namespace.IndexStats{Libraries: 2, Nodes: 8, Objects: 1},
		},
		fakeObjectStore{
			recovery: objectstore.RecoveryReport{
				Schema: objectstore.RecoveryReportSchema,
				Issues: []objectstore.IntegrityIssue{
					{ObjectID: 9, Code: objectstore.IntegrityMissingReferencedObject},
				},
			},
		},
		runtime,
		"1.2.3",
		nil,
	)

	result := service.Status(context.Background())

	if result.Schema != StatusSchema ||
		result.State != StateDegraded ||
		!result.Daemon.Ready ||
		result.Daemon.Version != "1.2.3" {
		t.Errorf("status identity = %#v", result)
	}
	if result.Database.State != StateReady ||
		result.Database.SchemaVersion != 4 ||
		result.Database.SupportedSchemaVersion != 4 {
		t.Errorf("database status = %#v", result.Database)
	}
	if result.Namespace.State != StateReady ||
		result.Namespace.Version != 7 ||
		result.Namespace.Nodes != 8 ||
		result.Namespace.Projections != 3 ||
		result.Namespace.OverlayFiles != 1 {
		t.Errorf("namespace status = %#v", result.Namespace)
	}
	if result.Index.State != StateReady ||
		result.Index.Libraries != 2 ||
		result.Index.Nodes != 8 ||
		result.Index.Objects != 1 {
		t.Errorf("index status = %#v", result.Index)
	}
	if result.Mount.State != StateReady ||
		result.Mount.Path != "/mnt/scrapefs" ||
		result.Socket.State != StateReady ||
		result.Socket.Path != "/run/scrapefs/scrapefs.sock" {
		t.Errorf("runtime status = mount %#v socket %#v", result.Mount, result.Socket)
	}
	if result.ObjectStore.State != StateDegraded ||
		result.ObjectStore.StartupIssues != 1 {
		t.Errorf("object-store status = %#v", result.ObjectStore)
	}
	wantLibraries := []LibrarySummary{
		{
			ID:             1,
			Name:           "jellyfin",
			Status:         "ACTIVE",
			RootNodeID:     2,
			ProfileName:    "jellyfin",
			ProfileVersion: 1,
		},
		{
			ID:             2,
			Name:           "plex",
			Status:         "ACTIVE",
			RootNodeID:     3,
			ProfileName:    "plex",
			ProfileVersion: 1,
		},
	}
	if !reflect.DeepEqual(result.Libraries.Items, wantLibraries) {
		t.Errorf("Library summaries = %#v, want %#v", result.Libraries.Items, wantLibraries)
	}
}

func TestStatusAndDoctorJSONSchemasHaveStableFields(t *testing.T) {
	runtime := NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock")
	runtime.SetMount(nil)
	runtime.SetSocket(nil)
	runtime.SetReady(true)
	service := newService(
		fakeDatabase{version: 4},
		fakeRepository{
			inspection: namespace.RepositoryInspection{
				ActiveLibraries: 1,
				Nodes:           2,
				Libraries: []namespace.Library{
					{
						ID:             1,
						Name:           "media",
						Status:         namespace.LibraryActive,
						RootNodeID:     2,
						ProfileName:    "jellyfin",
						ProfileVersion: 1,
					},
				},
			},
		},
		fakeIndex{},
		fakeObjectStore{},
		runtime,
		"dev",
		nil,
	)

	status := service.Status(context.Background())
	statusJSON, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal status: %v", err)
	}
	var statusObject map[string]json.RawMessage
	if err := json.Unmarshal(statusJSON, &statusObject); err != nil {
		t.Fatalf("Unmarshal status: %v", err)
	}
	assertJSONKeys(t, statusObject, []string{
		"schema",
		"state",
		"daemon",
		"database",
		"namespace",
		"index",
		"mount",
		"socket",
		"object_store",
		"libraries",
	})
	assertNestedJSONKeys(t, statusObject["daemon"], []string{"version", "ready"})
	assertNestedJSONKeys(t, statusObject["database"], []string{
		"state",
		"schema_version",
		"supported_schema_version",
	})
	assertNestedJSONKeys(t, statusObject["namespace"], []string{
		"state",
		"version",
		"nodes",
		"projections",
		"overlay_files",
	})
	assertNestedJSONKeys(t, statusObject["index"], []string{
		"state",
		"libraries",
		"nodes",
		"objects",
	})
	assertNestedJSONKeys(t, statusObject["mount"], []string{"state", "path"})
	assertNestedJSONKeys(t, statusObject["socket"], []string{"state", "path"})
	assertNestedJSONKeys(t, statusObject["object_store"], []string{"state", "startup_issues"})
	assertNestedJSONKeys(t, statusObject["libraries"], []string{"state", "active", "items"})
	var libraryStatus struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(statusObject["libraries"], &libraryStatus); err != nil {
		t.Fatalf("Unmarshal Library status: %v", err)
	}
	if len(libraryStatus.Items) != 1 {
		t.Fatalf("Library items = %#v, want one item", libraryStatus.Items)
	}
	assertJSONKeys(t, libraryStatus.Items[0], []string{
		"id",
		"name",
		"status",
		"root_node_id",
		"profile_name",
		"profile_version",
	})

	doctor := service.Doctor(context.Background())
	doctorJSON, err := json.Marshal(doctor)
	if err != nil {
		t.Fatalf("Marshal doctor: %v", err)
	}
	var doctorObject map[string]json.RawMessage
	if err := json.Unmarshal(doctorJSON, &doctorObject); err != nil {
		t.Fatalf("Unmarshal doctor: %v", err)
	}
	assertJSONKeys(t, doctorObject, []string{
		"schema",
		"severity",
		"status",
		"summary",
		"findings",
	})
	assertNestedJSONKeys(t, doctorObject["summary"], []string{
		"healthy",
		"warning",
		"blocking",
	})
	var findings []map[string]json.RawMessage
	if err := json.Unmarshal(doctorObject["findings"], &findings); err != nil {
		t.Fatalf("Unmarshal findings: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("healthy doctor JSON has no findings")
	}
	assertJSONKeys(t, findings[0], []string{
		"severity",
		"code",
		"component",
		"message",
	})
	detailedFindingJSON, err := json.Marshal(Finding{
		Severity:    SeverityBlocking,
		Code:        CodeMissingObject,
		Component:   "object_store",
		Message:     "missing",
		Remediation: "restore",
		Library:     "media",
		Target:      "media:/poster.jpg",
		NodeID:      7,
		NodeIDs:     []int64{7, 8},
		ObjectID:    9,
	})
	if err != nil {
		t.Fatalf("Marshal detailed finding: %v", err)
	}
	var detailedFinding map[string]json.RawMessage
	if err := json.Unmarshal(detailedFindingJSON, &detailedFinding); err != nil {
		t.Fatalf("Unmarshal detailed finding: %v", err)
	}
	assertJSONKeys(t, detailedFinding, []string{
		"severity",
		"code",
		"component",
		"message",
		"remediation",
		"library",
		"target",
		"node_id",
		"node_ids",
		"object_id",
	})
}

func TestStatusKeepsReadinessFalseUntilRuntimeIsMarkedReady(t *testing.T) {
	runtime := NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock")
	runtime.SetMount(nil)
	runtime.SetSocket(nil)
	service := newService(
		fakeDatabase{version: 4},
		fakeRepository{},
		fakeIndex{},
		fakeObjectStore{},
		runtime,
		"dev",
		nil,
	)

	before := service.Status(context.Background())
	if before.Daemon.Ready || before.State != StateUnavailable {
		t.Fatalf("pre-readiness status = %#v", before)
	}

	runtime.SetReady(true)
	after := service.Status(context.Background())
	if !after.Daemon.Ready || after.State != StateReady {
		t.Fatalf("ready status = %#v", after)
	}
}

func TestStatusClearsReadinessWhenRuntimeMetadataDegrades(t *testing.T) {
	runtime := NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock")
	runtime.SetMount(nil)
	runtime.SetSocket(func() error { return errors.New("socket inode changed") })
	runtime.SetReady(true)
	result := newService(
		fakeDatabase{version: 4},
		fakeRepository{},
		fakeIndex{},
		fakeObjectStore{},
		runtime,
		"dev",
		nil,
	).Status(context.Background())

	if result.Daemon.Ready ||
		result.State != StateUnavailable ||
		result.Socket.State != StateDegraded ||
		result.Socket.Error != "socket inode changed" {
		t.Fatalf("degraded runtime status = %#v", result)
	}
}

func TestStatusRepresentsUnavailableComponentsWithoutDroppingResult(t *testing.T) {
	result := newService(
		nil,
		nil,
		nil,
		nil,
		NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock"),
		"dev",
		nil,
	).Status(context.Background())

	if result.Schema != StatusSchema || result.State != StateUnavailable {
		t.Fatalf("status = %#v", result)
	}
	for name, component := range map[string]ComponentStatus{
		"database":     result.Database.ComponentStatus,
		"namespace":    result.Namespace.ComponentStatus,
		"index":        result.Index.ComponentStatus,
		"mount":        result.Mount,
		"socket":       result.Socket,
		"object store": result.ObjectStore.ComponentStatus,
		"Libraries":    result.Libraries.ComponentStatus,
	} {
		if component.State != StateUnavailable || component.Error == "" {
			t.Errorf("%s component = %#v", name, component)
		}
	}
}

func TestDoctorReportsDatabaseCorruption(t *testing.T) {
	runtime := NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock")
	runtime.SetMount(nil)
	runtime.SetSocket(nil)
	runtime.SetReady(true)
	service := newService(
		fakeDatabase{
			version:   4,
			integrity: []string{"row 7 missing from index nodes_parent_id"},
		},
		fakeRepository{},
		fakeIndex{},
		fakeObjectStore{},
		runtime,
		"dev",
		nil,
	)

	result := service.Doctor(context.Background())

	if !hasFindingCode(result.Findings, CodeDatabaseCorrupt) ||
		result.Severity != SeverityBlocking {
		t.Fatalf("doctor result = %#v", result)
	}
}

func TestDoctorDoesNotMisclassifyNamespaceCorruptionAsInvalidLibrary(t *testing.T) {
	runtime := NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock")
	runtime.SetMount(nil)
	runtime.SetSocket(nil)
	runtime.SetReady(true)
	service := newService(
		fakeDatabase{version: 4},
		fakeRepository{
			libraries: []namespace.Library{
				{
					ID:             1,
					Name:           "media",
					Status:         namespace.LibraryActive,
					RootNodeID:     2,
					ProfileName:    "jellyfin",
					ProfileVersion: 1,
				},
			},
			err: namespace.ErrInvalidNode,
		},
		fakeIndex{},
		fakeObjectStore{},
		runtime,
		"dev",
		nil,
	)

	result := service.Doctor(context.Background())

	if !hasFindingCode(result.Findings, CodeInvalidNamespace) {
		t.Fatalf("doctor findings = %#v, want invalid namespace", result.Findings)
	}
	if hasFindingCode(result.Findings, CodeInvalidLibrary) {
		t.Fatalf("doctor findings = %#v, do not want invalid Library", result.Findings)
	}
}

func TestDoctorReturnsDeterministicHealthyWarningAndBlockingFindings(t *testing.T) {
	runtime := NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock")
	runtime.SetMount(nil)
	runtime.SetSocket(func() error { return errors.New("socket path replaced") })
	runtime.SetReady(true)
	missingSource := namespace.Projection{
		LibraryName: "media",
		Target:      "Movies/Missing.mkv",
		Node: namespace.Node{
			ID: 12,
			Source: &namespace.ProjectionSource{
				Path:     "/sources/missing.mkv",
				DeviceID: 1,
				Inode:    2,
				Size:     3,
				MtimeNS:  4,
			},
		},
	}
	staleSource := namespace.Projection{
		LibraryName: "media",
		Target:      "Movies/Stale.mkv",
		Node: namespace.Node{
			ID: 11,
			Source: &namespace.ProjectionSource{
				Path:     "/sources/stale.mkv",
				DeviceID: 5,
				Inode:    6,
				Size:     7,
				MtimeNS:  8,
			},
		},
	}
	service := newService(
		fakeDatabase{version: 4},
		fakeRepository{
			inspection: namespace.RepositoryInspection{
				NamespaceVersion: 9,
				ActiveLibraries:  1,
				Nodes:            4,
				Projections:      2,
			},
			projections: []namespace.Projection{missingSource, staleSource},
		},
		fakeIndex{
			stats: namespace.IndexStats{Libraries: 1, Nodes: 3},
			inspection: namespace.IndexInspection{
				Index:      namespace.IndexStats{Libraries: 1, Nodes: 3},
				Repository: namespace.IndexStats{Libraries: 1, Nodes: 4},
				Diverged:   true,
			},
		},
		fakeObjectStore{
			inspection: objectstore.Inspection{
				Schema: objectstore.InspectionSchema,
				Issues: []objectstore.IntegrityIssue{
					{
						ObjectID: 22,
						Code:     objectstore.IntegrityOrphanObject,
						Message:  "LIVE object 22 has no namespace references",
					},
					{
						ObjectID: 21,
						Code:     objectstore.IntegrityMissingReferencedObject,
						NodeIDs:  []int64{13},
						Message:  "referenced LIVE object 21 is missing",
					},
				},
			},
		},
		runtime,
		"dev",
		func(path string, _ sourcefile.Identity) error {
			switch path {
			case "/sources/missing.mkv":
				return os.ErrNotExist
			case "/sources/stale.mkv":
				return unix.ESTALE
			default:
				return nil
			}
		},
	)

	first := service.Doctor(context.Background())
	second := service.Doctor(context.Background())

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("doctor results are not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if first.Schema != DoctorSchema ||
		first.Severity != SeverityBlocking ||
		first.Summary.Healthy != 3 ||
		first.Summary.Warning != 1 ||
		first.Summary.Blocking != 5 {
		t.Errorf("doctor summary = %#v", first)
	}
	wantCodes := []string{
		CodeIndexDivergence,
		CodeMissingObject,
		CodeBrokenProjection,
		CodeStaleProjection,
		CodeUnsafeRuntime,
		CodeOrphanObject,
		CodeDatabaseOK,
		CodeLibrariesOK,
		CodeNamespaceOK,
	}
	if len(first.Findings) != len(wantCodes) {
		t.Fatalf("findings = %#v", first.Findings)
	}
	for index, code := range wantCodes {
		if first.Findings[index].Code != code {
			t.Errorf("finding[%d] code = %q, want %q", index, first.Findings[index].Code, code)
		}
	}
	if first.Findings[2].NodeID != 12 ||
		first.Findings[2].Target != "media:/Movies/Missing.mkv" ||
		first.Findings[3].NodeID != 11 ||
		first.Findings[4].Component != "socket" {
		t.Errorf("finding details = %#v", first.Findings)
	}
}

func TestDoctorHealthyResultIsExplicit(t *testing.T) {
	runtime := NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock")
	runtime.SetMount(nil)
	runtime.SetSocket(nil)
	runtime.SetReady(true)
	service := newService(
		fakeDatabase{version: 4},
		fakeRepository{},
		fakeIndex{},
		fakeObjectStore{},
		runtime,
		"dev",
		func(string, sourcefile.Identity) error { return nil },
	)

	result := service.Doctor(context.Background())

	if result.Severity != SeverityHealthy ||
		result.Summary.Healthy != len(result.Findings) ||
		result.Summary.Warning != 0 ||
		result.Summary.Blocking != 0 {
		t.Fatalf("healthy doctor result = %#v", result)
	}
	for _, finding := range result.Findings {
		if finding.Severity != SeverityHealthy {
			t.Errorf("finding = %#v, want healthy", finding)
		}
	}
}

func TestDoctorInspectsRealCorruptStateWithoutMutation(t *testing.T) {
	root := t.TempDir()
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("database Close: %v", err)
		}
	})
	repository := namespace.NewRepository(opened)
	storePath := filepath.Join(root, "objects")
	store, err := objectstore.OpenWithOptions(
		storePath,
		opened,
		objectstore.Options{Repository: repository},
	)
	if err != nil {
		t.Fatalf("Open object store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("object store Close: %v", err)
		}
	})
	media := createDiagnosticLibrary(t, repository, "media")

	projectionService := projection.NewService(repository)
	missingPath := filepath.Join(root, "missing.mkv")
	if err := os.WriteFile(missingPath, []byte("missing"), 0o600); err != nil {
		t.Fatalf("WriteFile missing source: %v", err)
	}
	if _, err := projectionService.Create(context.Background(), projection.CreateRequest{
		SourcePath: missingPath,
		Target:     "media:/Movies/Missing.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Create missing Projection: %v", err)
	}
	if err := os.Remove(missingPath); err != nil {
		t.Fatalf("Remove missing source: %v", err)
	}

	stalePath := filepath.Join(root, "stale.mkv")
	if err := os.WriteFile(stalePath, []byte("old"), 0o600); err != nil {
		t.Fatalf("WriteFile stale source: %v", err)
	}
	if _, err := projectionService.Create(context.Background(), projection.CreateRequest{
		SourcePath: stalePath,
		Target:     "media:/Movies/Stale.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Create stale Projection: %v", err)
	}
	if err := os.Remove(stalePath); err != nil {
		t.Fatalf("Remove stale source: %v", err)
	}
	if err := os.WriteFile(stalePath, []byte("new"), 0o600); err != nil {
		t.Fatalf("Replace stale source: %v", err)
	}

	handle, overlay, err := store.Create(
		context.Background(),
		namespace.Operation{
			Name:  "overlay.create",
			Actor: namespace.Actor{Kind: namespace.ActorSystem},
			Details: namespace.OperationDetails{
				Paths:     []string{"media/poster.jpg"},
				NodeKinds: []namespace.NodeKind{namespace.OverlayFile},
			},
		},
		namespace.CreateOverlay{
			ParentID: media.RootNodeID,
			Name:     "poster.jpg",
			Creator:  namespace.Actor{Kind: namespace.ActorSystem},
			Attributes: namespace.Attributes{
				Mode: 0o640,
				UID:  1000,
				GID:  1000,
			},
		},
		strings.NewReader("poster"),
	)
	if err != nil {
		t.Fatalf("Create OverlayFile: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("Close OverlayFile: %v", err)
	}
	if err := os.Remove(filepath.Join(
		storePath,
		"live",
		strconv.FormatInt(*overlay.ObjectID, 10),
	)); err != nil {
		t.Fatalf("remove referenced object: %v", err)
	}
	const orphanID int64 = 99
	if err := os.WriteFile(
		filepath.Join(storePath, "live", strconv.FormatInt(orphanID, 10)),
		[]byte("orphan"),
		0o600,
	); err != nil {
		t.Fatalf("write orphan object: %v", err)
	}

	index := namespace.NewIndex(repository)
	if _, err := index.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild index: %v", err)
	}
	createDiagnosticLibrary(t, repository, "archive")

	runtime := NewRuntime("/mnt/scrapefs", "/run/scrapefs/scrapefs.sock")
	runtime.SetMount(nil)
	runtime.SetSocket(nil)
	runtime.SetReady(true)
	service := NewService(opened, repository, index, store, runtime, "dev")
	versionBefore, err := repository.NamespaceVersion(context.Background())
	if err != nil {
		t.Fatalf("NamespaceVersion before doctor: %v", err)
	}
	objectsBefore, err := store.Inspect(context.Background())
	if err != nil {
		t.Fatalf("object Inspect before doctor: %v", err)
	}

	first := service.Doctor(context.Background())
	second := service.Doctor(context.Background())

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("doctor results differ:\nfirst=%#v\nsecond=%#v", first, second)
	}
	for _, code := range []string{
		CodeBrokenProjection,
		CodeStaleProjection,
		CodeMissingObject,
		CodeOrphanObject,
		CodeIndexDivergence,
	} {
		if !hasFindingCode(first.Findings, code) {
			t.Errorf("doctor findings do not contain %s: %#v", code, first.Findings)
		}
	}
	missingFinding := findingByCode(first.Findings, CodeMissingObject)
	if missingFinding == nil ||
		!reflect.DeepEqual(missingFinding.NodeIDs, []int64{overlay.ID}) {
		t.Errorf("missing object finding = %#v, want node ID %d", missingFinding, overlay.ID)
	}
	versionAfter, err := repository.NamespaceVersion(context.Background())
	if err != nil {
		t.Fatalf("NamespaceVersion after doctor: %v", err)
	}
	objectsAfter, err := store.Inspect(context.Background())
	if err != nil {
		t.Fatalf("object Inspect after doctor: %v", err)
	}
	if versionAfter != versionBefore {
		t.Errorf("doctor changed namespace version from %d to %d", versionBefore, versionAfter)
	}
	if !reflect.DeepEqual(objectsBefore, objectsAfter) {
		t.Errorf("doctor changed object state:\nbefore=%#v\nafter=%#v", objectsBefore, objectsAfter)
	}
	if _, err := os.Stat(missingPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("doctor changed missing source state: %v", err)
	}
	if content, err := os.ReadFile(stalePath); err != nil || string(content) != "new" {
		t.Errorf("doctor changed stale source = %q/%v", content, err)
	}
}

func createDiagnosticLibrary(
	t *testing.T,
	repository *namespace.Repository,
	name string,
) namespace.Library {
	t.Helper()
	created, err := library.NewService(repository).Create(
		context.Background(),
		library.CreateRequest{
			Name:    name,
			Profile: library.ProfileIdentifier{Name: "jellyfin", Version: 1},
			Actor:   namespace.Actor{Kind: namespace.ActorSystem},
			UID:     1000,
			GID:     1000,
			Modes:   library.DefaultModes,
		},
	)
	if err != nil {
		t.Fatalf("Create Library %q: %v", name, err)
	}
	return created
}

func hasFindingCode(findings []Finding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func findingByCode(findings []Finding, code string) *Finding {
	for index := range findings {
		if findings[index].Code == code {
			return &findings[index]
		}
	}
	return nil
}

func assertNestedJSONKeys(t *testing.T, data json.RawMessage, want []string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("Unmarshal nested JSON: %v; data=%s", err, data)
	}
	assertJSONKeys(t, object, want)
}

func assertJSONKeys(
	t *testing.T,
	object map[string]json.RawMessage,
	want []string,
) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JSON keys = %v, want %v", got, want)
	}
}

type fakeDatabase struct {
	version   int
	integrity []string
	err       error
}

func (database fakeDatabase) SchemaVersion(context.Context) (int, error) {
	return database.version, database.err
}

func (database fakeDatabase) IntegrityCheck(context.Context) ([]string, error) {
	if database.integrity == nil {
		return []string{"ok"}, database.err
	}
	return append([]string(nil), database.integrity...), database.err
}

type fakeRepository struct {
	inspection  namespace.RepositoryInspection
	libraries   []namespace.Library
	projections []namespace.Projection
	err         error
}

func (repository fakeRepository) Inspect(context.Context) (namespace.RepositoryInspection, error) {
	return repository.inspection, repository.err
}

func (repository fakeRepository) Libraries(context.Context) ([]namespace.Library, error) {
	return append([]namespace.Library(nil), repository.libraries...), nil
}

func (repository fakeRepository) Projections(context.Context, string) ([]namespace.Projection, error) {
	return append([]namespace.Projection(nil), repository.projections...), repository.err
}

type fakeIndex struct {
	stats      namespace.IndexStats
	inspection namespace.IndexInspection
	err        error
}

func (index fakeIndex) Stats() namespace.IndexStats {
	return index.stats
}

func (index fakeIndex) Inspect(context.Context) (namespace.IndexInspection, error) {
	if index.inspection == (namespace.IndexInspection{}) {
		index.inspection = namespace.IndexInspection{
			Index:      index.stats,
			Repository: index.stats,
		}
	}
	return index.inspection, index.err
}

type fakeObjectStore struct {
	recovery   objectstore.RecoveryReport
	inspection objectstore.Inspection
	err        error
}

func (store fakeObjectStore) RecoveryReport() objectstore.RecoveryReport {
	if store.recovery.Schema == "" {
		store.recovery = objectstore.RecoveryReport{
			Schema: objectstore.RecoveryReportSchema,
			Issues: []objectstore.IntegrityIssue{},
		}
	}
	return store.recovery
}

func (store fakeObjectStore) Inspect(context.Context) (objectstore.Inspection, error) {
	if store.inspection.Schema == "" {
		store.inspection = objectstore.Inspection{
			Schema: objectstore.InspectionSchema,
			Issues: []objectstore.IntegrityIssue{},
		}
	}
	return store.inspection, store.err
}
