package mcpgrafana

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const (
	grafanaSSOStepWait         = 4 * time.Second
	grafanaAuthModeEnvVar      = "GRAFANA_AUTH_MODE"
	grafanaSSOLoginPathEnvVar  = "GRAFANA_LOGIN_PATH"
	grafanaSSOHostsEnvVar      = "GRAFANA_SSO_HOSTS"
	grafanaSSOTimeoutEnvVar    = "GRAFANA_SSO_TIMEOUT"
	grafanaSSOChromeBinEnvVar  = "GRAFANA_SSO_CHROME_BIN"
	grafanaSSODefaultLoginPath = "/login/azuread"
)

type GrafanaSSOConfig struct {
	GrafanaURL   string
	LoginPath    string
	Username     string
	Password     string
	TrustedHosts []string
	ChromeBinary string
	LoginTimeout time.Duration
}

type GrafanaSSOLoginFunc func(context.Context, GrafanaSSOConfig, *slog.Logger) ([]*http.Cookie, error)

// GrafanaSSOConfigFromEnv selects SSO automatically when service-account
// username and password are configured. Set GRAFANA_AUTH_MODE=token to retain
// token/basic auth instead, or sso to require a browser session explicitly.
func GrafanaSSOConfigFromEnv() (GrafanaSSOConfig, bool, error) {
	mode := strings.ToLower(strings.TrimSpace(getEnv(grafanaAuthModeEnvVar)))
	username := getEnv("GRAFANA_SERVICE_ACCOUNT_USERNAME")
	password := getEnv("GRAFANA_SERVICE_ACCOUNT_PASSWORD")
	if mode == "" {
		if username != "" || password != "" {
			mode = "sso"
		} else {
			mode = "token"
		}
	}
	switch mode {
	case "token":
		return GrafanaSSOConfig{}, false, nil
	case "sso":
	default:
		return GrafanaSSOConfig{}, false, fmt.Errorf("GRAFANA_AUTH_MODE must be sso or token, got %q", mode)
	}

	cfg := GrafanaSSOConfig{
		GrafanaURL:   normalizeGrafanaURL(getEnv(grafanaURLEnvVar)),
		LoginPath:    getEnv(grafanaSSOLoginPathEnvVar),
		Username:     username,
		Password:     password,
		ChromeBinary: firstNonEmpty(getEnv(grafanaSSOChromeBinEnvVar), getEnv("CHROME_BIN")),
		LoginTimeout: 120 * time.Second,
	}
	if cfg.GrafanaURL == "" {
		cfg.GrafanaURL = defaultGrafanaURL
	}
	if cfg.LoginPath == "" {
		cfg.LoginPath = grafanaSSODefaultLoginPath
	}
	if raw := strings.TrimSpace(getEnv(grafanaSSOTimeoutEnvVar)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return GrafanaSSOConfig{}, false, fmt.Errorf("%s must be a positive Go duration", grafanaSSOTimeoutEnvVar)
		}
		cfg.LoginTimeout = d
	}
	cfg.TrustedHosts = splitGrafanaSSOHosts(getEnv(grafanaSSOHostsEnvVar))
	if len(cfg.TrustedHosts) == 0 {
		cfg.TrustedHosts = []string{"login.microsoftonline.com"}
	}
	if err := cfg.validate(); err != nil {
		return GrafanaSSOConfig{}, false, err
	}
	return cfg, true, nil
}

