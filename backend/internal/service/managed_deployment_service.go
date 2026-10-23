package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrManagedDeploymentRequired = infraerrors.Conflict("MANAGED_DEPLOYMENT_REQUIRED", "use the managed deployment workflow")
var ErrManagedDeploymentUnavailable = infraerrors.ServiceUnavailable("MANAGED_DEPLOYMENT_UNAVAILABLE", "deployment executor is unavailable; check the existing task before retrying")

var deploymentIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type DeploymentCandidate struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Source  string `json:"source"`
	Summary string `json:"summary"`
	Channel string `json:"channel"`
	Reason  string `json:"reason,omitempty"`
}

type DeploymentJob struct {
	Mode            string   `json:"mode,omitempty"`
	LiveHandoff     bool     `json:"live_handoff,omitempty"`
	PreviousSource  string   `json:"previous_source,omitempty"`
	PreviousVersion string   `json:"previous_version,omitempty"`
	ID              string   `json:"id"`
	CandidateID     string   `json:"candidate_id"`
	Version         string   `json:"version"`
	Source          string   `json:"source"`
	Phase           string   `json:"phase"`
	ExpiresAt       float64  `json:"expires_at,omitempty"`
	Actions         []string `json:"actions"`
	ErrorCode       string   `json:"error_code,omitempty"`
	Reused          bool     `json:"reused,omitempty"`
}

type DeploymentSnapshot struct {
	Enabled      bool                  `json:"enabled"`
	CatalogError string                `json:"catalog_error,omitempty"`
	Candidates   []DeploymentCandidate `json:"candidates"`
	Job          *DeploymentJob        `json:"job,omitempty"`
	RollbackJob  *DeploymentJob        `json:"rollback_job,omitempty"`
}

type ManagedDeploymentService struct {
	socket string
	client *http.Client
}

func NewManagedDeploymentService(socket string) *ManagedDeploymentService {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return &ManagedDeploymentService{socket: socket, client: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		}},
	}}
}

func (s *ManagedDeploymentService) Enabled() bool { return s != nil && s.socket != "" }

func (s *ManagedDeploymentService) Snapshot(ctx context.Context, refresh bool) (*DeploymentSnapshot, error) {
	if !s.Enabled() {
		return &DeploymentSnapshot{Candidates: []DeploymentCandidate{}}, nil
	}
	path := "/status"
	if refresh {
		path += "?refresh=true"
	}
	return s.request(ctx, "GET", path, nil)
}

func (s *ManagedDeploymentService) Command(ctx context.Context, action, id string) (*DeploymentSnapshot, error) {
	if !deploymentIDPattern.MatchString(id) || (action != "prepare" && action != "activate" && action != "rollback") {
		return nil, infraerrors.BadRequest("INVALID_DEPLOYMENT_ACTION", "invalid deployment action or identifier")
	}
	body, _ := json.Marshal(map[string]string{"id": id})
	return s.request(ctx, "POST", "/"+action, body)
}

func (s *ManagedDeploymentService) request(ctx context.Context, method, path string, body []byte) (*DeploymentSnapshot, error) {
	if !s.Enabled() || !filepath.IsAbs(s.socket) {
		return nil, ErrManagedDeploymentUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://deployment"+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrManagedDeploymentUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, ErrManagedDeploymentUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusConflict {
		return nil, infraerrors.Conflict("DEPLOYMENT_CONFLICT", "deployment state changed or another operation is active; refresh the result")
	}
	if res.StatusCode != http.StatusOK {
		return nil, ErrManagedDeploymentUnavailable
	}
	var result DeploymentSnapshot
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&result); err != nil || !result.Enabled {
		return nil, ErrManagedDeploymentUnavailable
	}
	return &result, nil
}
