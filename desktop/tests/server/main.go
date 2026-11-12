//go:build linux && server && desktoptest

// This test-only application runs the real Wails bindings and control service
// against disposable data. OS dialogs and privileged actions are recorded locally.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/zendext/scrape-fs/desktop/internal/gui"
	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/control"
	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/diagnostics"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

type diagnosticService struct {
	mu      sync.RWMutex
	service *diagnostics.Service
}

func (service *diagnosticService) Status(ctx context.Context) diagnostics.StatusResult {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.service.Status(ctx)
}
func (service *diagnosticService) Doctor(ctx context.Context) diagnostics.DoctorResult {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.service.Doctor(ctx)
}

func main() {
	port := flag.Int("port", 9481, "Loopback test port")
	assets := flag.String("assets", "frontend/dist", "Built frontend")
	flag.Parse()
	root, err := os.MkdirTemp("", "scrapefs-gui-test-")
	must(err)
	defer os.RemoveAll(root)
	ctx := context.Background()
	opened, err := database.OpenWithOptions(ctx, filepath.Join(root, "state", "scrapefs.db"), database.Options{LockPath: filepath.Join(root, "run", "daemon.lock")})
	must(err)
	defer opened.Close()
	repository := namespace.NewRepository(opened)
	store, err := objectstore.OpenWithOptions(filepath.Join(root, "objects"), opened, objectstore.Options{OrphanGracePeriod: time.Hour, Repository: repository})
	must(err)
	defer store.Close()
	libraries := library.NewServiceWithObjectReclaimer(repository, store)
	actor := namespace.Actor{Kind: namespace.ActorSystem}
	created, err := libraries.Create(ctx, library.CreateRequest{Name: "media", Profile: library.ProfileIdentifier{Name: "jellyfin", Version: 1}, Actor: actor, Modes: library.DefaultModes})
	must(err)
	must(libraries.CreateDirectory(ctx, "media", "", "Shows", created.RootNodeID, actor))
	directory, err := libraries.Browse(ctx, "media", "Shows")
	must(err)
	source := filepath.Join(root, "source.mkv")
	must(os.WriteFile(source, []byte("original media"), 0o600))
	_, err = projection.NewService(repository).Create(ctx, projection.CreateRequest{SourcePath: source, Target: "media:/Shows/Long media filename with 中文字符 and a source mapping.mkv", Actor: actor})
	must(err)
	for index := 0; index < 140; index++ {
		handle, _, err := store.Create(ctx, namespace.Operation{Name: "test.seed", Actor: actor, Details: namespace.OperationDetails{Paths: []string{"media:/Shows"}, NodeKinds: []namespace.NodeKind{namespace.OverlayFile}}}, namespace.CreateOverlay{ParentID: directory.ID, Name: fmt.Sprintf("episode-%03d.nfo", index), Creator: actor, Attributes: namespace.Attributes{Mode: 0o644}}, bytes.NewBufferString("metadata"))
		must(err)
		must(handle.Close())
	}
	index := namespace.NewIndex(repository)
	_, err = index.Rebuild(ctx)
	must(err)
	repository.AddCommitListener(func(namespace.Commit) {
		_, err := index.Rebuild(context.Background())
		if err != nil {
			log.Print(err)
		}
	})
	socket := filepath.Join(root, "control.sock")
	mount := filepath.Join(root, "mount")
	must(os.MkdirAll(filepath.Join(mount, "media", "Shows"), 0o700))
	configuration, err := config.PatchDocument("# retained fixture comment\n", map[string]string{"storage.database_path": filepath.Join(root, "state", "scrapefs.db"), "storage.overlay_store_path": filepath.Join(root, "objects"), "fuse.mountpoint": mount, "control.socket_path": socket, "control.administrator_uid": fmt.Sprint(os.Geteuid())})
	must(err)
	configPath := filepath.Join(root, "config.toml")
	must(os.WriteFile(configPath, []byte(configuration.Text), 0o600))
	runtime := diagnostics.NewRuntime(mount, socket)
	runtime.SetMount(func() error { return nil })
	runtime.SetReady(true)
	diagnostic := &diagnosticService{service: diagnostics.NewService(opened, repository, index, store, runtime, "desktop-test", configuration.EffectiveDigest)}
	server, err := control.StartWithDiagnostics(socket, 0o660, uint32(os.Geteuid()), projectionbatch.NewService(repository), libraries, projection.NewService(repository), store, diagnostic)
	must(err)
	defer server.Close()
	runtime.SetSocket(server.Check)
	var platformMu sync.Mutex
	active := true
	service := gui.New(gui.Options{ConfigPath: configPath, SocketPath: socket, PreferencePath: filepath.Join(root, "preferences.json"),
		SystemState: func(context.Context) gui.SystemState {
			platformMu.Lock()
			defer platformMu.Unlock()
			state := "inactive"
			if active {
				state = "active"
			}
			return gui.SystemState{Installed: true, Available: true, Active: state, Substate: state}
		},
		OpenPath: func(path string, folder bool) error {
			return os.WriteFile(filepath.Join(root, "opened.json"), []byte(fmt.Sprintf("%t:%s", folder, path)), 0o600)
		},
		ChooseSource: func() (string, error) { return source, nil },
		Admin: func(_ context.Context, args []string, input any) (json.RawMessage, error) {
			platformMu.Lock()
			defer platformMu.Unlock()
			data, err := os.ReadFile(configPath)
			if err != nil {
				return nil, err
			}
			current, _ := config.ParseDocument(string(data))
			switch args[0] {
			case "read-config":
				return json.Marshal(current)
			case "save-config":
				encoded, _ := json.Marshal(input)
				var request map[string]string
				must(json.Unmarshal(encoded, &request))
				if request["expected"] != current.Digest {
					return nil, &gui.Failure{Code: "ConfigurationChanged"}
				}
				next, err := config.ParseDocument(request["text"])
				if err != nil {
					return nil, err
				}
				must(os.WriteFile(configPath+".previous", data, 0o600))
				must(os.WriteFile(configPath, []byte(next.Text), 0o600))
				return json.Marshal(next)
			case "service":
				active = args[1] != "stop"
				runtime.SetReady(active)
				if active {
					diagnostic.mu.Lock()
					diagnostic.service = diagnostics.NewService(opened, repository, index, store, runtime, "desktop-test", current.EffectiveDigest)
					diagnostic.mu.Unlock()
				}
				return json.RawMessage(`{}`), nil
			}
			return nil, fmt.Errorf("invalid test operation")
		},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /test/observations", func(writer http.ResponseWriter, _ *http.Request) {
		platformMu.Lock()
		defer platformMu.Unlock()
		opened, _ := os.ReadFile(filepath.Join(root, "opened.json"))
		media, _ := os.ReadFile(source)
		current, _ := os.ReadFile(configPath)
		backup, _ := os.ReadFile(configPath + ".previous")
		_ = json.NewEncoder(writer).Encode(map[string]string{"opened": string(opened), "source": string(media), "configuration": string(current), "backup": string(backup)})
	})
	mux.Handle("/", http.FileServer(http.Dir(*assets)))
	app := application.New(application.Options{Name: "ScrapeFS test", Services: []application.Service{application.NewService(service)}, Assets: application.AssetOptions{Handler: mux, DisableLogging: true}, Server: application.ServerOptions{Host: "127.0.0.1", Port: *port}})
	log.Printf("isolated fixture: %s", root)
	must(app.Run())
}
func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