func (cfg GrafanaSSOConfig) validate() error {
	grafanaURL, err := url.Parse(cfg.GrafanaURL)
	if err != nil || grafanaURL.Hostname() == "" || grafanaURL.User != nil {
		return errors.New("GRAFANA_URL must be an absolute URL without credentials")
	}
	if grafanaURL.Scheme != "https" && !(grafanaURL.Scheme == "http" && isLoopbackHost(grafanaURL.Hostname())) {
		return errors.New("GRAFANA_URL must use HTTPS (HTTP is allowed only for loopback hosts)")
	}
	loginPath, err := url.Parse(cfg.LoginPath)
	if err != nil || loginPath.IsAbs() || loginPath.Host != "" || !strings.HasPrefix(loginPath.Path, "/") || loginPath.Fragment != "" {
		return errors.New("GRAFANA_LOGIN_PATH must be a path on the Grafana host")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return errors.New("GRAFANA_SERVICE_ACCOUNT_USERNAME and GRAFANA_SERVICE_ACCOUNT_PASSWORD are required for SSO")
	}
	if cfg.LoginTimeout <= 0 {
		return errors.New("GRAFANA_SSO_TIMEOUT must be positive")
	}
	for _, host := range cfg.TrustedHosts {
		if strings.ContainsAny(host, "/:@") || host == "." {
			return fmt.Errorf("invalid GRAFANA_SSO_HOSTS entry %q", host)
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitGrafanaSSOHosts(raw string) []string {
	var hosts []string
	for _, host := range strings.Split(raw, ",") {
		if host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), "."); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func waitGrafanaSSOStep(ctx context.Context, logger *slog.Logger, stage string) error {
	logger.Info("Waiting for Grafana SSO step to settle", "stage", stage, "delay", grafanaSSOStepWait)
	timer := time.NewTimer(grafanaSSOStepWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func grafanaSSOIsDefault() bool {
	mode := strings.ToLower(strings.TrimSpace(getEnv(grafanaAuthModeEnvVar)))
	if mode == "sso" {
		return true
	}
	if mode == "token" {
		return false
	}
	return getEnv("GRAFANA_SERVICE_ACCOUNT_USERNAME") != "" || getEnv("GRAFANA_SERVICE_ACCOUNT_PASSWORD") != ""
}

func LoginGrafanaSSO(ctx context.Context, cfg GrafanaSSOConfig, logger *slog.Logger, login GrafanaSSOLoginFunc) (http.CookieJar, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("Preparing Grafana SSO", "grafana_url", redactedPageLocation(cfg.GrafanaURL), "login_path", cfg.LoginPath)
	if login == nil {
		login = rodGrafanaSSOLogin
	}
	cookies, err := login(ctx, cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("Grafana SSO login: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create Grafana session cookie jar: %w", err)
	}
	baseURL, _ := url.Parse(cfg.GrafanaURL)
	jar.SetCookies(baseURL, cookies)
	if len(jar.Cookies(baseURL)) == 0 {
		return nil, errors.New("Grafana SSO completed without a session cookie")
	}
	logger.Info("Grafana SSO session cookies captured", "cookie_count", len(jar.Cookies(baseURL)))
	return jar, nil
}

// GrafanaSSOSession lazily creates and refreshes the browser-authenticated
// Grafana session shared by all clients in a server process.
type GrafanaSSOSession struct {
	config GrafanaSSOConfig
	logger *slog.Logger
	login  GrafanaSSOLoginFunc

	mu  sync.Mutex
	jar http.CookieJar

	warnStrippedAuthOnce sync.Once
}

// NewGrafanaSSOSession creates a session manager without starting Chromium.
// The first Grafana API request performs the browser login.
func NewGrafanaSSOSession(config GrafanaSSOConfig, logger *slog.Logger) *GrafanaSSOSession {
	if logger == nil {
		logger = slog.Default()
	}
	return &GrafanaSSOSession{config: config, logger: logger, login: rodGrafanaSSOLogin}
}

func (s *GrafanaSSOSession) transport(underlying http.RoundTripper) http.RoundTripper {
	return &grafanaSSOSessionTransport{session: s, underlying: underlying}
}

func (s *GrafanaSSOSession) cookieJar(ctx context.Context) (http.CookieJar, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jar != nil {
		return s.jar, nil
	}
	jar, err := LoginGrafanaSSO(ctx, s.config, s.logger, s.login)
	if err != nil {
		return nil, err
	}
	s.jar = jar
	return s.jar, nil
}

func (s *GrafanaSSOSession) refresh(ctx context.Context, expired http.CookieJar) (http.CookieJar, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jar != nil && s.jar != expired {
		return s.jar, nil
	}
	s.jar = nil
	jar, err := LoginGrafanaSSO(ctx, s.config, s.logger, s.login)
	if err != nil {
		return nil, err
	}
	s.jar = jar
	return s.jar, nil
}

type grafanaSSOSessionTransport struct {
	session    *GrafanaSSOSession
	underlying http.RoundTripper
}

func (t *grafanaSSOSessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	baseURL, err := url.Parse(t.session.config.GrafanaURL)
	if err != nil || !sameURLOrigin(req.URL, baseURL) {
		return t.underlying.RoundTrip(req)
	}

	jar, err := t.session.cookieJar(req.Context())
	if err != nil {
		return nil, fmt.Errorf("start Grafana SSO session: %w", err)
	}
	response, err := t.roundTripWithJar(req, jar, false)
	if err != nil || response == nil || !grafanaSSOSessionExpired(response, req) {
		return response, err
	}

	refreshedJar, refreshErr := t.session.refresh(req.Context(), jar)
	if refreshErr != nil {
		t.session.logger.Warn("Could not refresh expired Grafana SSO session", "error", refreshErr)
		return response, nil
	}
	if req.Body != nil && req.GetBody == nil {
		return response, nil
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	return t.roundTripWithJar(req, refreshedJar, true)
}

func (t *grafanaSSOSessionTransport) roundTripWithJar(req *http.Request, jar http.CookieJar, retry bool) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	// Grafana prefers an Authorization header over the session cookie, so a
	// token or basic-auth credential (e.g. a client-supplied
	// X-Grafana-Service-Account-Token) would shadow the SSO session.
	if cloned.Header.Get("Authorization") != "" {
		cloned.Header.Del("Authorization")
		t.session.warnStrippedAuthOnce.Do(func() {
			t.session.logger.Warn("Ignoring Authorization credentials on Grafana requests because SSO auth is active; set GRAFANA_AUTH_MODE=token to use token or basic auth")
		})
	}
	if retry && req.Body != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("replay Grafana request after SSO refresh: %w", err)
		}
		cloned.Body = body
	}
	if cloned.Header.Get("Cookie") == "" {
		for _, cookie := range jar.Cookies(req.URL) {
			cloned.AddCookie(cookie)
		}
	}
	response, err := t.underlying.RoundTrip(cloned)
	if err == nil && response != nil {
		jar.SetCookies(req.URL, response.Cookies())
	}
	return response, err
}

func grafanaSSOSessionExpired(response *http.Response, req *http.Request) bool {
	if response.StatusCode == http.StatusUnauthorized {
		return true
	}
	if response.StatusCode != http.StatusFound && response.StatusCode != http.StatusSeeOther &&
		response.StatusCode != http.StatusTemporaryRedirect && response.StatusCode != http.StatusPermanentRedirect {
		return false
	}
	location, err := req.URL.Parse(response.Header.Get("Location"))
	return err == nil && sameURLOrigin(location, req.URL) &&
		(location.Path == "/login" || strings.HasPrefix(location.Path, "/login/"))
}

func rodGrafanaSSOLogin(ctx context.Context, cfg GrafanaSSOConfig, logger *slog.Logger) ([]*http.Cookie, error) {
	baseURL, _ := url.Parse(cfg.GrafanaURL)
	loginURL := strings.TrimRight(cfg.GrafanaURL, "/") + "/" + strings.TrimLeft(cfg.LoginPath, "/")
	loginCtx, cancel := context.WithTimeout(ctx, cfg.LoginTimeout)
	defer cancel()

	logger.Info("Starting Chromium for Grafana SSO")
	browserLauncher := launcher.New().Headless(true)
	if cfg.ChromeBinary != "" {
		browserLauncher = browserLauncher.Bin(cfg.ChromeBinary)
	}
	controlURL, err := browserLauncher.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch Chromium: %w", err)
	}
	defer browserLauncher.Kill()

	browser := rod.New().ControlURL(controlURL).Context(loginCtx)
	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("connect to Chromium: %w", err)
	}
	defer browser.Close()
	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, fmt.Errorf("open browser page: %w", err)
	}
	page = page.Context(loginCtx)
	if err := (proto.NetworkEnable{}).Call(page); err != nil {
		logger.Warn("Could not enable SSO network diagnostics", "error", err)
	} else {
		waitNetworkEvents := page.EachEvent(
			func(event *proto.NetworkRequestWillBeSent) {
				if event.Request == nil || !isGrafanaSSODiagnosticHost(event.Request.URL, cfg) {
					return
				}
				logger.Info("SSO HTTP request", "method", event.Request.Method, "url", redactedPageLocation(event.Request.URL))
				if event.RedirectResponse != nil {
					logger.Info("SSO HTTP redirect", "status", event.RedirectResponse.Status, "url", redactedPageLocation(event.RedirectResponse.URL))
				}
			},
			func(event *proto.NetworkResponseReceived) {
				if event.Response == nil || !isGrafanaSSODiagnosticHost(event.Response.URL, cfg) {
					return
				}
				logger.Info("SSO HTTP response", "status", event.Response.Status, "url", redactedPageLocation(event.Response.URL))
			},
		)
		go waitNetworkEvents()
	}
	if err := page.Navigate(loginURL); err != nil {
		return nil, fmt.Errorf("start Grafana SSO: %w", err)
	}
	logger.Info("Opened Grafana SSO route")
	if err := waitGrafanaSSOStep(loginCtx, logger, "grafana_route"); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(cfg.LoginTimeout)
	returned, err := waitForSSOPage(loginCtx, page, baseURL, "#i0116", deadline, false)
	if err != nil {
		return nil, fmt.Errorf("wait for SSO email prompt: %w", err)
	}
	if !returned {
		if err := requireTrustedSSOHost(page, cfg.TrustedHosts); err != nil {
			return nil, err
		}
		logger.Info("SSO email form detected")
		if err := waitGrafanaSSOStep(loginCtx, logger, "email_form"); err != nil {
			return nil, err
		}
		if err := enterAndSubmit(page, "#i0116", cfg.Username, deadline, logger); err != nil {
			return nil, fmt.Errorf("submit SSO username: %w", err)
		}
		logger.Info("SSO email submitted")
		if err := waitGrafanaSSOStep(loginCtx, logger, "email_submitted"); err != nil {
			return nil, err
		}
		returned, err = waitForSSOPage(loginCtx, page, baseURL, "#i0118", deadline, true)
		if err != nil {
			return nil, fmt.Errorf("wait for SSO password prompt: %w", err)
		}
		if !returned {
			if err := requireTrustedSSOHost(page, cfg.TrustedHosts); err != nil {
				return nil, err
			}
			logger.Info("SSO password form detected")
			if err := waitGrafanaSSOStep(loginCtx, logger, "password_form"); err != nil {
				return nil, err
			}
			if err := enterAndSubmit(page, "#i0118", cfg.Password, deadline, logger); err != nil {
				return nil, fmt.Errorf("submit SSO password: %w", err)
			}
			logger.Info("SSO password submitted; waiting for identity-provider redirect")
			if err := waitGrafanaSSOStep(loginCtx, logger, "password_submitted"); err != nil {
				return nil, err
			}
			if err := waitForGrafanaReturn(loginCtx, page, baseURL, deadline, logger); err != nil {
				return nil, fmt.Errorf("wait for Grafana redirect: %w", err)
			}
		}
	}
	cookies, err := page.Cookies([]string{cfg.GrafanaURL})
	if err != nil {
		return nil, fmt.Errorf("read Grafana browser cookies: %w", err)
	}
	result := make([]*http.Cookie, 0, len(cookies))
	for _, browserCookie := range cookies {
		cookie := &http.Cookie{
			Name:     browserCookie.Name,
			Value:    browserCookie.Value,
			Path:     browserCookie.Path,
			Domain:   browserCookie.Domain,
			Secure:   browserCookie.Secure,
			HttpOnly: browserCookie.HTTPOnly,
		}
		if !browserCookie.Session && browserCookie.Expires > 0 {
			cookie.Expires = time.Unix(int64(browserCookie.Expires), 0)
		}
		result = append(result, cookie)
	}
	return result, nil
}

