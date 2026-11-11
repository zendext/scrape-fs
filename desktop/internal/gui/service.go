package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/control"
	"github.com/zendext/scrape-fs/internal/desktopadmin"
	"github.com/zendext/scrape-fs/internal/diagnostics"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/projection"
)

type Request struct {
	Command string            `json:"command"`
	Library string            `json:"library"`
	Path    string            `json:"path"`
	Target  string            `json:"target"`
	Name    string            `json:"name"`
	Profile string            `json:"profile"`
	Source  string            `json:"source"`
	Digest  string            `json:"digest"`
	ID      int64             `json:"id"`
	Action  string            `json:"action"`
	Fields  map[string]string `json:"fields"`
	Text    string            `json:"text"`
	Raw     bool              `json:"raw"`
}

type Failure struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

func (failure *Failure) Error() string { return failure.Code }

type Response struct {
	Data  any      `json:"data,omitempty"`
	Error *Failure `json:"error,omitempty"`
}

type SystemState struct {
	Installed bool   `json:"installed"`
	Active    string `json:"active"`
	Substate  string `json:"substate"`
	Result    string `json:"result"`
	Available bool   `json:"available"`
}

type State struct {
	System        SystemState               `json:"system"`
	Daemon        *diagnostics.StatusResult `json:"daemon"`
	Error         *Failure                  `json:"error,omitempty"`
	SystemManaged bool                      `json:"system_managed"`
}

type ConfigPreview struct {
	Document       config.Document `json:"document"`
	PreviousDigest string          `json:"previous_digest"`
	Changes        []FieldChange   `json:"changes"`
	StorageChanged bool            `json:"storage_changed"`
}

