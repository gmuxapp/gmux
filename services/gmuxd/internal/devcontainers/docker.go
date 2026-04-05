package devcontainers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// container holds the subset of docker inspect output we need.
type container struct {
	ID     string
	Name   string
	Env    []string
	Labels map[string]string
	IP     string // first IP from network settings
}

// dockerRunner abstracts Docker CLI operations for testing.
type dockerRunner interface {
	// list returns all running containers with their metadata.
	list(ctx context.Context) ([]container, error)
	// readToken reads the gmuxd auth token from inside a container.
	readToken(ctx context.Context, id string) (string, error)
	// events streams container start/die events. The channel closes when
	// the stream ends (Docker daemon restart, context cancellation, etc).
	events(ctx context.Context) (<-chan struct{}, error)
}

// cliDocker implements dockerRunner using the docker CLI.
type cliDocker struct{}

func (c *cliDocker) list(ctx context.Context) ([]container, error) {
	// Get all running container IDs.
	psCmd := exec.CommandContext(ctx, "docker", "ps", "-q", "--no-trunc")
	psOut, err := psCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	ids := strings.Fields(strings.TrimSpace(string(psOut)))
	if len(ids) == 0 {
		return nil, nil
	}

	// Inspect all at once for labels, env, and network info.
	args := append([]string{"inspect"}, ids...)
	inspectCmd := exec.CommandContext(ctx, "docker", args...)
	inspectOut, err := inspectCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}

	var raw []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Env    []string          `json:"Env"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(inspectOut, &raw); err != nil {
		return nil, fmt.Errorf("parsing docker inspect: %w", err)
	}

	containers := make([]container, 0, len(raw))
	for _, r := range raw {
		c := container{
			ID:     r.ID,
			Name:   strings.TrimPrefix(r.Name, "/"),
			Env:    r.Config.Env,
			Labels: r.Config.Labels,
		}
		for _, net := range r.NetworkSettings.Networks {
			if net.IPAddress != "" {
				c.IP = net.IPAddress
				break
			}
		}
		containers = append(containers, c)
	}
	return containers, nil
}

func (c *cliDocker) events(ctx context.Context) (<-chan struct{}, error) {
	// We only use each event as a trigger to run a full scan, so we don't
	// need to parse the event body. Any output line from docker events is
	// a signal to reconcile.
	cmd := exec.CommandContext(ctx, "docker", "events",
		"--filter", "type=container",
		"--filter", "event=start",
		"--filter", "event=die")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("docker events: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker events: start: %w", err)
	}

	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		defer cmd.Wait()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case ch <- struct{}{}:
			case <-ctx.Done():
				return
			default:
				// Drop the trigger if the consumer is slow; the next scan
				// will pick up the current state anyway.
			}
		}
	}()
	return ch, nil
}

func (c *cliDocker) readToken(ctx context.Context, id string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", id,
		"sh", "-c", `cat "${XDG_DATA_HOME:-$HOME/.local/share}/gmuxd/auth-token"`)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reading token from container %s: %w", id[:min(12, len(id))], err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("empty token in container %s", id[:min(12, len(id))])
	}
	return token, nil
}
