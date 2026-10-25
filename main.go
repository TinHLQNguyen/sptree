// Command sptree is a fast, keyboard-driven tree explorer for SharePoint and OneDrive.
//
// It runs a small local web server, signs in to Microsoft 365 with the OAuth device-code
// flow, and serves a single-page UI that browses your document libraries through
// Microsoft Graph (read-only).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"
)

var version = "dev"

const usage = `sptree — browse SharePoint and OneDrive as a tree

Usage:
  sptree [flags]          start the local app and open it in your browser
  sptree logout           forget the saved sign-in

Flags:
`

func main() {
	loadDotEnv(".env")
	clientID := flag.String("client-id", envOr("SPTREE_CLIENT_ID", ""), "Entra ID (Azure AD) application (client) ID [env SPTREE_CLIENT_ID]")
	tenant := flag.String("tenant", envOr("SPTREE_TENANT", "organizations"), "tenant ID or domain, or 'organizations' [env SPTREE_TENANT]")
	addr := flag.String("addr", envOr("SPTREE_ADDR", "127.0.0.1:8765"), "address to listen on (loopback only)")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser automatically")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()

	if *showVersion {
		fmt.Println("sptree", version)
		return
	}
	cache := envOr("SPTREE_TOKEN_CACHE", defaultCachePath())
	loginBase := envOr("SPTREE_LOGIN_URL", "https://login.microsoftonline.com")

	if flag.Arg(0) == "logout" {
		NewAuth(*clientID, *tenant, loginBase, cache, os.Stderr).Logout()
		fmt.Println("Signed out. Removed", cache)
		return
	}
	if *clientID == "" {
		fmt.Fprintln(os.Stderr, "sptree needs an Entra ID app registration.\n\n"+
			"  1. Register an app (or ask your IT admin for one) — see README \"Set up sign-in\".\n"+
			"  2. Run:  sptree -client-id <application-id> [-tenant <your-tenant>]\n"+
			"     or set SPTREE_CLIENT_ID.")
		os.Exit(2)
	}

	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		fatal("bad -addr:", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		fatal("refusing to listen on a non-loopback address:", host)
	}

	auth := NewAuth(*clientID, *tenant, loginBase, cache, os.Stderr)
	graph := NewGraph(envOr("SPTREE_GRAPH_URL", "https://graph.microsoft.com/v1.0"), auth)
	srv := &Server{auth: auth, graph: graph, port: port}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fatal("cannot listen on", *addr+":", err)
	}
	url := "http://" + ln.Addr().String() + "/"
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	fmt.Fprintf(os.Stderr, "sptree %s running at %s  (Ctrl+C to quit)\n", version, url)
	if !auth.SignedIn() {
		auth.StartDeviceLogin()
	}
	if !*noBrowser {
		openBrowser(url)
	}

	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// loadDotEnv sets variables from a KEY=value file (# comments, optional quotes).
// Variables already set in the environment win. A missing file is fine.
func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set && k != "" {
			os.Setenv(k, v)
		}
	}
}

func fatal(a ...any) {
	fmt.Fprintln(os.Stderr, append([]any{"sptree:"}, a...)...)
	os.Exit(1)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