func isGrafanaSSODiagnosticHost(rawURL string, cfg GrafanaSSOConfig) bool {
	candidate, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if base, err := url.Parse(cfg.GrafanaURL); err == nil && strings.EqualFold(candidate.Hostname(), base.Hostname()) {
		return true
	}
	for _, host := range cfg.TrustedHosts {
		if strings.EqualFold(candidate.Hostname(), host) {
			return true
		}
	}
	return false
}

func waitForSSOPage(ctx context.Context, page *rod.Page, baseURL *url.URL, selector string, deadline time.Time, allowReturn bool) (bool, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := page.Info()
		if err == nil && allowReturn && sameURLOriginString(info.URL, baseURL) {
			return true, nil
		}
		if err == nil {
			element, findErr := visibleSSOElement(page, selector)
			if findErr == nil && element != nil {
				return false, nil
			}
		}
		if time.Until(deadline) <= 0 {
			return false, context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func visibleSSOElement(page *rod.Page, selector string) (*rod.Element, error) {
	elements, err := page.Elements(selector)
	if err != nil {
		return nil, err
	}
	for _, element := range elements {
		visible, err := element.Visible()
		if err == nil && visible {
			return element, nil
		}
	}
	return nil, nil
}

func waitForGrafanaReturn(ctx context.Context, page *rod.Page, baseURL *url.URL, deadline time.Time, logger *slog.Logger) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastPage := "unavailable"
	staySignedInNoClicked := false
	noControlWarningLogged := false
	nextProgressLog := time.Now().Add(5 * time.Second)
	for {
		info, err := page.Info()
		if err == nil {
			lastPage = redactedPageLocation(info.URL)
			if sameURLOriginString(info.URL, baseURL) {
				return nil
			}
		}
		if err == nil && !staySignedInNoClicked {
			clicked, err := clickStaySignedInAndWaitForNavigation(page, deadline, logger)
			if err != nil {
				return err
			}
			staySignedInNoClicked = clicked
			if clicked {
				logger.Info("Dismissed identity-provider stay-signed-in prompt")
				if err := waitGrafanaSSOStep(ctx, logger, "stay_signed_in_no_clicked"); err != nil {
					return err
				}
			} else if !noControlWarningLogged {
				logger.Info("No visible stay-signed-in choice yet")
				logger.Info("SSO prompt selector state", "state", ssoPromptSelectorState(page))
				noControlWarningLogged = true
			}
		}
		if time.Until(deadline) <= 0 {
			logger.Warn("Timed out waiting for Grafana SSO callback", "last_page", lastPage, "auth_stage", ssoAuthStage(page))
			return fmt.Errorf("Grafana redirect did not complete before timeout (last page %s, auth stage %s): %w", lastPage, ssoAuthStage(page), context.DeadlineExceeded)
		}
		select {
		case <-ctx.Done():
			stage := ssoAuthStage(page.Context(context.Background()))
			logger.Warn("Grafana SSO context ended before callback", "last_page", lastPage, "auth_stage", stage)
			return fmt.Errorf("Grafana redirect did not complete (last page %s, auth stage %s): %w", lastPage, stage, ctx.Err())
		case <-ticker.C:
			if time.Now().After(nextProgressLog) {
				logger.Info("Waiting for Grafana SSO callback", "last_page", lastPage, "auth_stage", ssoAuthStage(page))
				nextProgressLog = time.Now().Add(5 * time.Second)
			}
		}
	}
}

func ssoPromptSelectorState(page *rod.Page) string {
	result, err := page.Eval(`() => {
		const visible = (el) => !!el && el.getClientRects().length > 0 && getComputedStyle(el).visibility !== "hidden";
		const text = (document.body?.innerText || "").toLowerCase();
		const back = document.querySelector("#idBtn_Back");
		const noInput = document.querySelector("input[value='No'], input[value^='No']");
		const noButton = Array.from(document.querySelectorAll("button,[role=button]")).some((el) => /^(no|no[, ]|no\\s)/i.test((el.innerText || el.getAttribute("aria-label") || "").trim()) && visible(el));
		const password = document.querySelector("#i0118");
		return [text.includes("stay signed in"), visible(back), visible(noInput), noButton, visible(password)].map((v) => v ? "1" : "0").join(",");
	}`)
	if err != nil || result == nil {
		return "unavailable"
	}
	return result.Value.String()
}

func clickStaySignedInAndWaitForNavigation(page *rod.Page, deadline time.Time, logger *slog.Logger) (bool, error) {
	button := findStaySignedInNoButton(page)
	if button == nil {
		return false, nil
	}
	id, _ := button.Attribute("id")
	value, _ := button.Attribute("value")
	label, _ := button.Text()
	logger.Info("Clicking identity-provider No control", "control_id", id, "control_value", value, "control_text", strings.TrimSpace(label))
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false, context.DeadlineExceeded
	}
	if remaining > 3*time.Second {
		remaining = 3 * time.Second
	}
	waitNavigation := page.Timeout(remaining).WaitNavigation(proto.PageLifecycleEventNameDOMContentLoaded)
	clicked, clickErr := clickStaySignedInNo(button)
	waitNavigation()
	if clickErr != nil {
		return false, clickErr
	}
	return clicked, nil
}

