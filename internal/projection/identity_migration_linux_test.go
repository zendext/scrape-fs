package projection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/sourcefile"
)

func TestServiceMigratesDirectAndBatchOwnedLegacyIdentitiesAtomically(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	library := createTestLibrary(t, repository)
	service := NewService(repository)
	ctx := context.Background()

	directPath := writeIdentityMigrationSource(t, "direct.mkv", "direct")
	batchPath := writeIdentityMigrationSource(t, "batch.mkv", "batch")
	direct := createLegacyProjection(t, service, library, directPath, "Direct.mkv", nil)
	batchID := createLegacyBatchProjection(t, service, library, batchPath, "Batch.mkv")

	versionBefore, logsBefore := namespaceState(t, opened)
	preview, err := service.MigrateIdentity(ctx, IdentityMigrationRequest{
		LibraryName: "media",
		Actor:       namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("MigrateIdentity preview: %v", err)
	}
	if preview.Applied || preview.ConfirmationDigest == "" ||
		len(preview.Candidates) != 2 || len(preview.Rejections) != 0 {
		t.Fatalf("preview = %#v", preview)
	}
	observedByProjectionID := make(map[int64]ObservedSource, len(preview.Candidates))
	for _, candidate := range preview.Candidates {
		cached := candidate.Projection.Node.Source
		if cached == nil || cached.DeviceID == candidate.Current.DeviceID {
			t.Errorf(
				"candidate %d device IDs = cached %v current %d, want remount mismatch",
				candidate.Projection.Node.ID,
				cached,
				candidate.Current.DeviceID,
			)
		}
		observedByProjectionID[candidate.Projection.Node.ID] = candidate.Current
	}
	repeated, err := service.MigrateIdentity(ctx, IdentityMigrationRequest{
		LibraryName: "media",
		Actor:       namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("repeat MigrateIdentity preview: %v", err)
	}
	if !reflect.DeepEqual(repeated, preview) {
		t.Fatalf("migration preview is not deterministic:\nfirst=%#v\nsecond=%#v", preview, repeated)
	}
	if version, logs := namespaceState(t, opened); version != versionBefore || logs != logsBefore {
		t.Errorf("preview state = version %d logs %d, want %d/%d", version, logs, versionBefore, logsBefore)
	}
	legacy, err := repository.Node(ctx, direct.ID)
	if err != nil {
		t.Fatalf("read legacy direct Projection: %v", err)
	}
	if legacy.Source == nil || legacy.Source.HasFilesystem {
		t.Fatalf("preview mutated direct source = %#v", legacy.Source)
	}

	if _, err := service.MigrateIdentity(ctx, IdentityMigrationRequest{
		LibraryName:        "media",
		ConfirmationDigest: "sha256:" + strings.Repeat("0", 64),
		Actor:              namespace.Actor{Kind: namespace.ActorSystem},
	}); !errors.Is(err, ErrConfirmationMismatch) {
		t.Fatalf("wrong confirmation error = %v, want ErrConfirmationMismatch", err)
	}

	applied, err := service.MigrateIdentity(ctx, IdentityMigrationRequest{
		LibraryName:        "media",
		ConfirmationDigest: preview.ConfirmationDigest,
		Actor:              namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("MigrateIdentity confirm: %v", err)
	}
	if !applied.Applied || applied.ConfirmationDigest != preview.ConfirmationDigest {
		t.Errorf("applied = %#v", applied)
	}
	if version, logs := namespaceState(t, opened); version != versionBefore || logs != logsBefore+1 {
		t.Errorf("applied state = version %d logs %d, want %d/%d", version, logs, versionBefore, logsBefore+1)
	}
	projections, err := repository.Projections(ctx, "media")
	if err != nil {
		t.Fatalf("list migrated Projections: %v", err)
	}
	for _, projection := range projections {
		if projection.Node.Source == nil || !projection.Node.Source.HasFilesystem {
			t.Errorf("migrated Projection %d source = %#v", projection.Node.ID, projection.Node.Source)
			continue
		}
		source := *projection.Node.Source
		current, found := observedByProjectionID[projection.Node.ID]
		if !found {
			t.Fatalf("migrated Projection %d was not in preview", projection.Node.ID)
		}
		if source.DeviceID == current.DeviceID {
			t.Errorf(
				"migrated Projection %d device ID = %d, want retained remount mismatch from current %d",
				projection.Node.ID,
				source.DeviceID,
				current.DeviceID,
			)
		}
		verified, err := sourcefile.OpenVerified(source.Path, sourcefile.Identity{
			Filesystem: &sourcefile.FilesystemIdentity{
				Type: source.Filesystem.Type,
				ID:   source.Filesystem.ID,
			},
			DeviceID: source.DeviceID,
			Inode:    source.Inode,
			Size:     source.Size,
			MtimeNS:  source.MtimeNS,
		})
		if err != nil {
			t.Errorf("verify migrated Projection %d across device mismatch: %v", projection.Node.ID, err)
			continue
		}
		if err := verified.Close(); err != nil {
			t.Errorf("close verified migrated Projection %d: %v", projection.Node.ID, err)
		}
	}
	entries, err := repository.ProjectionBatchEntries(ctx, batchID)
	if err != nil {
		t.Fatalf("ProjectionBatchEntries: %v", err)
	}
	if len(entries) != 1 || !entries[0].Source.HasFilesystem {
		t.Fatalf("migrated Batch entry = %#v", entries)
	}
	if entries[0].Target != "Batch.mkv" || entries[0].ProjectionNodeID == nil {
		t.Errorf("Batch provenance changed = %#v", entries[0])
	} else if current := observedByProjectionID[*entries[0].ProjectionNodeID]; entries[0].Source.DeviceID == current.DeviceID {
		t.Errorf(
			"migrated Batch entry device ID = %d, want retained remount mismatch from current %d",
			entries[0].Source.DeviceID,
			current.DeviceID,
		)
	}
}

func TestServiceIdentityMigrationRejectsCtimeMismatch(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	library := createTestLibrary(t, repository)
	service := NewService(repository)
	path := writeIdentityMigrationSource(t, "ctime.mkv", "source")
	source, err := service.projectionSource(path)
	if err != nil {
		t.Fatalf("projectionSource: %v", err)
	}
	source.Filesystem = namespace.FilesystemIdentity{}
	source.HasFilesystem = false
	source.SourceCtimeNS ^= 1
	createLegacyProjectionWithSource(t, library, source, "Ctime.mkv", nil, repository)

	preview, err := service.MigrateIdentity(context.Background(), IdentityMigrationRequest{
		LibraryName: "media",
		Actor:       namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("MigrateIdentity preview: %v", err)
	}
	if len(preview.Candidates) != 0 || len(preview.Rejections) != 1 ||
		preview.Rejections[0].Code != "CtimeMismatch" {
		t.Fatalf("preview = %#v, want ctime rejection", preview)
	}
}

func TestServiceIdentityMigrationChangedSourceRejectsConfirmationWithoutMutation(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	library := createTestLibrary(t, repository)
	service := NewService(repository)
	path := writeIdentityMigrationSource(t, "changed.mkv", "source")
	node := createLegacyProjection(t, service, library, path, "Changed.mkv", nil)

	preview, err := service.MigrateIdentity(context.Background(), IdentityMigrationRequest{
		LibraryName: "media",
		Actor:       namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("MigrateIdentity preview: %v", err)
	}
	if err := os.WriteFile(path, []byte("changed source"), 0o600); err != nil {
		t.Fatalf("change source: %v", err)
	}
	_, err = service.MigrateIdentity(context.Background(), IdentityMigrationRequest{
		LibraryName:        "media",
		ConfirmationDigest: preview.ConfirmationDigest,
		Actor:              namespace.Actor{Kind: namespace.ActorSystem},
	})
	if !errors.Is(err, ErrConfirmationMismatch) {
		t.Fatalf("changed source confirmation error = %v, want ErrConfirmationMismatch", err)
	}
	current, err := repository.Node(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("read Projection after rejection: %v", err)
	}
	if current.Source == nil || current.Source.HasFilesystem {
		t.Errorf("changed source confirmation mutated identity = %#v", current.Source)
	}
}

func TestServiceIdentityMigrationRollsBackAllCandidatesAfterAuditFailure(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	library := createTestLibrary(t, repository)
	service := NewService(repository)
	createLegacyProjection(
		t,
		service,
		library,
		writeIdentityMigrationSource(t, "direct.mkv", "direct"),
		"Direct.mkv",
		nil,
	)
	batchID := createLegacyBatchProjection(
		t,
		service,
		library,
		writeIdentityMigrationSource(t, "batch.mkv", "batch"),
		"Batch.mkv",
	)
	preview, err := service.MigrateIdentity(context.Background(), IdentityMigrationRequest{
		LibraryName: "media",
		Actor:       namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("MigrateIdentity preview: %v", err)
	}
	failOperationLog(t, opened, "projection.identity.migrate")
	_, err = service.MigrateIdentity(context.Background(), IdentityMigrationRequest{
		LibraryName:        "media",
		ConfirmationDigest: preview.ConfirmationDigest,
		Actor:              namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err == nil || !strings.Contains(err.Error(), "forced operation log failure") {
		t.Fatalf("MigrateIdentity error = %v, want forced operation log failure", err)
	}
	projections, err := repository.Projections(context.Background(), "media")
	if err != nil {
		t.Fatalf("Projections after rollback: %v", err)
	}
	for _, projection := range projections {
		if projection.Node.Source == nil || projection.Node.Source.HasFilesystem {
			t.Errorf("Projection after rollback = %#v", projection.Node)
		}
	}
	entries, err := repository.ProjectionBatchEntries(context.Background(), batchID)
	if err != nil {
		t.Fatalf("ProjectionBatchEntries after rollback: %v", err)
	}
	if len(entries) != 1 || entries[0].Source.HasFilesystem {
		t.Errorf("Batch entry after rollback = %#v", entries)
	}
}

func writeIdentityMigrationSource(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile %q: %v", path, err)
	}
	return path
}

func createLegacyProjection(
	t *testing.T,
	service *Service,
	library namespace.Library,
	path string,
	target string,
	batchID *int64,
) namespace.Node {
	t.Helper()
	source, err := service.projectionSource(path)
	if err != nil {
		t.Fatalf("projectionSource: %v", err)
	}
	source.Filesystem = namespace.FilesystemIdentity{}
	source.HasFilesystem = false
	source.DeviceID ^= 1
	return createLegacyProjectionWithSource(t, library, source, target, batchID, service.repository)
}

func createLegacyProjectionWithSource(
	t *testing.T,
	library namespace.Library,
	source namespace.ProjectionSource,
	target string,
	batchID *int64,
	repository *namespace.Repository,
) namespace.Node {
	t.Helper()
	var node namespace.Node
	_, err := repository.Update(context.Background(), namespace.Operation{
		Name:  "projection.create",
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
		Details: namespace.OperationDetails{
			Paths:     []string{library.Name + ":/" + target},
			NodeKinds: []namespace.NodeKind{namespace.ProjectionFile},
		},
	}, func(transaction *namespace.Transaction) error {
		var err error
		node, err = transaction.CreateNode(context.Background(), namespace.CreateNode{
			ParentID:       library.RootNodeID,
			Name:           target,
			Kind:           namespace.ProjectionFile,
			Source:         &source,
			OriginScrapeID: batchID,
			Creator:        namespace.Actor{Kind: namespace.ActorSystem},
			Attributes: namespace.Attributes{
				Mode: library.ProjectionMode,
				UID:  library.UID,
				GID:  library.GID,
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("create legacy Projection: %v", err)
	}
	return node
}

func createLegacyBatchProjection(
	t *testing.T,
	service *Service,
	library namespace.Library,
	path string,
	target string,
) int64 {
	t.Helper()
	source, err := service.projectionSource(path)
	if err != nil {
		t.Fatalf("projectionSource: %v", err)
	}
	source.Filesystem = namespace.FilesystemIdentity{}
	source.HasFilesystem = false
	source.DeviceID ^= 1
	var batch namespace.ProjectionBatch
	_, err = service.repository.Update(context.Background(), namespace.Operation{
		Name:  "projection.batch.apply",
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
		Details: namespace.OperationDetails{
			Paths:     []string{library.Name + ":/" + target},
			NodeKinds: []namespace.NodeKind{namespace.ProjectionFile},
		},
	}, func(transaction *namespace.Transaction) error {
		var err error
		batch, err = transaction.CreateProjectionBatch(context.Background(), namespace.CreateProjectionBatch{
			LibraryID:      library.ID,
			ProfileName:    library.ProfileName,
			ProfileVersion: library.ProfileVersion,
			Digest:         "sha256:" + strings.Repeat("a", 64),
			AppliedByUID:   1000,
			AppliedByGID:   1000,
			AppliedAt:      1,
		})
		if err != nil {
			return err
		}
		if err := transaction.AssociateOperationWithProjectionBatch(context.Background(), batch.ID); err != nil {
			return err
		}
		node, err := transaction.CreateNode(context.Background(), namespace.CreateNode{
			ParentID:       library.RootNodeID,
			Name:           target,
			Kind:           namespace.ProjectionFile,
			Source:         &source,
			OriginScrapeID: &batch.ID,
			Creator:        namespace.Actor{Kind: namespace.ActorSystem},
			Attributes: namespace.Attributes{
				Mode: library.ProjectionMode,
				UID:  library.UID,
				GID:  library.GID,
			},
		})
		if err != nil {
			return err
		}
		return transaction.CreateProjectionBatchEntry(context.Background(), namespace.CreateProjectionBatchEntry{
			BatchID:          batch.ID,
			Position:         0,
			Source:           source,
			Target:           target,
			ProjectionNodeID: node.ID,
		})
	})
	if err != nil {
		t.Fatalf("create legacy Batch Projection: %v", err)
	}
	return batch.ID
}
