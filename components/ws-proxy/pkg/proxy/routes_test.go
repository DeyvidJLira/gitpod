// Copyright (c) 2020 TypeFox GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package proxy

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitpod-io/gitpod/common-go/log"
	"github.com/gitpod-io/gitpod/common-go/util"
	"github.com/gitpod-io/gitpod/ws-manager/api"
	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
)

const (
	hostBasedHeader = "x-host-header"
	wsHostSuffix    = ".gitpod.io"
)

var (
	workspaces = []WorkspaceInfo{
		{
			IDEImage: "gitpod-io/ide:latest",
			Auth: &api.WorkspaceAuthentication{
				Admission:  api.AdmissionLevel_ADMIT_OWNER_ONLY,
				OwnerToken: "owner-token",
			},
			IDEPublicPort: "23000",
			InstanceID:    "1943c611-a014-4f4d-bf5d-14ccf0123c60",
			Ports: []PortInfo{
				{PortSpec: api.PortSpec{Port: 28080, Target: 38080, Url: "https://28080-c95fd41c-13d9-4d51-b282-e2be09de207f.gitpod.io/", Visibility: api.PortVisibility_PORT_VISIBILITY_PUBLIC}},
			},
			URL:         "https://c95fd41c-13d9-4d51-b282-e2be09de207f.gitpod.io/",
			WorkspaceID: "c95fd41c-13d9-4d51-b282-e2be09de207f",
		},
	}

	ideServerHost  = "localhost:20000"
	workspacePort  = uint16(20001)
	supervisorPort = uint16(20002)
	workspaceHost  = fmt.Sprintf("localhost:%d", workspacePort)
	portServeHost  = fmt.Sprintf("localhost:%d", workspaces[0].Ports[0].Port)
	blobServeHost  = "localhost:20003"

	config = Config{
		TransportConfig: &TransportConfig{
			ConnectTimeout:           util.Duration(10 * time.Second),
			IdleConnTimeout:          util.Duration(60 * time.Second),
			WebsocketIdleConnTimeout: util.Duration(5 * time.Minute),
			MaxIdleConns:             100,
		},
		TheiaServer: &TheiaServer{
			Host:                    ideServerHost,
			Scheme:                  "http",
			StaticVersionPathPrefix: "/test-version.1234",
		},
		GitpodInstallation: &GitpodInstallation{
			HostName:            "gitpod.io",
			Scheme:              "https",
			WorkspaceHostSuffix: "",
		},
		WorkspacePodConfig: &WorkspacePodConfig{
			ServiceTemplate:     "http://localhost:{{ .port }}",
			PortServiceTemplate: "http://localhost:{{ .port }}",
			TheiaPort:           workspacePort,
			SupervisorPort:      supervisorPort,
			SupervisorImage:     "gitpod-io/supervisor:latest",
		},
	}
)

func configWithBlobserve() *Config {
	cfg := config
	cfg.BlobServer = &BlobServerConfig{
		Host:   blobServeHost,
		Scheme: "http",
	}
	return &cfg
}

type Target struct {
	Status  int
	Handler func(w http.ResponseWriter, r *http.Request, requestCount uint8)
}
type testTarget struct {
	Target       *Target
	RequestCount uint8
	listener     net.Listener
	server       *http.Server
}

func (tt *testTarget) Close() {
	tt.listener.Close()
	tt.server.Shutdown(context.Background())
}

// startTestTarget starts a new HTTP server that serves as some test target during the unit tests
func startTestTarget(t *testing.T, host, name string) *testTarget {
	l, err := net.Listen("tcp", host)
	if err != nil {
		t.Fatalf("cannot start fake IDE host: %q", err)
		return nil
	}

	tt := &testTarget{
		Target:   &Target{Status: http.StatusOK},
		listener: l,
	}
	srv := &http.Server{Addr: host, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			tt.RequestCount++
		}()

		if tt.Target.Handler != nil {
			tt.Target.Handler(w, r, tt.RequestCount)
			return
		}

		if tt.Target.Status == http.StatusOK {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "%s hit: %s\n", name, r.URL.String())
			return
		}

		if tt.Target.Status != 0 {
			w.WriteHeader(tt.Target.Status)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(l)
	tt.server = srv

	return tt
}