func ssoAuthStage(page *rod.Page) string {
	if findStaySignedInNoButton(page) != nil {
		return "stay_signed_in"
	}
	for _, stage := range []struct {
		name     string
		selector string
	}{
		{name: "mfa", selector: `#idTxtBx_SAOTCC_OTC, input[autocomplete="one-time-code"]`},
		{name: "password", selector: "#i0118"},
		{name: "email", selector: "#i0116"},
	} {
		found, _, err := page.Timeout(500 * time.Millisecond).Has(stage.selector)
		if err == nil && found {
			return stage.name
		}
	}
	return "other_identity_provider_step"
}

func redactedPageLocation(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "unavailable"
	}
	return parsed.Scheme + "://" + parsed.Host + parsed.EscapedPath()
}

func enterAndSubmit(page *rod.Page, selector, value string, deadline time.Time, logger *slog.Logger) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var element *rod.Element
	for element == nil {
		if time.Until(deadline) <= 0 {
			return context.DeadlineExceeded
		}
		var err error
		element, err = visibleSSOElement(page, selector)
		if err != nil {
			return err
		}
		if element != nil {
			break
		}
		select {
		case <-page.GetContext().Done():
			return page.GetContext().Err()
		case <-ticker.C:
		}
	}
	if err := element.Input(value); err != nil {
		return err
	}
	if err := waitGrafanaSSOStep(page.GetContext(), logger, selector+"_filled"); err != nil {
		return err
	}
	if selector == "#i0118" {
		logger.Info("Submitting SSO password form with Enter")
		return page.Keyboard.Type(input.Enter)
	}
	inputType, _ := element.Attribute("type")
	logger.Info("Filled SSO input", "selector", selector, "input_type", inputType)
	return clickSSOSubmit(page, deadline, logger)
}

