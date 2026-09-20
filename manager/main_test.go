package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const fullProviderProfile = "mixed-port: 7890\nproxies:\n  - name: local-direct\n    type: direct\nproxy-providers:\n  primary:\n    type: http\n    url: https://1.1.1.1/nodes.yaml\n    interval: 3600\nproxy-groups:\n  - name: Main\n    type: select\n    proxies:\n      - DIRECT\n    use: []\nrules:\n  - MATCH,DIRECT\ndns:\n  enable: false\n  nameserver:\n    - 1.1.1.1\ntun:\n  enable: false\n  stack: system\n"

const initialPersistentConfig = "mixed-port: 7890\nallow-lan: true\nbind-address: 0.0.0.0\nexternal-controller: 127.0.0.1:9999\nsecret: old-secret\nproxies:\n  - name: stale-local-node\n    type: direct\nproxy-groups:\n  - name: OldGroup\n    type: select\n    proxies:\n      - stale-local-node\nold-custom-setting: keep-in-backup\n"

const initialRuntimeConfig = "mixed-port: 7890\nallow-lan: true\nbind-address: 0.0.0.0\nexternal-controller: 127.0.0.1:9999\nsecret: runtime-secret\nproxies:\n  - name: stale-local-node\n    type: direct\nproxy-groups:\n  - name: OldGroup\n    type: select\n    proxies:\n      - stale-local-node\nold-custom-setting: runtime-only\n"

const initialShellCrashConfig = "mix_port=7890\ndb_port=9999\nsecret=local-secret\ndisoverride=0\n"

const overlayConfig = "dns:\n  enable: true\n  nameserver:\n    - 9.9.9.9\ntun:\n  enable: true\n  stack: mixed\n"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestInspectProviderProfileAndGroupReferences(t *testing.T) {
	info, err := inspectSubscriptionProfile([]byte(fullProviderProfile))
	if err != nil {
		t.Fatal(err)
	}
	if info.proxyCount != 1 || info.providerCount != 1 || len(info.providers) != 1 {
		t.Fatalf("unexpected profile counts: %+v", info)
	}
	_, root, err := decodeConfig([]byte(fullProviderProfile))
	if err != nil {
		t.Fatal(err)
	}
	if got := unreferencedProxyProviderCount(root); got != 1 {
		t.Fatalf("unreferenced provider count = %d, want 1", got)
	}
	groups := mappingValue(root, "proxy-groups")
	group := groups.Content[0]
	use := mappingValue(group, "use")
	if use == nil || use.Kind != yaml.SequenceNode || len(use.Content) != 0 {
		t.Fatal("test profile should retain its empty provider use list")
	}
}

func TestInspectRejectsPrivateProviderURL(t *testing.T) {
	profile := strings.Replace(fullProviderProfile, "https://1.1.1.1/nodes.yaml", "http://127.0.0.1/private.yaml", 1)
	if _, err := inspectSubscriptionProfile([]byte(profile)); err == nil {
		t.Fatal("expected private provider URL to be rejected")
	}
}

func TestSafeCoreErrorRedactsCredentialsAndURLs(t *testing.T) {
	secret := strings.Repeat("a", 64)
	body := []byte(`{"message":"invalid source https://sub.example/profile?token=private and ` + secret + `"}`)
	got := safeCoreError(body, secret)
	if strings.Contains(got, "https://") || strings.Contains(got, secret) || strings.Contains(got, "private") {
		t.Fatalf("core error was not redacted: %s", got)
	}
	if !strings.Contains(got, "invalid source") || !strings.Contains(got, "[URL REDACTED]") {
		t.Fatalf("safe diagnostic detail was lost: %s", got)
	}
}

func TestSetShellConfigValuePreservesSettingsAndLineEndings(t *testing.T) {
	input := []byte("mix_port=7890\r\ndisoverride=0\r\nsecret=local-secret\r\n")
	got, err := setShellConfigValue(input, "disoverride", "1")
	if err != nil {
		t.Fatal(err)
	}
	want := "mix_port=7890\r\ndisoverride=1\r\nsecret=local-secret\r\n"
	if string(got) != want {
		t.Fatalf("ShellCrash config changed unexpectedly:\n%s", got)
	}
}

