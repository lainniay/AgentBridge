package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lainniay/AgentBridge/internal/adapter/antigravity"
	"github.com/lainniay/AgentBridge/internal/adapter/codex"
	"github.com/lainniay/AgentBridge/internal/api"
	"github.com/lainniay/AgentBridge/internal/hub"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(out, "agentbridge daemon | install | uninstall | doctor | hook <pre-invocation|pre-tool-use|stop> | mcp-stdio | status | link | unlink")
		return nil
	}
	endpoint := api.Endpoint(api.Address())
	switch args[0] {
	case "hook":
		event := ""
		if len(args) == 2 {
			event = args[1]
		}
		runHook(in, out, endpoint, event)
		return nil
	case "daemon":
		flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
		flags.SetOutput(stderr)
		workspace := flags.String("workspace", "", "optional fallback workspace")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected daemon arguments")
		}
		return daemon(*workspace, stderr)
	case "install":
		return installCmd(args[1:], out, stderr)
	case "uninstall":
		return uninstallCmd(args[1:], out, stderr)
	case "doctor":
		return doctorCmd(args[1:], out, stderr, endpoint)
	case "mcp-stdio":
		return stdio(args[1:], in, out, stderr, endpoint)
	case "status", "link", "unlink":
		return control(args[0], args[1:], out, stderr, endpoint)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func listenLocal(addr string) (net.Listener, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	network := "tcp"
	listenAddr := addr
	if host == "" || strings.EqualFold(host, "localhost") {
		network = "tcp4"
		listenAddr = net.JoinHostPort("127.0.0.1", port)
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			network = "tcp4"
		} else {
			network = "tcp6"
		}
	}
	ln, err := net.Listen(network, listenAddr)
	if err != nil {
		return nil, "", err
	}
	return ln, host, nil
}

func waitForHealthy(endpoint string, stopped <-chan error) error {
	client := httpClient(500*time.Millisecond, 200*time.Millisecond)
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-stopped:
			if err == nil {
				return errors.New("daemon server exited before becoming ready")
			}
			return err
		default:
		}
		resp, err := client.Get(endpoint + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("healthz returned %s", resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("daemon failed readiness probe at %s: %w", endpoint, lastErr)
	}
	return fmt.Errorf("daemon failed readiness probe at %s", endpoint)
}

func daemon(workspace string, stderr io.Writer) error {
	if strings.TrimSpace(workspace) != "" {
		abs, err := filepath.Abs(workspace)
		if err != nil {
			return err
		}
		workspace, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return err
		}
	}
	h, err := hub.Open("")
	if err != nil {
		return err
	}
	addr := api.Address()
	listener, host, err := listenLocal(addr)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	service := api.New(ctx, h, workspace,
		antigravity.New(os.Getenv("AGENTBRIDGE_ANTIGRAVITY_ROOT")),
		codex.New(os.Getenv("AGENTBRIDGE_CODEX_ROOT")))
	server := &http.Server{
		Handler:           service.Handler(host),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	endpoint := api.Endpoint(listener.Addr().String())
	if err := waitForHealthy(endpoint, stopped); err != nil {
		_ = server.Close()
		cancel()
		service.Wait()
		return err
	}
	codexHome, _ := defaultCodexHome()
	fmt.Fprintf(stderr, "AgentBridge listening at %s (home=%s, codex_home=%s, rooms=%d)\n",
		endpoint, h.Home(), codexHome, len(h.Rooms("")))
	select {
	case err := <-stopped:
		cancel()
		service.Wait()
		return err
	case <-ctx.Done():
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		err := server.Shutdown(shutdown)
		service.Wait()
		return err
	}
}

func httpClient(timeout, connectTimeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: connectTimeout}).DialContext
	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}
