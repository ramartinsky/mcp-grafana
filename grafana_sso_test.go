package mcpgrafana

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGrafanaSSOConfigFromEnvDefaultsToSSOWithCredentials(t *testing.T) {
	setSSOTestEnv(t)

	cfg, enabled, err := GrafanaSSOConfigFromEnv()
	if err != nil {
		t.Fatalf("GrafanaSSOConfigFromEnv() error = %v", err)
	}
	if !enabled {
		t.Fatal("Grafana SSO should be the default when service-account credentials are set")
	}
	if cfg.GrafanaURL != "https://grafana.example.test" || cfg.LoginPath != "/login/azuread" {
		t.Errorf("unexpected SSO config: %#v", cfg)
	}
	if cfg.LoginPath != "/login/azuread" {
		t.Errorf("SSO login path = %q, want /login/azuread", cfg.LoginPath)
	}
}

func TestGrafanaSSOConfigAllowsExplicitTokenMode(t *testing.T) {
	setSSOTestEnv(t)
	t.Setenv(grafanaAuthModeEnvVar, "token")

	_, enabled, err := GrafanaSSOConfigFromEnv()
	if err != nil {
		t.Fatalf("GrafanaSSOConfigFromEnv() error = %v", err)
	}
	if enabled {
		t.Fatal("explicit token mode unexpectedly enabled SSO")
	}
	_, token := urlAndAPIKeyFromEnv(slog.Default())
	if token != "legacy-token" {
		t.Fatalf("token mode token = %q, want legacy-token", token)
	}
}