func TestMergeUsesFullProfileAndOnlyOverlaysDNSAndTUN(t *testing.T) {
	profile, runtime, err := mergeSubscriptionConfig(
		[]byte(initialPersistentConfig),
		[]byte(initialRuntimeConfig),
		[]byte(fullProviderProfile),
		[]byte(overlayConfig),
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"persistent": profile, "runtime": runtime} {
		_, root, err := decodeConfig(data)
		if err != nil {
			t.Fatalf("%s config is invalid: %v", name, err)
		}
		if mappingValue(root, "proxy-providers") == nil {
			t.Fatalf("%s config lost the subscription provider", name)
		}
		if mappingValue(root, "old-custom-setting") != nil {
			t.Fatalf("%s config retained stale settings from the previous profile", name)
		}
		if scalarValue(mappingValue(root, "secret")) != "runtime-secret" {
			t.Fatalf("%s config did not preserve the local API secret", name)
		}
		if scalarValue(mappingValue(root, "external-controller")) != "127.0.0.1:9999" {
			t.Fatalf("%s config did not preserve the local API endpoint", name)
		}
		if scalarValue(mappingValue(root, "allow-lan")) != "true" {
			t.Fatalf("%s config did not preserve the app network setting", name)
		}
		if proxies := mappingValue(root, "proxies"); proxies == nil || len(proxies.Content) != 1 || scalarValue(mappingValue(proxies.Content[0], "name")) != "local-direct" {
			t.Fatalf("%s config did not use the subscription proxy list", name)
		}
		group := mappingValue(root, "proxy-groups").Content[0]
		use := mappingValue(group, "use")
		if use == nil || len(use.Content) != 0 {
			t.Fatalf("%s config modified the provider group membership", name)
		}
		if scalarValue(mappingValue(mappingValue(root, "dns"), "enable")) != "true" {
			t.Fatalf("%s config did not apply DNS overlay", name)
		}
		if scalarValue(mappingValue(mappingValue(root, "tun"), "enable")) != "true" {
			t.Fatalf("%s config did not apply TUN overlay", name)
		}
		if scalarValue(mappingValue(mappingValue(root, "tun"), "stack")) != "system" {
			t.Fatalf("%s config did not select the supported TUN stack", name)
		}
	}
}