func clickSSOSubmit(page *rod.Page, deadline time.Time, logger *slog.Logger) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		buttons, err := page.Timeout(remaining).Elements("#idSIButton9")
		if err != nil {
			return err
		}
		for _, button := range buttons {
			visible, err := button.Visible()
			if err != nil || !visible {
				continue
			}
			if _, err := button.Interactable(); err != nil {
				continue
			}
			label, _ := button.Text()
			buttonType, _ := button.Attribute("type")
			logger.Info("Clicking SSO submit control", "control_type", buttonType, "control_label", strings.TrimSpace(label))
			if err := button.Click(proto.InputMouseButtonLeft, 1); err == nil {
				return nil
			} else if strings.Contains(err.Error(), "Node is detached from document") {
				return nil
			} else if !strings.Contains(err.Error(), "pointer-events is none") {
				return err
			}
		}
		select {
		case <-page.GetContext().Done():
			return page.GetContext().Err()
		case <-ticker.C:
		}
	}
}

func requireTrustedSSOHost(page *rod.Page, trustedHosts []string) error {
	info, err := page.Info()
	if err != nil {
		return fmt.Errorf("inspect SSO page: %w", err)
	}
	current, err := url.Parse(info.URL)
	if err != nil || current.Scheme != "https" {
		return errors.New("refusing to enter credentials on a non-HTTPS SSO page")
	}
	host := strings.TrimSuffix(strings.ToLower(current.Hostname()), ".")
	for _, trusted := range trustedHosts {
		if host == trusted {
			return nil
		}
	}
	return fmt.Errorf("refusing to enter credentials on untrusted SSO host %q; configure GRAFANA_SSO_HOSTS to allow it", host)
}

