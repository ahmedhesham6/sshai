package testfixtures

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
	"github.com/restatedev/sdk-go/server"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const RestateImage = "docker.io/restatedev/restate:1.6.2@sha256:e8e072c174bb0f997331c055b7bd84cae6ddc8c7c31ac7c8a197bdc935eec2f5"

type RestateEnvironment struct {
	endpoint      *httptest.Server
	enabled       atomic.Bool
	ingressClient *ingress.Client
}

func StartRestate(t *testing.T, services ...restate.ServiceDefinition) *RestateEnvironment {
	t.Helper()
	restateServer := server.NewRestate()
	for _, service := range services {
		restateServer.Bind(service)
	}
	handler, err := restateServer.Handler()
	if err != nil {
		t.Fatalf("build Restate handler: %v", err)
	}
	environment := &RestateEnvironment{}
	environment.enabled.Store(true)
	environment.endpoint = httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !environment.enabled.Load() {
			http.Error(response, "deployment unavailable", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(response, request)
	}))
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	environment.endpoint.Config.Protocols = &protocols
	environment.endpoint.EnableHTTP2 = true
	environment.endpoint.Start()
	t.Cleanup(environment.endpoint.Close)
	sdkPort, err := strconv.Atoi(strings.Split(environment.endpoint.URL, ":")[2])
	if err != nil {
		t.Fatalf("read Restate endpoint port: %v", err)
	}
	container, err := testcontainers.Run(
		t.Context(), RestateImage,
		testcontainers.WithEnv(map[string]string{
			"RUST_LOG":                              "warn",
			"RESTATE_META__REST_ADDRESS":            "0.0.0.0:9070",
			"RESTATE_WORKER__INGRESS__BIND_ADDRESS": "0.0.0.0:8080",
		}),
		testcontainers.WithExposedPorts("9070/tcp", "8080/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForAll(
			wait.ForHTTP("/health").WithPort("9070/tcp"),
			wait.ForHTTP("/restate/health").WithPort("8080/tcp"),
		)),
		testcontainers.WithHostPortAccess(sdkPort),
	)
	if err != nil {
		t.Fatalf("start Restate: %v", err)
	}
	testcontainers.CleanupContainer(t, container)
	adminPort, err := container.MappedPort(t.Context(), "9070/tcp")
	if err != nil {
		t.Fatalf("map Restate admin port: %v", err)
	}
	ingressPort, err := container.MappedPort(t.Context(), "8080/tcp")
	if err != nil {
		t.Fatalf("map Restate ingress port: %v", err)
	}
	environment.ingressClient = ingress.NewClient(fmt.Sprintf("http://localhost:%d", ingressPort.Num()))
	body := fmt.Sprintf(`{"uri":"http://%s:%d"}`, testcontainers.HostInternal, sdkPort)
	response, err := http.Post(
		fmt.Sprintf("http://localhost:%d/deployments", adminPort.Num()),
		"application/json", bytes.NewBufferString(body),
	)
	if err != nil {
		t.Fatalf("register Restate endpoint: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("register Restate endpoint status = %d: %s", response.StatusCode, body)
	}
	return environment
}

func (environment *RestateEnvironment) Ingress() *ingress.Client {
	return environment.ingressClient
}

func (environment *RestateEnvironment) TerminateEndpoint() {
	environment.enabled.Store(false)
	environment.endpoint.CloseClientConnections()
}

func (environment *RestateEnvironment) ResumeEndpoint() {
	environment.enabled.Store(true)
}