func TestDecodeOverlayUsesSupportedTunStack(t *testing.T) {
	for _, stack := range []string{"", "mixed", "gvisor"} {
		input := "tun:\n  enable: true\n"
		if stack != "" {
			input += "  stack: " + stack + "\n"
		}
		_, root, err := decodeOverlay([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if got := scalarValue(mappingValue(mappingValue(root, "tun"), "stack")); got != "system" {
			t.Fatalf("stack %q normalized to %q, want system", stack, got)
		}
	}
	_, root, err := decodeOverlay([]byte("tun:\n  enable: true\n  stack: system\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := scalarValue(mappingValue(mappingValue(root, "tun"), "stack")); got != "system" {
		t.Fatalf("explicit system stack changed to %q", got)
	}
	_, root, err = decodeOverlay([]byte("tun:\n  enable: false\n  stack: mixed\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := scalarValue(mappingValue(mappingValue(root, "tun"), "stack")); got != "mixed" {
		t.Fatalf("disabled TUN stack changed to %q", got)
	}
}

func TestDecodeOverlayBuildsHostTUNRoutesAndKeepsLocalNetworksExcluded(t *testing.T) {
	input := []byte("dns:\n  enable: true\ntun:\n  enable: true\n  stack: gvisor\n  route-exclude-address:\n    - 203.0.113.0/24\n")
	_, root, err := decodeOverlay(input)
	if err != nil {
		t.Fatal(err)
	}
	tun := mappingValue(root, "tun")
	for _, key := range []string{"auto-route", "auto-redirect", "auto-detect-interface"} {
		if scalarValue(mappingValue(tun, key)) != "true" {
			t.Fatalf("host TUN %s = %q, want true", key, scalarValue(mappingValue(tun, key)))
		}
	}
	if scalarValue(mappingValue(tun, "stack")) != "system" {
		t.Fatalf("host TUN stack = %q, want system", scalarValue(mappingValue(tun, "stack")))
	}
	for _, required := range []string{"203.0.113.0/24", "192.168.0.0/16", "172.16.0.0/12", "fc00::/7"} {
		if !sequenceHasString(mappingValue(tun, "route-exclude-address"), required) {
			t.Fatalf("host TUN route exclusions lost %s", required)
		}
	}
	for _, unsupported := range []string{"240.0.0.0/4", "ff00::/8"} {
		if sequenceHasString(mappingValue(tun, "route-exclude-address"), unsupported) {
			t.Fatalf("host TUN includes nftables max-range CIDR %s", unsupported)
		}
	}
	for _, required := range []string{"any:53", "tcp://any:53"} {
		if !sequenceHasString(mappingValue(tun, "dns-hijack"), required) {
			t.Fatalf("host DNS hijack is missing %s", required)
		}
	}
}

func TestPrepareNativeRuntimeChangesOnlyManagedFields(t *testing.T) {
	base := t.TempDir()
	profilePath := filepath.Join(base, "ShellCrash", "yamls", "config.yaml")
	runtimePath := filepath.Join(base, "ShellCrash", "runtime", "config.yaml")
	settingsPath := filepath.Join(base, "ShellCrash", "configs", "subscription-manager.json")
	overlayPath := filepath.Join(base, "ShellCrash", "configs", "fnos-overrides.yaml")
	secretPath := filepath.Join(base, "api.secret")
	profile := []byte("mixed-port: 17890\nallow-lan: true\nbind-address: 0.0.0.0\nexternal-controller: :9999\nsecret: old-secret\nproxy-providers:\n  primary:\n    type: http\n    url: https://1.1.1.1/nodes.yaml\nproxy-groups:\n  - name: Main\n    type: select\n    use: [primary]\nrules:\n  - MATCH,Main\n")
	for path, contents := range map[string][]byte{
		profilePath:  profile,
		overlayPath:  []byte(overlayConfig),
		secretPath:   []byte(strings.Repeat("b", 64) + "\n"),
		settingsPath: []byte("{}\n"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app := &manager{
		configPath:        profilePath,
		runtimeConfigPath: runtimePath,
		settingsPath:      settingsPath,
		overlayPath:       overlayPath,
		secretFile:        secretPath,
		mixedPort:         7890,
	}
	if err := app.prepareNativeRuntime(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, profilePath), profile) {
		t.Fatal("native runtime preparation changed the user's persistent profile")
	}
	runtime := mustRead(t, runtimePath)
	_, root, err := decodeConfig(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if scalarValue(mappingValue(root, "mixed-port")) != "7890" {
		t.Fatal("runtime did not apply the default 7890 proxy port")
	}
	if scalarValue(mappingValue(root, "external-controller")) != "127.0.0.1:9999" {
		t.Fatal("runtime controller is not loopback-only")
	}
	if scalarValue(mappingValue(root, "secret")) != strings.Repeat("b", 64) {
		t.Fatal("runtime did not receive the private API secret")
	}
	providers := mappingValue(root, "proxy-providers")
	groups := mappingValue(root, "proxy-groups")
	if providers == nil || groups == nil || scalarValue(mappingValue(groups.Content[0], "name")) != "Main" {
		t.Fatal("runtime preparation lost the provider or policy group")
	}
	if fileMode(t, runtimePath) != 0o600 {
		t.Fatal("runtime configuration is not private")
	}
}

func TestGatewayRequiresAdminAndRoutesCoreWithSecret(t *testing.T) {
	secret := strings.Repeat("c", 64)
	secretPath := filepath.Join(t.TempDir(), "api.secret")
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dashboardDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dashboardDir, "index.html"), []byte("<html><head></head><body>dashboard</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dashboardDir, "_nuxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dashboardDir, "_nuxt", "entry.js"), []byte("dashboard asset"), 0o644); err != nil {
		t.Fatal(err)
	}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			http.Error(w, "missing injected secret", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/connections" {
			if r.URL.Query().Get("token") != secret {
				http.Error(w, "missing injected websocket token", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/version" {
			http.Error(w, "unexpected upstream path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":"test"}`)
	}))
	defer core.Close()
	managerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.Error(w, "unexpected manager path", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer managerServer.Close()
	managerURL, _ := url.Parse(managerServer.URL)
	app := &manager{
		apiURL:         core.URL,
		secretFile:     secretPath,
		appPrefix:      "/app/shellcrash-fnos",
		dashboardDir:   dashboardDir,
		controllerAddr: coreControllerAddress,
		listenAddr:     managerURL.Host,
		coreHTTP:       core.Client(),
		coreBinary:     "/native/mihomo",
		coreDataDir:    t.TempDir(),
		coreLogPath:    filepath.Join(t.TempDir(), "mihomo.log"),
		gatewaySocket:  filepath.Join(t.TempDir(), "app.sock"),
	}
	handler := app.gatewayHandler()
	request := httptest.NewRequest(http.MethodGet, "/app/shellcrash-fnos/ui/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous gateway status = %d, want 401", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/app/shellcrash-fnos/ui/", nil)
	request.Header.Set("X-Trim-Userid", "regular-user")
	request.Header.Set("X-Trim-Isadmin", "false")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-admin gateway status = %d, want 403", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/app/shellcrash-fnos/ui/", nil)
	request.Header.Set("X-Trim-Userid", "admin")
	request.Header.Set("X-Trim-Isadmin", "true")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("admin core proxy status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "shellcrash-fnos-back") || !strings.Contains(response.Body.String(), "base+'/manager/'") {
		t.Fatalf("advanced dashboard did not receive the in-window return link: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "config.defaultBackendURL=window.location.origin+base") {
		t.Fatal("advanced dashboard did not receive the local fnOS-gateway default URL")
	}
	if strings.Contains(response.Body.String(), "q.set('secret'") {
		t.Fatal("advanced dashboard must not rely on query-based secret auto-login")
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatal("advanced dashboard HTML exposed the real Mihomo API secret")
	}
	request = httptest.NewRequest(http.MethodGet, "/app/shellcrash-fnos/ui/_nuxt/entry.js", nil)
	request.Header.Set("X-Trim-Userid", "admin")
	request.Header.Set("X-Trim-Isadmin", "true")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "dashboard asset" {
		t.Fatalf("static dashboard asset status = %d, body = %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/app/shellcrash-fnos/version", nil)
	request.Header.Set("X-Trim-Userid", "admin")
	request.Header.Set("X-Trim-Isadmin", "true")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"version":"test"`) {
		t.Fatalf("admin Core API proxy status = %d, body = %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/app/shellcrash-fnos/connections?token=fnos-gateway", nil)
	request.Header.Set("X-Trim-Userid", "admin")
	request.Header.Set("X-Trim-Isadmin", "true")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("dashboard WebSocket token proxy status = %d, want 204: %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/app/shellcrash-fnos/manager/api/status", nil)
	request.Header.Set("X-Trim-Userid", "admin")
	request.Header.Set("X-Trim-Isadmin", "true")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("manager proxy status = %d, want 204", response.Code)
	}
}

func TestMergeFullProfileRecoversFromInvalidRuntimeFile(t *testing.T) {
	profile, runtime, err := mergeSubscriptionConfig(
		[]byte(initialPersistentConfig),
		[]byte("proxy-groups:\n  - name: broken\n    proxies: [*domain]\n"),
		[]byte(fullProviderProfile),
		[]byte(overlayConfig),
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"persistent": profile, "runtime": runtime} {
		_, root, err := decodeConfig(data)
		if err != nil {
			t.Fatalf("%s config is invalid: %v", name, err)
		}
		if mappingValue(root, "proxy-providers") == nil {
			t.Fatalf("%s config lost the full subscription", name)
		}
		if scalarValue(mappingValue(root, "secret")) != "old-secret" {
			t.Fatalf("%s config did not preserve local settings from the persistent profile", name)
		}
	}
}

func TestSavePreservesProviderProfileWithoutAddingItToGroup(t *testing.T) {
	app, corePayload := newTestManager(t, false, 1)
	response := postSubscription(t, app, "https://subscription.example/full.yaml", overlayConfig)
	if response.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", response.Code, response.Body.String())
	}
	var status statusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ProviderCount != 1 || status.UnreferencedProviderCount != 1 || status.ProxyCount != 2 {
		t.Fatalf("unexpected status after import: %+v", status)
	}
	if strings.Contains(response.Body.String(), "full.yaml") {
		t.Fatal("subscription path was returned to the web UI")
	}

	persisted, err := os.ReadFile(app.configPath)
	if err != nil {
		t.Fatal(err)
	}
	_, root, err := decodeConfig(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if mappingValue(root, "proxy-providers") == nil {
		t.Fatal("full provider configuration was not persisted")
	}
	if scalarValue(mappingValue(root, "secret")) != "runtime-secret" {
		t.Fatal("local Mihomo API secret was not preserved")
	}
	if mappingValue(root, "old-custom-setting") != nil {
		t.Fatal("stale prior profile setting remains in the active full profile")
	}
	group := mappingValue(root, "proxy-groups").Content[0]
	use := mappingValue(group, "use")
	if use == nil || len(use.Content) != 0 {
		t.Fatal("manager inserted a provider into the subscription's group")
	}
	if scalarValue(mappingValue(mappingValue(root, "tun"), "stack")) != "system" {
		t.Fatal("unsupported mixed TUN stack was not normalized")
	}
	applied := <-corePayload
	_, appliedRoot, err := decodeConfig(applied)
	if err != nil {
		t.Fatal(err)
	}
	if mappingValue(appliedRoot, "proxy-providers") == nil {
		t.Fatal("Mihomo API did not receive the full provider profile")
	}
	if mode := fileMode(t, app.profileBackupPath); mode != 0o600 {
		t.Fatalf("profile backup mode = %o, want 600", mode)
	}
	if mode := fileMode(t, app.runtimeBackupPath); mode != 0o600 {
		t.Fatalf("runtime backup mode = %o, want 600", mode)
	}
	shellConfig, err := os.ReadFile(app.shellConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shellConfig), "disoverride=1\n") || !strings.Contains(string(shellConfig), "secret=local-secret\n") {
		t.Fatalf("ShellCrash settings were not preserved with override enabled: %s", shellConfig)
	}
	shellBackup, err := os.ReadFile(app.shellBackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(shellBackup) != initialShellCrashConfig {
		t.Fatal("ShellCrash settings backup does not match the pre-subscription file")
	}
	if mode := fileMode(t, app.shellBackupPath); mode != 0o600 {
		t.Fatalf("ShellCrash settings backup mode = %o, want 600", mode)
	}
}

func TestFeatureModesApplyIndependentlyAndPreserveOtherFields(t *testing.T) {
	app, _ := newTestManager(t, false, 1)
	modes := &featureModes{dns: "off", tun: "on"}
	response := postSubscriptionWithModes(t, app, "https://subscription.example/full.yaml", overlayConfig, modes)
	if response.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", response.Code, response.Body.String())
	}

	var status statusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.DNSMode != "off" || status.TUNMode != "on" {
		t.Fatalf("saved feature modes = DNS %q / TUN %q, want off / on", status.DNSMode, status.TUNMode)
	}

	persisted, err := os.ReadFile(app.configPath)
	if err != nil {
		t.Fatal(err)
	}
	_, root, err := decodeConfig(persisted)
	if err != nil {
		t.Fatal(err)
	}
	dns := mappingValue(root, "dns")
	if scalarValue(mappingValue(dns, "enable")) != "false" {
		t.Fatal("DNS off mode did not override dns.enable")
	}
	nameservers := mappingValue(dns, "nameserver")
	if nameservers == nil || len(nameservers.Content) != 1 || scalarValue(nameservers.Content[0]) != "9.9.9.9" {
		t.Fatal("DNS toggle did not preserve the custom nameserver")
	}
	tun := mappingValue(root, "tun")
	if scalarValue(mappingValue(tun, "enable")) != "true" || scalarValue(mappingValue(tun, "stack")) != "system" {
		t.Fatal("TUN on mode did not enable TUN with the supported stack")
	}
	group := mappingValue(root, "proxy-groups").Content[0]
	use := mappingValue(group, "use")
	if use == nil || len(use.Content) != 0 {
		t.Fatal("feature toggles modified the subscription's provider group")
	}
}

func TestInheritModesUseSubscriptionDNSAndTUN(t *testing.T) {
	app, _ := newTestManager(t, false, 1)
	modes := &featureModes{dns: "inherit", tun: "inherit"}
	response := postSubscriptionWithModes(t, app, "https://subscription.example/full.yaml", overlayConfig, modes)
	if response.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", response.Code, response.Body.String())
	}
	var status statusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.DNSMode != "inherit" || status.TUNMode != "inherit" {
		t.Fatalf("saved feature modes = DNS %q / TUN %q, want inherit / inherit", status.DNSMode, status.TUNMode)
	}
	persisted, err := os.ReadFile(app.configPath)
	if err != nil {
		t.Fatal(err)
	}
	_, root, err := decodeConfig(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if scalarValue(mappingValue(mappingValue(root, "dns"), "enable")) != "false" ||
		scalarValue(mappingValue(mappingValue(root, "tun"), "enable")) != "false" {
		t.Fatal("inherit mode did not preserve the subscription's DNS/TUN settings")
	}
}

func TestSaveRollbackWhenMihomoReloadFails(t *testing.T) {
	app, _ := newTestManager(t, true, 1)
	beforeProfile, _ := os.ReadFile(app.configPath)
	beforeRuntime, _ := os.ReadFile(app.runtimeConfigPath)
	response := postSubscription(t, app, "https://subscription.example/full.yaml", overlayConfig)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("save status = %d, want 502: %s", response.Code, response.Body.String())
	}
	afterProfile, _ := os.ReadFile(app.configPath)
	afterRuntime, _ := os.ReadFile(app.runtimeConfigPath)
	if !bytes.Equal(afterProfile, beforeProfile) || !bytes.Equal(afterRuntime, beforeRuntime) {
		t.Fatal("failed reload changed the existing configs")
	}
	for _, path := range []string{app.settingsPath, app.overlayPath, app.profileBackupPath, app.runtimeBackupPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("failed reload left managed file %s behind", filepath.Base(path))
		}
	}
	if _, err := os.Stat(app.shellBackupPath); !os.IsNotExist(err) {
		t.Fatal("failed reload left a ShellCrash settings backup behind")
	}
	shellConfig, _ := os.ReadFile(app.shellConfigPath)
	if string(shellConfig) != initialShellCrashConfig {
		t.Fatal("failed reload changed ShellCrash settings")
	}
}

func TestSaveStagesFullProfileUntilMihomoStarts(t *testing.T) {
	app, _ := newTestManager(t, false, 1)
	invalidRuntime := []byte("proxy-groups:\n  - name: broken\n    proxies: [*domain]\n")
	if err := os.WriteFile(app.runtimeConfigPath, invalidRuntime, 0o600); err != nil {
		t.Fatal(err)
	}
	app.apiURL = "http://127.0.0.1:1"
	response := postSubscription(t, app, "https://subscription.example/full.yaml", overlayConfig)
	if response.Code != http.StatusOK {
		t.Fatalf("staged save status = %d, body = %s", response.Code, response.Body.String())
	}
	var status statusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.LastError, "尚未就绪") {
		t.Fatalf("status did not explain the staged Core startup: %+v", status)
	}
	profile, err := os.ReadFile(app.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, root, err := decodeConfig(profile); err != nil || mappingValue(root, "proxy-providers") == nil {
		t.Fatalf("provider profile was not persisted: %v", err)
	}
	runtime, err := os.ReadFile(app.runtimeConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeConfig(runtime); err != nil {
		t.Fatalf("invalid runtime config was not replaced by the full profile: %v", err)
	}
	backup, err := os.ReadFile(app.runtimeBackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != initialPersistentConfig {
		t.Fatal("invalid runtime contents were incorrectly saved as the restore point")
	}
	shellConfig, err := os.ReadFile(app.shellConfigPath)
	if err != nil || !strings.Contains(string(shellConfig), "disoverride=1\n") {
		t.Fatalf("ShellCrash override mode was not staged: %v", err)
	}
}

func TestSaveRollsBackWhenProviderHasNoNodes(t *testing.T) {
	app, _ := newTestManager(t, false, 0)
	beforeProfile, _ := os.ReadFile(app.configPath)
	beforeRuntime, _ := os.ReadFile(app.runtimeConfigPath)
	response := postSubscription(t, app, "https://subscription.example/full.yaml", overlayConfig)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("save status = %d, want 502: %s", response.Code, response.Body.String())
	}
	afterProfile, _ := os.ReadFile(app.configPath)
	afterRuntime, _ := os.ReadFile(app.runtimeConfigPath)
	if !bytes.Equal(afterProfile, beforeProfile) || !bytes.Equal(afterRuntime, beforeRuntime) {
		t.Fatal("empty provider changed the existing configs")
	}
	if _, err := os.Stat(app.settingsPath); !os.IsNotExist(err) {
		t.Fatal("empty provider left subscription settings behind")
	}
	if _, err := os.Stat(app.shellBackupPath); !os.IsNotExist(err) {
		t.Fatal("empty provider left a ShellCrash settings backup behind")
	}
}

func TestDeleteRestoresBackedUpConfig(t *testing.T) {
	app, _ := newTestManager(t, false, 1)
	beforeProfile, _ := os.ReadFile(app.configPath)
	beforeRuntime, _ := os.ReadFile(app.runtimeConfigPath)
	beforeShell, _ := os.ReadFile(app.shellConfigPath)
	save := postSubscription(t, app, "https://subscription.example/full.yaml", overlayConfig)
	if save.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", save.Code, save.Body.String())
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/api/subscription", nil)
	app.handleSubscription(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", response.Code, response.Body.String())
	}
	afterProfile, _ := os.ReadFile(app.configPath)
	afterRuntime, _ := os.ReadFile(app.runtimeConfigPath)
	if !bytes.Equal(afterProfile, beforeProfile) || !bytes.Equal(afterRuntime, beforeRuntime) {
		t.Fatal("delete did not restore the pre-subscription config")
	}
	afterShell, err := os.ReadFile(app.shellConfigPath)
	if err != nil || !bytes.Equal(afterShell, beforeShell) {
		t.Fatal("delete did not restore the pre-subscription ShellCrash settings")
	}
	for _, path := range []string{app.settingsPath, app.overlayPath, app.profileBackupPath, app.runtimeBackupPath, app.shellBackupPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("delete left managed file %s behind", filepath.Base(path))
		}
	}
}

func postSubscription(t *testing.T, app *manager, subscriptionURL, overlay string) *httptest.ResponseRecorder {
	return postSubscriptionWithModes(t, app, subscriptionURL, overlay, nil)
}

func postSubscriptionWithModes(t *testing.T, app *manager, subscriptionURL, overlay string, modes *featureModes) *httptest.ResponseRecorder {
	t.Helper()
	input := subscriptionRequest{URL: subscriptionURL, IntervalHours: 24, OverlayYAML: &overlay}
	if modes != nil {
		input.DNSMode = modes.dns
		input.TUNMode = modes.tun
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/subscription", bytes.NewReader(body))
	app.handleSubscription(response, request)
	return response
}

func newTestManager(t *testing.T, reloadFails bool, providerNodeCount int) (*manager, <-chan []byte) {
	t.Helper()
	base := t.TempDir()
	app := &manager{
		configPath:        filepath.Join(base, "yamls", "config.yaml"),
		runtimeConfigPath: filepath.Join(base, "runtime", "config.yaml"),
		settingsPath:      filepath.Join(base, "configs", "subscription-manager.json"),
		overlayPath:       filepath.Join(base, "configs", "fnos-overrides.yaml"),
		profileBackupPath: filepath.Join(base, "configs", "profile.backup.yaml"),
		runtimeBackupPath: filepath.Join(base, "configs", "runtime.backup.yaml"),
		shellConfigPath:   filepath.Join(base, "configs", "ShellCrash.cfg"),
		shellBackupPath:   filepath.Join(base, "configs", "shellcrash-settings.backup"),
		secretFile:        filepath.Join(base, "api.secret"),
		version:           "test",
	}
	for path, contents := range map[string]string{
		app.configPath:        initialPersistentConfig,
		app.runtimeConfigPath: initialRuntimeConfig,
		app.shellConfigPath:   initialShellCrashConfig,
		app.secretFile:        strings.Repeat("a", 64),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	corePayload := make(chan []byte, 4)
	coreServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/configs":
			if r.Method != http.MethodPut {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			if reloadFails {
				http.Error(w, "reload failed", http.StatusBadRequest)
				return
			}
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "bad payload", http.StatusBadRequest)
				return
			}
			select {
			case corePayload <- []byte(request["payload"]):
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		case "/providers/proxies/primary":
			if r.Method == http.MethodPut {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.Method == http.MethodGet {
				proxies := []map[string]string{}
				if providerNodeCount > 0 {
					proxies = append(proxies, map[string]string{"name": "node-1"})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"proxies": proxies})
				return
			}
			http.Error(w, "method", http.StatusMethodNotAllowed)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(coreServer.Close)
	app.apiURL = coreServer.URL
	app.coreHTTP = coreServer.Client()
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/yaml"}},
			Body:       io.NopCloser(strings.NewReader(fullProviderProfile)),
			Request:    r,
		}, nil
	})}
	return app, corePayload
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func sequenceHasString(node *yaml.Node, value string) bool {
	if node == nil || node.Kind != yaml.SequenceNode {
		return false
	}
	for _, item := range node.Content {
		if scalarValue(item) == value {
			return true
		}
	}
	return false
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