type requestModifier func(r *http.Request)

func addHeader(name string, val string) requestModifier {
	return func(r *http.Request) {
		r.Header.Add(name, val)
	}
}

func addHostHeader(r *http.Request) {
	r.Header.Add(hostBasedHeader, r.Host)
}

func addOwnerToken(instanceID, token string) requestModifier {
	return func(r *http.Request) {
		setOwnerTokenCookie(r, instanceID, token)
	}
}

func modifyRequest(r *http.Request, mod ...requestModifier) *http.Request {
	for _, m := range mod {
		m(r)
	}
	return r
}

func TestRoutes(t *testing.T) {
	type RouterFactory func(cfg *Config) WorkspaceRouter
	type Expectation struct {
		Status int
		Header http.Header
		Body   string
	}
	type Targets struct {
		IDE        *Target
		Blobserve  *Target
		Workspace  *Target
		Supervisor *Target
		Port       *Target
	}
	tests := []struct {
		Desc        string
		Config      *Config
		Request     *http.Request
		Workspaces  []WorkspaceInfo
		Router      RouterFactory
		Targets     *Targets
		Expectation Expectation
	}{
		{
			Desc: "favicon",
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"favicon.ico", nil),
				addHostHeader,
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{"Content-Length": {"50"}, "Content-Type": {"text/plain; charset=utf-8"}},
				Body:   "supervisor hit: /_supervisor/frontend/favicon.ico\n",
			},
		},
		{
			Desc: "IDE unauthorized GET /",
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL, nil),
				addHostHeader,
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{"Content-Length": {"29"}, "Content-Type": {"text/plain; charset=utf-8"}},
				Body:   "IDE hit: /test-version.1234/\n",
			},
		},
		{
			Desc:   "blobserve IDE unauthorized GET /",
			Config: configWithBlobserve(),
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL, nil),
				addHostHeader,
			),
			Expectation: Expectation{
				Status: http.StatusPermanentRedirect,
				Header: http.Header{
					"Cache-Control": {"public, max-age=31536000"},
					"Content-Type":  {"text/html; charset=utf-8"},
					"Location":      {"https://gitpod.io/blobserve/gitpod-io/ide:latest/__files__/"},
				},
				Body: "<a href=\"https://gitpod.io/blobserve/gitpod-io/ide:latest/__files__/\">Permanent Redirect</a>.\n\n",
			},
		},
		{
			Desc:   "blobserve IDE unauthorized navigate /",
			Config: configWithBlobserve(),
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL, nil),
				addHostHeader,
				addHeader("Sec-Fetch-Mode", "navigate"),
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Cache-Control":  {"public, max-age=31536000"},
					"Content-Length": {"38"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "blobserve hit: /gitpod-io/ide:latest/\n",
			},
		},
		{
			Desc:   "blobserve IDE unauthorized same-origin /",
			Config: configWithBlobserve(),
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL, nil),
				addHostHeader,
				addHeader("Sec-Fetch-Mode", "same-origin"),
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Cache-Control":  {"public, max-age=31536000"},
					"Content-Length": {"38"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "blobserve hit: /gitpod-io/ide:latest/\n",
			},
		},
		{
			Desc:   "blobserve IDE authorized GET /?foobar",
			Config: configWithBlobserve(),
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"?foobar", nil),
				addHostHeader,
				addOwnerToken(workspaces[0].InstanceID, workspaces[0].Auth.OwnerToken),
			),
			Targets: &Targets{Workspace: &Target{Status: http.StatusOK}},
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Content-Length": {"24"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "workspace hit: /?foobar\n",
			},
		},
		{
			Desc:   "blobserve IDE authorized GET /not-from-blobserve",
			Config: configWithBlobserve(),
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"not-from-blobserve", nil),
				addHostHeader,
				addOwnerToken(workspaces[0].InstanceID, workspaces[0].Auth.OwnerToken),
			),
			Targets: &Targets{Workspace: &Target{Status: http.StatusOK}, Blobserve: &Target{Status: http.StatusNotFound}},
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Content-Length": {"35"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "workspace hit: /not-from-blobserve\n",
			},
		},
		{
			Desc:   "blobserve IDE authorized GET /not-from-failed-blobserve",
			Config: configWithBlobserve(),
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"not-from-failed-blobserve", nil),
				addHostHeader,
				addOwnerToken(workspaces[0].InstanceID, workspaces[0].Auth.OwnerToken),
			),
			Targets: &Targets{Workspace: &Target{Status: http.StatusOK}, Blobserve: &Target{Status: http.StatusInternalServerError}},
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Content-Length": {"42"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "workspace hit: /not-from-failed-blobserve\n",
			},
		},
		{
			Desc: "IDE authorized GET /",
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL, nil),
				addHostHeader,
				addOwnerToken(workspaces[0].InstanceID, workspaces[0].Auth.OwnerToken),
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Content-Length": {"29"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "IDE hit: /test-version.1234/\n",
			},
		},
		{
			Desc: "CORS preflight",
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL, nil),
				addHostHeader,
				addOwnerToken(workspaces[0].InstanceID, workspaces[0].Auth.OwnerToken),
				addHeader("Origin", config.GitpodInstallation.HostName),
				addHeader("Access-Control-Request-Method", "OPTIONS"),
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Access-Control-Allow-Credentials": {"true"},
					"Access-Control-Allow-Origin":      {"gitpod.io"},
					"Access-Control-Expose-Headers":    {"Authorization"},
					"Content-Length":                   {"29"},
					"Content-Type":                     {"text/plain; charset=utf-8"},
				},
				Body: "IDE hit: /test-version.1234/\n",
			},
		},
		{
			Desc: "unauthenticated supervisor API (supervisor status)",
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"_supervisor/v1/status/supervisor", nil),
				addHostHeader,
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Content-Length": {"50"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "supervisor hit: /_supervisor/v1/status/supervisor\n",
			},
		},
		{
			Desc: "unauthenticated supervisor API (IDE status)",
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"_supervisor/v1/status/ide", nil),
				addHostHeader,
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Content-Length": {"43"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "supervisor hit: /_supervisor/v1/status/ide\n",
			},
		},
		{
			Desc: "unauthenticated supervisor API (content status)",
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"_supervisor/v1/status/content", nil),
				addHostHeader,
			),
			Expectation: Expectation{
				Status: http.StatusUnauthorized,
			},
		},
		{
			Desc: "authenticated supervisor API (content status)",
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"_supervisor/v1/status/content", nil),
				addHostHeader,
				addOwnerToken(workspaces[0].InstanceID, workspaces[0].Auth.OwnerToken),
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Content-Length": {"47"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "supervisor hit: /_supervisor/v1/status/content\n",
			},
		},
		{
			Desc: "non-existent authorized GET /",
			Request: modifyRequest(httptest.NewRequest("GET", strings.ReplaceAll(workspaces[0].URL, "c95fd41c", "00000000"), nil),
				addHostHeader,
				addOwnerToken(workspaces[0].InstanceID, workspaces[0].Auth.OwnerToken),
			),
			Expectation: Expectation{
				Status: http.StatusFound,
				Header: http.Header{
					"Content-Type": {"text/html; charset=utf-8"},
					"Location":     {"https://gitpod.io/start/#00000000-13d9-4d51-b282-e2be09de207f"},
				},
				Body: ("<a href=\"https://gitpod.io/start/#00000000-13d9-4d51-b282-e2be09de207f\">Found</a>.\n\n"),
			},
		},
		{
			Desc: "non-existent unauthorized GET /",
			Request: modifyRequest(httptest.NewRequest("GET", strings.ReplaceAll(workspaces[0].URL, "c95fd41c", "00000000"), nil),
				addHostHeader,
			),
			Expectation: Expectation{
				Status: http.StatusFound,
				Header: http.Header{
					"Content-Type": {"text/html; charset=utf-8"},
					"Location":     {"https://gitpod.io/start/#00000000-13d9-4d51-b282-e2be09de207f"},
				},
				Body: ("<a href=\"https://gitpod.io/start/#00000000-13d9-4d51-b282-e2be09de207f\">Found</a>.\n\n"),
			},
		},
		{
			Desc:   "blobserve supervisor frontend /worker-proxy.js",
			Config: configWithBlobserve(),
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"_supervisor/frontend/worker-proxy.js", nil),
				addHostHeader,
			),
			Expectation: Expectation{
				Status: http.StatusOK,
				Header: http.Header{
					"Content-Length": {"60"},
					"Content-Type":   {"text/plain; charset=utf-8"},
				},
				Body: "blobserve hit: /gitpod-io/supervisor:latest/worker-proxy.js\n",
			},
		},
		{
			Desc:   "blobserve supervisor frontend /main.js",
			Config: configWithBlobserve(),
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"_supervisor/frontend/main.js", nil),
				addHostHeader,
			),
			Expectation: Expectation{
				Status: http.StatusSeeOther,
				Header: http.Header{
					"Content-Type": {"text/html; charset=utf-8"},
					"Location":     {"https://gitpod.io/blobserve/gitpod-io/supervisor:latest/__files__/main.js"},
				},
				Body: "<a href=\"https://gitpod.io/blobserve/gitpod-io/supervisor:latest/__files__/main.js\">See Other</a>.\n\n",
			},
		},
		{
			Desc:   "blobserve supervisor frontend /main.js retry on timeout",
			Config: configWithBlobserve(),
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].URL+"_supervisor/frontend/main.js", nil),
				addHostHeader,
			),
			Targets: &Targets{Blobserve: &Target{
				Handler: func(w http.ResponseWriter, r *http.Request, requestCount uint8) {
					if requestCount == 0 {
						w.WriteHeader(http.StatusServiceUnavailable)
						io.WriteString(w, "timeout")
						return
					}
					w.WriteHeader(http.StatusOK)
				},
			}},
			Expectation: Expectation{
				Status: http.StatusSeeOther,
				Header: http.Header{
					"Content-Type": {"text/html; charset=utf-8"},
					"Location":     {"https://gitpod.io/blobserve/gitpod-io/supervisor:latest/__files__/main.js"},
				},
				Body: "<a href=\"https://gitpod.io/blobserve/gitpod-io/supervisor:latest/__files__/main.js\">See Other</a>.\n\n",
			},
		},
		{
			Desc: "port GET 404",
			Request: modifyRequest(httptest.NewRequest("GET", workspaces[0].Ports[0].Url+"this-does-not-exist", nil),
				addHostHeader,
				addOwnerToken(workspaces[0].InstanceID, workspaces[0].Auth.OwnerToken),
			),
			Targets: &Targets{Port: &Target{Status: http.StatusNotFound}},
			Expectation: Expectation{
				Header: http.Header{"Content-Length": {"0"}},
				Status: http.StatusNotFound,
			},
		},
	}

	log.Init("ws-proxy-test", "", false, true)
	log.Log.Logger.SetLevel(logrus.ErrorLevel)

	defaultTargets := &Targets{
		IDE:        &Target{Status: http.StatusOK},
		Blobserve:  &Target{Status: http.StatusOK},
		Port:       &Target{Status: http.StatusOK},
		Supervisor: &Target{Status: http.StatusOK},
		Workspace:  &Target{Status: http.StatusOK},
	}
	targets := make(map[string]*testTarget)
	controlTarget := func(target *Target, name, host string) {
		_, runs := targets[name]
		if runs && target == nil {
			targets[name].Close()
			delete(targets, name)
			return
		}

		if !runs && target != nil {
			targets[name] = startTestTarget(t, host, name)
			runs = true
		}

		if runs {
			targets[name].Target = target
			targets[name].RequestCount = 0
		}
	}
	defer func() {
		for _, c := range targets {
			c.Close()
		}
	}()

	for _, test := range tests {
		if test.Targets == nil {
			test.Targets = defaultTargets
		}

		t.Run(test.Desc, func(t *testing.T) {
			controlTarget(test.Targets.IDE, "IDE", ideServerHost)
			controlTarget(test.Targets.Blobserve, "blobserve", blobServeHost)
			controlTarget(test.Targets.Port, "port", portServeHost)
			controlTarget(test.Targets.Workspace, "workspace", workspaceHost)
			controlTarget(test.Targets.Supervisor, "supervisor", fmt.Sprintf("localhost:%d", supervisorPort))

			cfg := config
			if test.Config != nil {
				cfg = *test.Config
			}
			router := HostBasedRouter(hostBasedHeader, wsHostSuffix)
			if test.Router != nil {
				router = test.Router(&cfg)
			}

			proxy := NewWorkspaceProxy(":8080", cfg, router, &fakeWsInfoProvider{infos: workspaces})
			handler, err := proxy.Handler()
			if err != nil {
				t.Fatalf("cannot create proxy handler: %q", err)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, test.Request)
			resp := rec.Result()

			body, _ := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			act := Expectation{
				Status: resp.StatusCode,
				Body:   string(body),
				Header: resp.Header,
			}
			if _, ok := act.Header["Date"]; ok {
				delete(act.Header, "Date")
			}
			if len(act.Header) == 0 {
				act.Header = nil
			}

			if diff := cmp.Diff(test.Expectation, act); diff != "" {
				t.Errorf("Expectation mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

type fakeWsInfoProvider struct {
	infos []WorkspaceInfo
}

// GetWsInfoByID returns the workspace for the given ID
func (p *fakeWsInfoProvider) WorkspaceInfo(workspaceID string) *WorkspaceInfo {
	for _, nfo := range p.infos {
		if nfo.WorkspaceID == workspaceID {
			return &nfo
		}
	}

	return nil
}

// WorkspaceCoords returns the workspace coords for a public port
func (p *fakeWsInfoProvider) WorkspaceCoords(wsProxyPort string) *WorkspaceCoords {
	for _, info := range p.infos {
		if info.IDEPublicPort == wsProxyPort {
			return &WorkspaceCoords{
				ID:   info.WorkspaceID,
				Port: "",
			}
		}

		for _, portInfo := range info.Ports {
			if portInfo.PublicPort == wsProxyPort {
				return &WorkspaceCoords{
					ID:   info.WorkspaceID,
					Port: strconv.Itoa(int(portInfo.Port)),
				}
			}
		}
	}

	return nil
}

func TestRemoveSensitiveCookies(t *testing.T) {
	var (
		domain         = "test-domain.com"
		sessionCookie  = &http.Cookie{Domain: domain, Name: "_test_domain_com_", Value: "fobar"}
		portAuthCookie = &http.Cookie{Domain: domain, Name: "_test_domain_com_ws_77f6b236_3456_4b88_8284_81ca543a9d65_port_auth_", Value: "some-token"}
		ownerCookie    = &http.Cookie{Domain: domain, Name: "_test_domain_com_ws_77f6b236_3456_4b88_8284_81ca543a9d65_owner_", Value: "some-other-token"}
		miscCookie     = &http.Cookie{Domain: domain, Name: "some-other-cookie", Value: "I like cookies"}
	)

	tests := []struct {
		Name     string
		Input    []*http.Cookie
		Expected []*http.Cookie
	}{
		{"no cookies", []*http.Cookie{}, []*http.Cookie{}},
		{"session cookie", []*http.Cookie{sessionCookie, miscCookie}, []*http.Cookie{miscCookie}},
		{"portAuth cookie", []*http.Cookie{portAuthCookie, miscCookie}, []*http.Cookie{miscCookie}},
		{"owner cookie", []*http.Cookie{ownerCookie, miscCookie}, []*http.Cookie{miscCookie}},
		{"misc cookie", []*http.Cookie{miscCookie}, []*http.Cookie{miscCookie}},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			res := removeSensitiveCookies(test.Input, domain)
			if diff := cmp.Diff(test.Expected, res); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}
