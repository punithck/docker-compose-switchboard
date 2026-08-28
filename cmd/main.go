package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

//go:embed static/index.html
var indexHTML string

type Config struct {
	Projects []ProjectConfig `json:"projects"`
}

type ProjectConfig struct {
	Name         string          `json:"name"`
	Workdir      string          `json:"workdir"`
	ComposeFiles []string        `json:"composeFiles,omitempty"`
	Services     []ServiceConfig `json:"services,omitempty"`
}

type ServiceConfig struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Group       string   `json:"group,omitempty"`
	Port        string   `json:"port,omitempty"`
	URL         string   `json:"url,omitempty"`
	Description string   `json:"description,omitempty"`
	DependsOn   []string `json:"dependsOn,omitempty"`
}

type ServiceView struct {
	ID          string   `json:"id"`
	Project     string   `json:"project"`
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Group       string   `json:"group"`
	Port        string   `json:"port,omitempty"`
	URL         string   `json:"url,omitempty"`
	Description string   `json:"description,omitempty"`
	DependsOn   []string `json:"dependsOn,omitempty"`
	Status      string   `json:"status"`
	Health      string   `json:"health,omitempty"`
	State       string   `json:"state,omitempty"`
}

type Server struct {
	config Config
	runner CommandRunner
	mu     sync.Mutex
}

type CommandRunner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

type DockerRunner struct{}

type ActionRequest struct {
	Action string `json:"action"`
}