func declineStaySignedIn(page *rod.Page) (bool, error) {
	if button := findStaySignedInNoButton(page); button != nil {
		return clickStaySignedInNo(button)
	}
	return false, nil
}

func findStaySignedInNoButton(page *rod.Page) *rod.Element {
	for _, query := range []string{"Stay signed in", "stay signed in", "signed in"} {
		results, err := page.Sleeper(rod.NotFoundSleeper).Search(query)
		if err != nil || results.First == nil {
			continue
		}
		button := findNoButtonInPage(page, true)
		results.Release()
		if button != nil {
			return button
		}
	}
	if hasVisibleSSOElement(page, "#i0118") {
		return nil
	}
	return findNoButtonInPage(page, false)
}

func findNoButtonInPage(page *rod.Page, staySignedInPromptVisible bool) *rod.Element {
	for _, selector := range []string{
		"#idBtn_Back",
		"input[value='No']",
		"input[value^='No']",
		"button",
		"[role=button]",
	} {
		elements, err := page.Elements(selector)
		if err != nil {
			continue
		}
		for _, element := range elements {
			visible, err := element.Visible()
			if err != nil || !visible {
				continue
			}
			id, _ := element.Attribute("id")
			isMicrosoftNoButton := id != nil && *id == "idBtn_Back"
			if isMicrosoftNoButton && !staySignedInPromptVisible {
				continue
			}
			if !isMicrosoftNoButton {
				if _, err := element.Interactable(); err != nil {
					continue
				}
			}
			if isMicrosoftNoButton || isStaySignedInNoLabel(element) {
				return element
			}
		}
	}
	return nil
}

