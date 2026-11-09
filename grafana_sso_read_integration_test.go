//go:build integration

package mcpgrafana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGrafanaSSODockerMCPReadTools(t *testing.T) {
	if _, err := os.Stat(".env"); err != nil {
		t.Skip(".env is required for the Docker SSO integration test")
	}
	image := firstNonEmpty(os.Getenv("GRAFANA_SSO_TEST_IMAGE"), "mcp-grafana:sso-alpine-test")
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("Docker image %q is unavailable; build it with Dockerfile.alpine first", image)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	command := exec.Command("docker", "run", "--rm", "-i", "--env-file", ".env", image, "-t", "stdio")
	var serverLog bytes.Buffer
	command.Stderr = &serverLog
	client := mcp.NewClient(&mcp.Implementation{Name: "grafana-sso-read-test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect to Docker MCP server: %v\nSSO log:\n%s", err, serverLog.String())
	}
	defer session.Close()

	if strings.Contains(serverLog.String(), "Starting Chromium for Grafana SSO") {
		t.Fatal("Grafana SSO login started before the first MCP tool request")
	}

	firstRequestStarted := time.Now()
	dashboardJSON := callMCPToolJSON(t, ctx, session, &serverLog, "search_dashboards", map[string]any{"limit": 5})
	firstRequestDuration := time.Since(firstRequestStarted)
	if got := countSSOLoginStarts(serverLog.String()); got != 1 {
		t.Fatalf("SSO login count after first MCP request = %d, want 1", got)
	}
	reuseDurations := make([]time.Duration, 0, 3)
	for i := 0; i < cap(reuseDurations); i++ {
		started := time.Now()
		callMCPToolJSON(t, ctx, session, &serverLog, "search_dashboards", map[string]any{"limit": 5})
		reuseDurations = append(reuseDurations, time.Since(started))
	}
	sort.Slice(reuseDurations, func(i, j int) bool { return reuseDurations[i] < reuseDurations[j] })
	medianReuseDuration := reuseDurations[len(reuseDurations)/2]
	t.Logf("SSO MCP performance: first request=%s, reused-session median=%s", firstRequestDuration, medianReuseDuration)
	if medianReuseDuration >= firstRequestDuration {
		t.Fatalf("reused-session median %s was not faster than first request %s", medianReuseDuration, firstRequestDuration)
	}
	if got := countSSOLoginStarts(serverLog.String()); got != 1 {
		t.Fatalf("SSO login count after reused requests = %d, want 1", got)
	}

	var dashboards struct {
		Dashboards []struct {
			Title string `json:"title"`
			UID   string `json:"uid"`
		} `json:"dashboards"`
	}
	if err := json.Unmarshal(dashboardJSON, &dashboards); err != nil {
		t.Fatalf("decode search_dashboards result: %v\nSSO log:\n%s", err, serverLog.String())
	}
	dashboardNames := make([]string, 0, len(dashboards.Dashboards))
	for _, dashboard := range dashboards.Dashboards {
		dashboardNames = append(dashboardNames, fmt.Sprintf("%s (%s)", dashboard.Title, dashboard.UID))
	}
	t.Logf("Dashboards (%d): %s", len(dashboardNames), strings.Join(dashboardNames, "; "))

	datasourceJSON := callMCPToolJSON(t, ctx, session, &serverLog, "list_datasources", map[string]any{"limit": 10})
	var datasources struct {
		Datasources []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			UID  string `json:"uid"`
		} `json:"datasources"`
	}
	if err := json.Unmarshal(datasourceJSON, &datasources); err != nil {
		t.Fatalf("decode list_datasources result: %v\nSSO log:\n%s", err, serverLog.String())
	}
	datasourceNames := make([]string, 0, len(datasources.Datasources))
	for _, datasource := range datasources.Datasources {
		datasourceNames = append(datasourceNames, fmt.Sprintf("%s (%s, %s)", datasource.Name, datasource.Type, datasource.UID))
	}
	t.Logf("Data sources (%d): %s", len(datasourceNames), strings.Join(datasourceNames, "; "))

	reportJSON := callMCPToolJSON(t, ctx, session, &serverLog, "grafana_api_request", map[string]any{"endpoint": "/api/reports", "method": "GET"})
	var reportResponse struct {
		Status int `json:"status"`
		Data   []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(reportJSON, &reportResponse); err != nil {
		t.Fatalf("decode reports API result: %v\nSSO log:\n%s", err, serverLog.String())
	}
	if reportResponse.Status != 200 {
		t.Fatalf("GET /api/reports returned status %d", reportResponse.Status)
	}
	reportNames := make([]string, 0, len(reportResponse.Data))
	for _, report := range reportResponse.Data {
		reportNames = append(reportNames, fmt.Sprintf("%s (%d)", report.Name, report.ID))
	}
	t.Logf("Reports (%d): %s", len(reportNames), strings.Join(reportNames, "; "))
}

func countSSOLoginStarts(logText string) int {
	return strings.Count(logText, "Starting Chromium for Grafana SSO")
}

func callMCPToolJSON(t *testing.T, ctx context.Context, session *mcp.ClientSession, serverLog *bytes.Buffer, name string, arguments map[string]any) []byte {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v\nSSO log:\n%s", name, err, serverLog.String())
	}
	if result.IsError {
		var messages []string
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				messages = append(messages, text.Text)
			}
		}
		t.Fatalf("tool %s returned an error: %s", name, strings.Join(messages, "\n"))
	}
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return []byte(text.Text)
		}
	}
	if result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatalf("marshal %s structured content: %v", name, err)
		}
		return data
	}
	t.Fatalf("tool %s returned no content", name)
	return nil
}
