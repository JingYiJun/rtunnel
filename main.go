package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rtunnel/logging"
	"rtunnel/tunnel"
)

type cliOptions struct {
	certFile string
	keyFile  string
	secure   bool
	debug    bool
	args     []string
}

func main() {
	opts := parseFlags()
	logging.SetDebug(opts.debug)

	if len(opts.args) != 2 {
		fmt.Fprintf(os.Stderr, "Error: invalid number of arguments\n\n")
		flag.Usage()
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if isRemoteEndpoint(opts.args[0]) {
		if err := runClient(ctx, opts); err != nil {
			fmt.Fprintf(os.Stderr, "Client error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := runServer(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() cliOptions {
	var opts cliOptions

	flag.StringVar(&opts.certFile, "cert", "", "TLS certificate file (server mode)")
	flag.StringVar(&opts.keyFile, "key", "", "TLS private key file (server mode)")
	flag.BoolVar(&opts.secure, "secure", false, "Enable secure mode (server: TLS, client: verify certificates)")
	flag.BoolVar(&opts.debug, "debug", false, "Enable debug logging")
	flag.BoolVar(&opts.secure, "s", false, "Short for --secure")
	flag.BoolVar(&opts.debug, "d", false, "Short for --debug")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "RTunnel - Reliable TCP over WebSocket tunnel\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  Server mode:  rtunnel <target_addr> <listen_addr> [options]\n")
		fmt.Fprintf(os.Stderr, "  Client mode:  rtunnel <remote_url> <local_addr> [options]\n")
		fmt.Fprintf(os.Stderr, "  Client (stdio): rtunnel <remote_url> stdio://<target_addr> [options]\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  Server (HTTP):   rtunnel localhost:22 0.0.0.0:8080\n")
		fmt.Fprintf(os.Stderr, "  Server (HTTPS):  rtunnel localhost:22 0.0.0.0:8443 --cert server.crt --key server.key\n")
		fmt.Fprintf(os.Stderr, "  Client (WS):     rtunnel ws://localhost:8080 127.0.0.1:2222\n")
		fmt.Fprintf(os.Stderr, "  Client (WSS):    rtunnel wss://localhost:8443 127.0.0.1:2222 --secure\n")
		fmt.Fprintf(os.Stderr, "  Client (stdio):  rtunnel ws://localhost:8080 stdio://127.0.0.1:22\n")
		fmt.Fprintf(os.Stderr, "  SSH ProxyCommand: ssh -o ProxyCommand='rtunnel ws://server:8080 stdio://%%h:%%p' user@host\n")
	}

	flag.CommandLine.Parse(reorderArgs(os.Args[1:]))
	opts.args = flag.Args()
	return opts
}

func runClient(ctx context.Context, opts cliOptions) error {
	remoteURL, err := normalizeRemoteURL(opts.args[0])
	if err != nil {
		return err
	}

	// 检测是否为 stdio 模式
	isStdio := strings.HasPrefix(opts.args[1], "stdio://")

	if isStdio {
		// stdio 模式：直接使用 stdin/stdout
		client := tunnel.NewClient(remoteURL, "", !opts.secure)

		fmt.Fprintf(os.Stderr, "Starting client (stdio mode)...\n")
		fmt.Fprintf(os.Stderr, "Remote URL: %s\n", remoteURL)
		if opts.secure {
			fmt.Fprintf(os.Stderr, "TLS verification: enabled\n")
		} else {
			fmt.Fprintf(os.Stderr, "TLS verification: disabled (use --secure to enable)\n")
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- client.StartStdio()
		}()

		select {
		case <-ctx.Done():
			client.Stop()
			select {
			case err := <-errCh:
				if err != nil && !errors.Is(err, context.Canceled) {
					return err
				}
			case <-time.After(5 * time.Second):
				return errors.New("client shutdown timeout")
			}
			return nil
		case err := <-errCh:
			client.Stop()
			return err
		}
	}

	// TCP 监听模式
	localAddr, err := normalizeAddress(opts.args[1], "127.0.0.1")
	if err != nil {
		return fmt.Errorf("invalid local address: %w", err)
	}

	client := tunnel.NewClient(remoteURL, localAddr, !opts.secure)

	fmt.Printf("Starting client...\n")
	fmt.Printf("Remote URL: %s\n", remoteURL)
	fmt.Printf("Local bind: %s\n", localAddr)
	if opts.secure {
		fmt.Println("TLS verification: enabled")
	} else {
		fmt.Println("TLS verification: disabled (use --secure to enable)")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Start()
	}()

	select {
	case <-ctx.Done():
		client.Stop()
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
		case <-time.After(5 * time.Second):
			return errors.New("client shutdown timeout")
		}
		return nil
	case err := <-errCh:
		client.Stop()
		return err
	}
}

func runServer(ctx context.Context, opts cliOptions) error {
	targetAddr, err := normalizeAddress(opts.args[0], "127.0.0.1")
	if err != nil {
		return fmt.Errorf("invalid target address: %w", err)
	}
	listenAddr, err := normalizeAddress(opts.args[1], "0.0.0.0")
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}

	if opts.secure && (opts.certFile == "" || opts.keyFile == "") {
		return errors.New("secure mode requires both --cert and --key")
	}

	server := tunnel.NewServer(targetAddr, listenAddr)
	server.CertFile = opts.certFile
	server.KeyFile = opts.keyFile

	fmt.Printf("Starting server...\n")
	fmt.Printf("Target: %s\n", targetAddr)
	fmt.Printf("Listen: %s\n", listenAddr)
	if opts.certFile != "" && opts.keyFile != "" {
		fmt.Printf("TLS: enabled (%s, %s)\n", opts.certFile, opts.keyFile)
	} else {
		fmt.Println("TLS: disabled (HTTP mode)")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Stop(shutdownCtx); err != nil {
			return err
		}
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}

func reorderArgs(argv []string) []string {
	var flags []string
	var positional []string
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "--" {
			positional = append(positional, argv[i+1:]...)
			break
		}
		if strings.HasPrefix(tok, "-") {
			flags = append(flags, tok)
			if needsValue(tok) && !strings.Contains(tok, "=") && i+1 < len(argv) {
				flags = append(flags, argv[i+1])
				i++
			}
			continue
		}
		positional = append(positional, tok)
	}
	return append(flags, positional...)
}