func hasVisibleSSOElement(page *rod.Page, selector string) bool {
	elements, err := page.Elements(selector)
	if err != nil {
		return false
	}
	for _, element := range elements {
		visible, err := element.Visible()
		if err == nil && visible {
			return true
		}
	}
	return false
}

func isStaySignedInNoLabel(element *rod.Element) bool {
	if id, err := element.Attribute("id"); err == nil && id != nil && *id == "idBtn_Back" {
		return true
	}
	text, _ := element.Text()
	for _, attribute := range []string{"value", "aria-label"} {
		if strings.TrimSpace(text) != "" {
			break
		}
		value, err := element.Attribute(attribute)
		if err == nil && value != nil {
			text = *value
		}
	}
	return isNoStaySignedInAction(text)
}

func isNoStaySignedInAction(label string) bool {
	normalized := strings.ToLower(strings.TrimSpace(label))
	return normalized == "no" || strings.HasPrefix(normalized, "no,") || strings.HasPrefix(normalized, "no ")
}

func clickStaySignedInNo(button *rod.Element) (bool, error) {
	err := button.Click(proto.InputMouseButtonLeft, 1)
	if strings.Contains(fmt.Sprint(err), "Node is detached from document") {
		return true, nil
	}
	if strings.Contains(fmt.Sprint(err), "pointer-events is none") {
		if _, evalErr := button.Eval("() => this.click()"); evalErr == nil {
			return true, nil
		} else {
			return false, evalErr
		}
	}
	return err == nil, err
}

func sameURLOriginString(rawURL string, baseURL *url.URL) bool {
	current, err := url.Parse(rawURL)
	return err == nil && sameURLOrigin(current, baseURL)
}

func sameURLOrigin(current, baseURL *url.URL) bool {
	return strings.EqualFold(current.Scheme, baseURL.Scheme) && strings.EqualFold(current.Host, baseURL.Host)
}

type grafanaSessionCookieTransport struct {
	jar        http.CookieJar
	underlying http.RoundTripper
}

func (t *grafanaSessionCookieTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if cloned.Header.Get("Cookie") == "" {
		for _, cookie := range t.jar.Cookies(req.URL) {
			cloned.AddCookie(cookie)
		}
	}
	response, err := t.underlying.RoundTrip(cloned)
	if err == nil && response != nil {
		t.jar.SetCookies(req.URL, response.Cookies())
	}
	return response, err
}