type ActionResponse struct {
	Output  string      `json:"output"`
	Service ServiceView `json:"service"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type composePS struct {
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to switchboard config json")
	addr := flag.String("addr", ":9090", "HTTP address")
	flag.Parse()

	// configure zerolog for human-friendly console output
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	zlog.Logger = zlog.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	config, err := loadConfig(*configPath)
	if err != nil {
		zlog.Fatal().Err(err).Msg("failed to load config")
	}

	server := &Server{
		config: config,
		runner: DockerRunner{},
	}

	mux := http.NewServeMux()
	server.routes(mux)

	zlog.Info().Str("addr", *addr).Msg("service switchboard running")
	zlog.Fatal().Err(http.ListenAndServe(*addr, mux)).Msg("http server exited")
}

func defaultConfigPath() string {
	if path := strings.TrimSpace(os.Getenv("SWITCHBOARD_CONFIG")); path != "" {
		return path
	}
	return "switchboard.json"
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var config Config
		if err := json.Unmarshal(data, &config); err != nil {
			return Config{}, fmt.Errorf("read %s: %w", path, err)
		}
		return normalizeConfig(config)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}

	config, err := discoverConfig(".")
	if err != nil {
		return Config{}, err
	}
	return normalizeConfig(config)
}

func discoverConfig(workdir string) (Config, error) {
	names := []string{
		"compose.yaml",
		"compose.yml",
		"docker-compose.yaml",
		"docker-compose.yml",
	}

	var files []string
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(workdir, name)); err == nil {
			files = append(files, name)
		}
	}

	abs, err := filepath.Abs(workdir)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Projects: []ProjectConfig{
			{
				Name:         filepath.Base(abs),
				Workdir:      abs,
				ComposeFiles: files,
			},
		},
	}, nil
}

func normalizeConfig(config Config) (Config, error) {
	for i := range config.Projects {
		project := &config.Projects[i]
		if project.Name == "" {
			return Config{}, fmt.Errorf("project %d is missing name", i)
		}
		if project.Workdir == "" {
			return Config{}, fmt.Errorf("project %q is missing workdir", project.Name)
		}
		abs, err := filepath.Abs(project.Workdir)
		if err != nil {
			return Config{}, fmt.Errorf("project %q workdir: %w", project.Name, err)
		}
		project.Workdir = abs
	}
	return config, nil
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/services", s.handleServices)
	mux.HandleFunc("/api/services/", s.handleServiceAction)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	services, err := s.services(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": services})
}

func (s *Server) handleServiceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/services/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing service id")
		return
	}

	var req ActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// log incoming request
	zlog.Info().Str("service_id", id).Str("action", req.Action).Msg("received action request")

	s.mu.Lock()
	defer s.mu.Unlock()

	output, err := s.runAction(r.Context(), id, req.Action)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error()+"\n"+output)
		return
	}

	service, err := s.service(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ActionResponse{
		Output:  output,
		Service: service,
	})
}

func (s *Server) services(ctx context.Context) ([]ServiceView, error) {
	all := []ServiceView{}
	for _, project := range s.config.Projects {
		statuses, err := s.statuses(ctx, project)
		if err != nil {
			return nil, err
		}

		configured, err := s.configuredServices(ctx, project)
		if err != nil {
			return nil, err
		}

		for _, service := range configured {
			view := serviceView(project, service)
			if status, ok := statuses[service.Name]; ok {
				view.Status = normalizeStatus(status.State)
				view.State = status.State
				view.Health = status.Health
			} else {
				view.Status = "stopped"
			}
			all = append(all, view)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Project != all[j].Project {
			return all[i].Project < all[j].Project
		}
		if all[i].Group != all[j].Group {
			return all[i].Group < all[j].Group
		}
		return all[i].DisplayName < all[j].DisplayName
	})
	return all, nil
}

func (s *Server) service(ctx context.Context, id string) (ServiceView, error) {
	services, err := s.services(ctx)
	if err != nil {
		return ServiceView{}, err
	}
	for _, service := range services {
		if service.ID == id {
			return service, nil
		}
	}
	return ServiceView{}, fmt.Errorf("unknown service %q", id)
}

func (s *Server) configuredServices(ctx context.Context, project ProjectConfig) ([]ServiceConfig, error) {
	if len(project.Services) > 0 {
		return project.Services, nil
	}
	if len(project.ComposeFiles) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	args := composeArgs(project, "config", "--services")
	out, err := s.runner.Run(ctx, project.Workdir, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: list services: %w\n%s", project.Name, err, strings.TrimSpace(string(out)))
	}

	var services []ServiceConfig
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		services = append(services, ServiceConfig{Name: name})
	}
	return services, nil
}

func (s *Server) statuses(ctx context.Context, project ProjectConfig) (map[string]composePS, error) {
	statuses := make(map[string]composePS)
	if len(project.ComposeFiles) == 0 {
		return statuses, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	args := composeArgs(project, "ps", "--all", "--format", "json")
	out, err := s.runner.Run(ctx, project.Workdir, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: read service status: %w\n%s", project.Name, err, strings.TrimSpace(string(out)))
	}

	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return statuses, nil
	}

	var rows []composePS
	if err := json.Unmarshal(trimmed, &rows); err == nil {
		for _, row := range rows {
			statuses[row.Service] = row
		}
		return statuses, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	for {
		var row composePS
		if err := decoder.Decode(&row); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("%s: parse docker compose ps output: %w", project.Name, err)
		}
		statuses[row.Service] = row
	}
	return statuses, nil
}

func (s *Server) runAction(ctx context.Context, id string, action string) (string, error) {
	project, service, err := s.findService(ctx, id)
	if err != nil {
		return "", err
	}
	if len(project.ComposeFiles) == 0 {
		return "", fmt.Errorf("project %q has no compose files configured", project.Name)
	}

	var args []string
	switch action {
	case "start":
		targets := append([]string{}, service.DependsOn...)
		targets = append(targets, service.Name)
		args = composeArgs(project, append([]string{"up", "-d"}, targets...)...)
	case "stop":
		args = composeArgs(project, "stop", service.Name)
	case "restart":
		args = composeArgs(project, "restart", service.Name)
	default:
		return "", fmt.Errorf("unsupported action %q", action)
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// log the docker command about to be executed
	zlog.Info().Str("workdir", project.Workdir).Strs("args", args).Msg("executing docker command")

	out, err := s.runner.Run(ctx, project.Workdir, args...)
	trimmedOut := strings.TrimSpace(string(out))
	if err != nil {
		zlog.Error().Err(err).
			Str("workdir", project.Workdir).
			Strs("args", args).
			Str("output", trimmedOut).
			Msg("docker command failed")

		// detect common container name conflict and provide actionable suggestion
		suggestion := ""
		if strings.Contains(trimmedOut, "already in use by container") || strings.Contains(trimmedOut, "Conflict. The container name") {
			composeFile := "compose.yaml"
			if len(project.ComposeFiles) > 0 {
				composeFile = project.ComposeFiles[0]
			}
			suggestion = fmt.Sprintf("container name conflict: remove the existing container (e.g. `docker rm -f <id>`), or bring the project down (`docker compose -f %s down`), or run with a different project name: `COMPOSE_PROJECT_NAME=yourname docker compose -f %s up -d ...`", composeFile, composeFile)
			userErr := fmt.Errorf("%s: %v", suggestion, err)
			zlog.Error().Err(userErr).Str("suggestion", suggestion).Msg("container name conflict detected")
			return trimmedOut, userErr
		}

		return trimmedOut, err
	}
	return trimmedOut, nil
}

func (s *Server) findService(ctx context.Context, id string) (ProjectConfig, ServiceConfig, error) {
	for _, project := range s.config.Projects {
		services, err := s.configuredServices(ctx, project)
		if err != nil {
			return ProjectConfig{}, ServiceConfig{}, err
		}
		for _, service := range services {
			if serviceID(project.Name, service.Name) == id {
				return project, service, nil
			}
		}
	}
	return ProjectConfig{}, ServiceConfig{}, fmt.Errorf("unknown service %q", id)
}

func serviceView(project ProjectConfig, service ServiceConfig) ServiceView {
	displayName := service.DisplayName
	if displayName == "" {
		displayName = service.Name
	}
	group := service.Group
	if group == "" {
		group = "default"
	}
	return ServiceView{
		ID:          serviceID(project.Name, service.Name),
		Project:     project.Name,
		Name:        service.Name,
		DisplayName: displayName,
		Group:       group,
		Port:        service.Port,
		URL:         service.URL,
		Description: service.Description,
		DependsOn:   service.DependsOn,
	}
}

func serviceID(projectName, serviceName string) string {
	return projectName + ":" + serviceName
}

func normalizeStatus(state string) string {
	switch strings.ToLower(state) {
	case "running":
		return "running"
	case "exited", "created", "paused", "restarting", "removing", "dead":
		return "stopped"
	default:
		if state == "" {
			return "stopped"
		}
		return strings.ToLower(state)
	}
}

func composeArgs(project ProjectConfig, args ...string) []string {
	base := []string{"compose"}
	for _, file := range project.ComposeFiles {
		base = append(base, "-f", file)
	}
	return append(base, args...)
}

func (DockerRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ErrorResponse{Error: strings.TrimSpace(message)})
}