func TestLoginGrafanaSSOUsesInjectedLoginAndCookieJar(t *testing.T) {
	setSSOTestEnv(t)
	cfg, _, err := GrafanaSSOConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	jar, err := LoginGrafanaSSO(context.Background(), cfg, nil, func(_ context.Context, got GrafanaSSOConfig, _ *slog.Logger) ([]*http.Cookie, error) {
		calls++
		if got.GrafanaURL != cfg.GrafanaURL {
			t.Errorf("injected login received URL %q", got.GrafanaURL)
		}
		return []*http.Cookie{{Name: "grafana_session", Value: "initial", Path: "/", HttpOnly: true}}, nil
	})
	if err != nil {
		t.Fatalf("LoginGrafanaSSO() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("login calls = %d, want 1", calls)
	}
	parsedURL, _ := url.Parse(cfg.GrafanaURL)
	cookies := jar.Cookies(parsedURL)
	if len(cookies) != 1 || cookies[0].Name != "grafana_session" || cookies[0].Value != "initial" {
		t.Fatalf("session cookies = %#v", cookies)
	}
}

func TestLoginGrafanaSSOWritesCredentialFreeLog(t *testing.T) {
	setSSOTestEnv(t)
	cfg, _, err := GrafanaSSOConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cfg.LogFile = filepath.Join(t.TempDir(), "sso", "login.log")
	_, err = LoginGrafanaSSO(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func(context.Context, GrafanaSSOConfig, *slog.Logger) ([]*http.Cookie, error) {
		return []*http.Cookie{{Name: "grafana_session", Value: "test-session", Path: "/"}}, nil
	})
	if err != nil {
		t.Fatalf("LoginGrafanaSSO() error = %v", err)
	}
	contents, err := os.ReadFile(cfg.LogFile)
	if err != nil {
		t.Fatalf("read SSO log: %v", err)
	}
	logText := string(contents)
	if !strings.Contains(logText, "Grafana SSO session cookies captured") {
		t.Fatalf("SSO log missing completion event: %s", logText)
	}
	if strings.Contains(logText, cfg.Username) || strings.Contains(logText, cfg.Password) {
		t.Fatal("SSO log contains credentials")
	}
}

func TestBuildTransportRotatesGrafanaSessionCookie(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		cookie, _ := r.Cookie("grafana_session")
		if requests == 1 {
			if cookie == nil || cookie.Value != "initial" {
				t.Errorf("first request cookie = %v, want initial", cookie)
			}
			w.Header().Add("Set-Cookie", "grafana_session=rotated; Path=/; HttpOnly")
			return
		}
		if cookie == nil || cookie.Value != "rotated" {
			t.Errorf("second request cookie = %v, want rotated", cookie)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	jar := newTestCookieJar(t, server.URL)
	transport, err := BuildTransport(&GrafanaConfig{CookieJar: jar}, server.Client().Transport)
	if err != nil {
		t.Fatalf("BuildTransport() error = %v", err)
	}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for i := 0; i < 2; i++ {
		response, err := client.Get(server.URL + "/api/user")
		if err != nil {
			t.Fatalf("request %d error = %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %s", i+1, response.Status)
		}
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestGrafanaSSOSessionLogsInLazilyAndRefreshesExpiredSession(t *testing.T) {
	setSSOTestEnv(t)
	var currentSession atomic.Value
	currentSession.Store("session-1")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		cookie, _ := r.Cookie("grafana_session")
		if cookie == nil || cookie.Value != currentSession.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg, _, err := GrafanaSSOConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cfg.GrafanaURL = server.URL
	var logins atomic.Int32
	session := &GrafanaSSOSession{
		config: cfg,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		login: func(context.Context, GrafanaSSOConfig, *slog.Logger) ([]*http.Cookie, error) {
			loginNumber := logins.Add(1)
			value := "session-" + strconv.Itoa(int(loginNumber))
			currentSession.Store(value)
			return []*http.Cookie{{Name: "grafana_session", Value: value, Path: "/", HttpOnly: true}}, nil
		},
	}
	transport, err := BuildTransport(&GrafanaConfig{SSOSession: session}, server.Client().Transport)
	if err != nil {
		t.Fatalf("BuildTransport() error = %v", err)
	}
	client := &http.Client{Transport: transport, Timeout: time.Second}

	if logins.Load() != 0 {
		t.Fatal("SSO login occurred before the first Grafana request")
	}
	for requestNumber := 1; requestNumber <= 3; requestNumber++ {
		response, err := client.Get(server.URL + "/api/user")
		if err != nil {
			t.Fatalf("request %d error = %v", requestNumber, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %s, want 200", requestNumber, response.Status)
		}
		if requestNumber == 1 && logins.Load() != 1 {
			t.Fatalf("initial request login count = %d, want 1", logins.Load())
		}
		if requestNumber == 1 {
			currentSession.Store("server-side-rotated-session")
		}
		if requestNumber == 2 {
			if logins.Load() != 2 {
				t.Fatalf("expired session login count = %d, want 2", logins.Load())
			}
			if requests.Load() != 3 {
				t.Fatalf("request count after expired-session retry = %d, want 3", requests.Load())
			}
		}
	}
	if got := logins.Load(); got != 2 {
		t.Fatalf("total login count = %d, want 2 (initial login plus one refresh)", got)
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("total request count = %d, want 4 (one request, 401, retry, reuse)", got)
	}
}

func TestSSOAuthSuppressesConfiguredBearerToken(t *testing.T) {
	setSSOTestEnv(t)
	t.Setenv(grafanaAuthModeEnvVar, "sso")

	_, token := urlAndAPIKeyFromEnv(slog.Default())
	if token != "" {
		t.Fatalf("SSO mode returned bearer token %q", token)
	}
	if userAndPassFromEnv() != nil {
		t.Fatal("SSO mode should not also configure environment basic auth")
	}
}

func setSSOTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv(grafanaAuthModeEnvVar, "")
	t.Setenv(grafanaURLEnvVar, "https://grafana.example.test")
	t.Setenv("GRAFANA_SERVICE_ACCOUNT_USERNAME", "svc@example.test")
	t.Setenv("GRAFANA_SERVICE_ACCOUNT_PASSWORD", "test-password")
	t.Setenv(grafanaServiceAccountTokenEnvVar, "legacy-token")
	t.Setenv(grafanaAPIEnvVar, "")
	t.Setenv(grafanaSSOLoginPathEnvVar, "")
	t.Setenv(grafanaSSOHostsEnvVar, "")
	t.Setenv(grafanaSSOTimeoutEnvVar, "")
	t.Setenv(grafanaSSOChromeBinEnvVar, "")
	t.Setenv("CHROME_BIN", "")
	t.Setenv(grafanaUsernameEnvVar, "")
	t.Setenv(grafanaPasswordEnvVar, "")
}

func newTestCookieJar(t *testing.T, rawURL string) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(parsedURL, []*http.Cookie{{Name: "grafana_session", Value: "initial", Path: "/"}})
	return jar
}

func TestSSOHostParsingIsExact(t *testing.T) {
	hosts := splitGrafanaSSOHosts(" login.microsoftonline.com., tenant.example.test ")
	if strings.Join(hosts, ",") != "login.microsoftonline.com,tenant.example.test" {
		t.Fatalf("hosts = %v", hosts)
	}
}

func TestIsNoStaySignedInAction(t *testing.T) {
	for _, test := range []struct {
		label string
		want  bool
	}{
		{label: "No", want: true},
		{label: "No, thanks", want: true},
		{label: "No thanks", want: true},
		{label: "Nobody", want: false},
		{label: "Yes", want: false},
	} {
		t.Run(test.label, func(t *testing.T) {
			if got := isNoStaySignedInAction(test.label); got != test.want {
				t.Errorf("isNoStaySignedInAction(%q) = %t, want %t", test.label, got, test.want)
			}
		})
	}
}
