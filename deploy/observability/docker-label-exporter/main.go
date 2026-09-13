// Command docker-label-exporter is a tiny Prometheus exporter
// (docs/observability.md) that maps a running container's full ID to its
// real Docker name and compose project/service labels — the piece
// cAdvisor's containerd factory cannot supply on Docker Desktop's
// containerd-snapshotter storage backend. Confirmed directly (`ctr
// containers info` inside the "moby" namespace dockerd uses): Docker's own
// container name and compose labels live entirely in dockerd's own
// metadata store and are never pushed down into containerd itself — only
// `com.docker/engine.bundle.path` survives that trip. Reading the same
// information straight from the Docker Engine API instead — which has
// always carried it correctly, independent of the storage/snapshotter
// backend underneath it — is the standard fix, not a Docker Desktop
// settings change (docker-compose.observability.yml's own cadvisor service
// comment has the full story, including why cAdvisor is pointed at
// containerd at all).
//
// Every metric this emits is a JOIN KEY, never content: container id,
// name, and the two compose labels that already identify it in `docker
// ps` — nothing about what a container is actually doing. A Grafana panel
// joins it against cAdvisor's own id-keyed series with a PromQL
// `* on(id) group_left(name, compose_service)` (see
// nexus-infrastructure.json).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// dockerSocket is the standard Docker Engine API socket path — the same
// one docker-compose.observability.yml already mounts read-only into
// promtail for its own Docker service discovery.
const dockerSocket = "/var/run/docker.sock"

// composeProject scopes this exporter to one project's own containers,
// the same project name every compose file in this repo already shares
// (docker-compose.yml's own `name: nexus-agent-demo` directive) —
// overridable so this same image could map a different project without a
// code change.
var composeProject = envOr("COMPOSE_PROJECT", "nexus-agent-demo")

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// dockerContainer is the subset of the Docker Engine API's
// GET /containers/json response this exporter actually reads.
type dockerContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
}

func newDockerClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", dockerSocket)
			},
		},
	}
}

// listContainers calls the Docker Engine API's own container list,
// server-side filtered to this compose project's label — the daemon does
// the filtering (it has always had this metadata correctly, independent
// of the containerd-snapshotter storage backend cAdvisor's containerd
// factory can't read it from), not this exporter.
func listContainers(ctx context.Context, client *http.Client) ([]dockerContainer, error) {
	filters := fmt.Sprintf(`{"label":["com.docker.compose.project=%s"]}`, composeProject)
	u := "http://unix/containers/json?filters=" + url.QueryEscape(filters)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build docker API request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dial docker API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker API returned %s", resp.Status)
	}

	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("decode docker API response: %w", err)
	}
	return containers, nil
}

func handleMetrics(client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		containers, err := listContainers(r.Context(), client)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			log.Println("docker-label-exporter:", err)
			return
		}

		w.Header().Set("content-type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# HELP container_id_info Maps a container's full ID to its Docker name and compose labels — a join key, value is always 1.") //nolint:errcheck // best-effort write to a scrape response
		fmt.Fprintln(w, "# TYPE container_id_info gauge")                                                                                             //nolint:errcheck // best-effort write to a scrape response
		for _, c := range containers {
			name := c.ID
			if len(c.Names) > 0 {
				// Docker's own Names entries carry a leading slash
				// ("/nexus-agent-demo-postgres-1") — stripped so this
				// matches `docker ps`'s own display.
				name = trimLeadingSlash(c.Names[0])
			}
			fmt.Fprintf(w, "container_id_info{id=%q,name=%q,compose_service=%q,compose_project=%q} 1\n", //nolint:errcheck // best-effort write to a scrape response
				c.ID, name, c.Labels["com.docker.compose.service"], c.Labels["com.docker.compose.project"])
		}
	}
}

func trimLeadingSlash(s string) string {
	if len(s) > 0 && s[0] == '/' {
		return s[1:]
	}
	return s
}

func main() {
	addr := envOr("LISTEN_ADDR", ":9101")
	client := newDockerClient()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", handleMetrics(client))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("docker-label-exporter listening on %s (compose project %q)", addr, composeProject)
	log.Fatal(http.ListenAndServe(addr, mux)) //nolint:gosec // internal-only, no TLS/timeouts needed for a scrape-only sidecar behind the compose network
}