func needsValue(tok string) bool {
	if strings.HasPrefix(tok, "--") {
		name := strings.TrimPrefix(tok, "--")
		if idx := strings.Index(name, "="); idx >= 0 {
			name = name[:idx]
		}
		switch name {
		case "cert", "key":
			return true
		}
	}
	return false
}

func isRemoteEndpoint(value string) bool {
	return strings.HasPrefix(value, "ws://") ||
		strings.HasPrefix(value, "wss://") ||
		strings.HasPrefix(value, "http://") ||
		strings.HasPrefix(value, "https://")
}

func normalizeRemoteURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("remote url is empty")
	}
	switch {
	case strings.HasPrefix(raw, "ws://"), strings.HasPrefix(raw, "wss://"):
		return raw, nil
	case strings.HasPrefix(raw, "http://"):
		return "ws" + raw[4:], nil
	case strings.HasPrefix(raw, "https://"):
		return "wss" + raw[5:], nil
	default:
		if _, err := url.Parse(raw); err != nil {
			return "", fmt.Errorf("invalid remote url: %w", err)
		}
		return "", fmt.Errorf("remote url must start with ws://, wss://, http://, or https://: %s", raw)
	}
}

func normalizeAddress(addr, defaultHost string) (string, error) {
	if addr == "" {
		return "", errors.New("address is empty")
	}
	if _, err := strconv.Atoi(addr); err == nil {
		return net.JoinHostPort(defaultHost, addr), nil
	}
	if !strings.Contains(addr, ":") {
		return "", fmt.Errorf("address must include port: %s", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = defaultHost
	}
	return net.JoinHostPort(host, port), nil
}