type FieldChange struct {
	Key    string `json:"key"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type SaveResult struct {
	Document     config.Document `json:"document"`
	State        State           `json:"state"`
	RestartError *Failure        `json:"restart_error,omitempty"`
}

type Options struct {
	ConfigPath     string
	SocketPath     string
	OpenPath       func(string, bool) error
	ChooseSource   func() (string, error)
	SystemState    func(context.Context) SystemState
	Admin          func(context.Context, []string, any) (json.RawMessage, error)
	PreferencePath string
}

type Service struct {
	options                 Options
	mu                      sync.RWMutex
	activeSocket            string
	systemManaged           bool
	authorizedConfiguration *config.Document
}

func New(options Options) *Service {
	if options.ConfigPath == "" {
		options.ConfigPath = config.DefaultPath
	}
	socket := options.SocketPath
	if socket == "" {
		socket = control.DefaultSocketPath
		if document, err := readConfiguration(options.ConfigPath); err == nil && document.Fields != nil {
			socket = document.Fields["control.socket_path"]
		}
	}
	managed := options.ConfigPath == config.DefaultPath && options.SocketPath == ""
	if options.Admin != nil {
		managed = true
	} else {
		options.Admin = administer
	}
	if options.SystemState == nil {
		options.SystemState = systemState
	}
	return &Service{options: options, activeSocket: socket, systemManaged: managed}
}

func (service *Service) client() *control.Client {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return control.NewClient(service.activeSocket)
}

func (service *Service) Execute(ctx context.Context, request Request) Response {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	data, err := service.execute(ctx, request)
	if err != nil {
		return Response{Error: failure(err)}
	}
	return Response{Data: data}
}

func (service *Service) execute(ctx context.Context, request Request) (any, error) {
	client := service.client()
	switch request.Command {
	case "preferences":
		return service.preferences(), nil
	case "save_preferences":
		if err := service.savePreferences(request.Name); err != nil {
			return nil, &Failure{Code: "PreferenceSaveFailed"}
		}
		return struct{}{}, nil
	case "state":
		return service.state(ctx), nil
	case "libraries":
		return client.ListLibraries(ctx)
	case "browse":
		return client.Browse(ctx, request.Library, request.Path)
	case "create_library":
		profile, err := parseProfile(request.Profile)
		if err != nil {
			return nil, err
		}
		return client.CreateLibrary(ctx, request.Name, profile)
	case "rename_library":
		return client.RenameLibrary(ctx, request.Library, request.Name)
	case "profile_library":
		profile, err := parseProfile(request.Profile)
		if err != nil {
			return nil, err
		}
		return client.SetLibraryProfile(ctx, request.Library, profile)
	case "create_directory":
		return struct{}{}, client.CreateDirectory(ctx, request.Library, request.Path, request.Name, request.ID)
	case "move_directory":
		return struct{}{}, client.MoveDirectory(ctx, request.Library, request.Path, request.Target, request.ID)
	case "preview_removal":
		return client.PreviewRemoval(ctx, request.Library, request.Path)
	case "remove_tree":
		return struct{}{}, client.RemoveTree(ctx, request.Library, request.Path, request.Digest)
	case "create_mapping":
		return client.CreateProjection(ctx, projection.CreateRequest{SourcePath: request.Source, Target: request.Library + ":/" + request.Target})
	case "move_mapping":
		return client.MoveProjection(ctx, projection.MoveRequest{Target: request.Library + ":/" + request.Path, NewTarget: request.Library + ":/" + request.Target})
	case "remove_mapping":
		return client.RemoveProjection(ctx, projection.RemoveRequest{Target: request.Library + ":/" + request.Path})
	case "rebind_mapping":
		return client.RebindProjection(ctx, projection.RebindRequest{Target: request.Library + ":/" + request.Path, SourcePath: request.Source, ConfirmationDigest: request.Digest})
	case "doctor":
		return client.Doctor(ctx)
	case "open_folder", "open_file":
		return struct{}{}, service.open(ctx, request)
	case "choose_source":
		if service.options.ChooseSource == nil {
			return nil, &Failure{Code: "UnsupportedOperation"}
		}
		return service.options.ChooseSource()
	case "configuration":
		return readConfiguration(service.options.ConfigPath)
	case "authorize_configuration":
		if !service.systemManaged {
			return nil, &Failure{Code: "SystemUnavailable"}
		}
		encoded, err := service.options.Admin(ctx, []string{"read-config"}, nil)
		if err != nil {
			return nil, err
		}
		var document config.Document
		err = json.Unmarshal(encoded, &document)
		if err == nil {
			service.mu.Lock()
			service.authorizedConfiguration = &document
			service.mu.Unlock()
		}
		return document, err
	case "preview_configuration":
		return service.previewConfiguration(request)
	case "save_configuration":
		if !service.systemManaged {
			return nil, &Failure{Code: "SystemUnavailable"}
		}
		encoded, err := service.options.Admin(ctx, []string{"save-config"}, map[string]string{"text": request.Text, "expected": request.Digest})
		if err != nil {
			return nil, err
		}
		var document config.Document
		if err := json.Unmarshal(encoded, &document); err != nil {
			return nil, err
		}
		service.mu.Lock()
		service.authorizedConfiguration = &document
		service.mu.Unlock()
		result := SaveResult{Document: document}
		if request.Action == "restart" {
			if err := service.serviceAction(ctx, "restart"); err != nil {
				result.RestartError = failure(err)
			}
		}
		result.State = service.state(ctx)
		return result, nil
	case "service":
		if err := service.serviceAction(ctx, request.Action); err != nil {
			return nil, err
		}
		return service.state(ctx), nil
	default:
		return nil, &Failure{Code: "UnsupportedOperation"}
	}
}

func (service *Service) state(ctx context.Context) State {
	result := State{System: service.options.SystemState(ctx), SystemManaged: service.systemManaged}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := service.client().Status(probe)
	if err != nil {
		if document, readErr := service.runningCandidate(); readErr == nil && document.Fields != nil && service.options.SocketPath == "" {
			socket := document.Fields["control.socket_path"]
			if candidate, candidateErr := control.NewClient(socket).Status(probe); candidateErr == nil {
				service.mu.Lock()
				service.activeSocket = socket
				service.mu.Unlock()
				status, err = candidate, nil
			}
		}
	}
	if err != nil {
		result.Error = failure(err)
	} else {
		result.Daemon = &status
	}
	return result
}

func (service *Service) previewConfiguration(request Request) (ConfigPreview, error) {
	current, err := service.runningCandidate()
	if err != nil {
		return ConfigPreview{}, err
	}
	if current.Digest != request.Digest {
		return ConfigPreview{}, &Failure{Code: "ConfigurationChanged"}
	}
	var candidate config.Document
	if request.Raw {
		candidate, err = config.ParseDocument(request.Text)
	} else {
		candidate, err = config.PatchDocument(current.Text, request.Fields)
	}
	if err != nil {
		return ConfigPreview{}, &Failure{Code: "InvalidConfiguration", Detail: err.Error()}
	}
	preview := ConfigPreview{Document: candidate, PreviousDigest: current.Digest, Changes: []FieldChange{}}
	for _, key := range configKeys {
		if current.Fields[key] != candidate.Fields[key] {
			preview.Changes = append(preview.Changes, FieldChange{Key: key, Before: current.Fields[key], After: candidate.Fields[key]})
			if strings.HasPrefix(key, "storage.") {
				preview.StorageChanged = true
			}
		}
	}
	return preview, nil
}

func (service *Service) runningCandidate() (config.Document, error) {
	current, err := readConfiguration(service.options.ConfigPath)
	if os.IsPermission(err) {
		service.mu.RLock()
		if service.authorizedConfiguration != nil {
			current, err = *service.authorizedConfiguration, nil
		}
		service.mu.RUnlock()
	}
	return current, err
}

var configKeys = []string{"storage.database_path", "storage.overlay_store_path", "fuse.mountpoint", "fuse.entry_timeout", "fuse.attr_timeout", "fuse.negative_timeout", "control.socket_path", "control.socket_mode", "control.administrator_uid", "gc.orphan_grace_period"}

func (service *Service) serviceAction(ctx context.Context, action string) error {
	if !service.systemManaged {
		return &Failure{Code: "SystemUnavailable"}
	}
	if action != "start" && action != "stop" && action != "restart" {
		return &Failure{Code: "UnsupportedOperation"}
	}
	if _, err := service.options.Admin(ctx, []string{"service", action}, nil); err != nil {
		return err
	}
	if action == "stop" {
		return nil
	}
	expectedConfiguration := ""
	if document, err := service.runningCandidate(); err == nil {
		expectedConfiguration = document.EffectiveDigest
	}
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state := service.state(ctx)
		if state.Daemon != nil && state.Daemon.Daemon.Ready && state.Daemon.Mount.State == diagnostics.StateReady && state.Daemon.Socket.State == diagnostics.StateReady &&
			(expectedConfiguration == "" || state.Daemon.Daemon.ConfigurationDigest == expectedConfiguration) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return &Failure{Code: "ServiceNotReady"}
		case <-ticker.C:
		}
	}
}

func (service *Service) open(ctx context.Context, request Request) error {
	state := service.state(ctx)
	if state.Daemon == nil || !state.Daemon.Daemon.Ready || state.Daemon.Mount.State != diagnostics.StateReady {
		return &Failure{Code: "ServiceNotReady"}
	}
	if request.Path != "" {
		if _, err := relativeComponents(request.Path); err != nil {
			return err
		}
	}
	if _, err := relativeComponents(request.Library); err != nil || strings.Contains(request.Library, "/") {
		return &Failure{Code: "InvalidPath"}
	}
	libraries, err := service.client().ListLibraries(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, item := range libraries.Libraries {
		if item.Name == request.Library && item.Status == "ACTIVE" {
			found = true
		}
	}
	if !found {
		return &Failure{Code: "LibraryNotFound"}
	}
	path := filepath.Join(state.Daemon.Mount.Path, request.Library, filepath.FromSlash(request.Path))
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (request.Command == "open_folder" && !info.IsDir()) {
		return &Failure{Code: "InvalidPath"}
	}
	if service.options.OpenPath == nil {
		return &Failure{Code: "UnsupportedOperation"}
	}
	return service.options.OpenPath(path, info.IsDir())
}

func relativeComponents(path string) ([]string, error) {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\\x00") {
		return nil, &Failure{Code: "InvalidPath"}
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, &Failure{Code: "InvalidPath"}
		}
	}
	return parts, nil
}

func parseProfile(value string) (library.ProfileIdentifier, error) {
	switch value {
	case "jellyfin@1":
		return library.ProfileIdentifier{Name: "jellyfin", Version: 1}, nil
	case "plex@1":
		return library.ProfileIdentifier{Name: "plex", Version: 1}, nil
	}
	return library.ProfileIdentifier{}, &Failure{Code: "ProfileNotFound"}
}

func readConfiguration(path string) (config.Document, error) {
	file, err := os.Open(path)
	if err != nil {
		return config.Document{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, desktopadmin.MaxConfigBytes+1))
	if err != nil {
		return config.Document{}, err
	}
	if len(data) > desktopadmin.MaxConfigBytes {
		return config.Document{}, &Failure{Code: "InvalidConfiguration"}
	}
	document, _ := config.ParseDocument(string(data))
	return document, nil
}

func systemState(ctx context.Context) SystemState {
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probe, "/usr/bin/systemctl", "show", "scrapefsd.service", "--property=LoadState,ActiveState,SubState,Result", "--no-pager").Output()
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	return SystemState{Available: err == nil, Installed: values["LoadState"] != "" && values["LoadState"] != "not-found", Active: values["ActiveState"], Substate: values["SubState"], Result: values["Result"]}
}

func administer(ctx context.Context, arguments []string, input any) (json.RawMessage, error) {
	for path := desktopadmin.HelperPath; path != "/"; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, &Failure{Code: "HelperNotInstalled"}
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 || info.Mode()&os.ModeSymlink != 0 {
			return nil, &Failure{Code: "UnsafeHelper"}
		}
	}
	command := exec.CommandContext(ctx, "/usr/bin/pkexec", append([]string{desktopadmin.HelperPath}, arguments...)...)
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		command.Stdin = bytes.NewReader(encoded)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "configuration changed") {
			return nil, &Failure{Code: "ConfigurationChanged"}
		}
		return nil, &Failure{Code: "AuthorizationFailed"}
	}
	return output, nil
}

func failure(err error) *Failure {
	var typed *Failure
	if errors.As(err, &typed) {
		return typed
	}
	var remote *control.RemoteError
	if errors.As(err, &remote) {
		if remote.Code == "Internal" && strings.Contains(remote.Message, "unsupported control command") {
			return &Failure{Code: "UpgradeRequired"}
		}
		return &Failure{Code: remote.Code}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &Failure{Code: "OperationTimeout"}
	}
	if os.IsPermission(err) {
		return &Failure{Code: "PermissionDenied"}
	}
	if os.IsNotExist(err) || errors.Is(err, syscall.ECONNREFUSED) {
		return &Failure{Code: "Unavailable"}
	}
	return &Failure{Code: "OperationFailed"}
}
