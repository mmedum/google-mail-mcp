// Command google-mail-mcp is an MCP server for Gmail.
//
// With no subcommand it serves MCP over stdio. Stdout carries JSON-RPC
// frames and nothing else: this file is the one place that names the
// process's streams, and it passes them down as io.Writer so nothing
// below can print to the wrong one (CLAUDE.md rule 2).
//
//	google-mail-mcp [serve]           run the MCP server over stdio
//	google-mail-mcp login             authorize a Google account
//	google-mail-mcp logout            revoke and forget the stored token
//	google-mail-mcp status [--json]   show the profile, without contacting Google
//	google-mail-mcp doctor            check the setup against Google
//	google-mail-mcp --version | --dump-schemas
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/internal/app"
	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/redact"
	"github.com/mmedum/google-mail-mcp/internal/server"
	"github.com/mmedum/google-mail-mcp/internal/tools"
	"github.com/mmedum/google-mail-mcp/internal/version"
)

func main() {
	//nolint:forbidigo // the one place the process's streams are named
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

// run is main with its inputs passed in, so a test drives every path,
// the serve path included, and reads the exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, env func(string) string) int {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return serve(args[1:], stdin, stdout, stderr, env)
		case "login":
			return cmdLogin(args[1:], stdout, stderr, env)
		case "logout":
			return cmdLogout(args[1:], stdout, stderr, env)
		case "status":
			return cmdStatus(args[1:], stdout, stderr, env)
		case "doctor":
			return cmdDoctor(args[1:], stdout, stderr, env)
		case "help", "-h", "-help", "--help":
			usage(stdout)
			return 0
		}
		// Anything else that is not a flag was meant as a command.
		// Falling through would start the server, which blocks on stdin
		// and looks like a hang.
		if !strings.HasPrefix(args[0], "-") {
			usage(stderr)
			return fail(stderr, "unknown command %q", args[0])
		}
	}
	return serve(args, stdin, stdout, stderr, env)
}

func usage(w io.Writer) {
	outf(w, `google-mail-mcp — MCP server for Gmail

Usage:
  google-mail-mcp [serve]                    run the MCP server over stdio
  google-mail-mcp login [--no-browser] [--consent]
                                             authorize a Google account
  google-mail-mcp logout                     revoke and delete the stored token
  google-mail-mcp status [--json]            show the profile and settings
  google-mail-mcp doctor                     check the setup against Google
  google-mail-mcp --version
  google-mail-mcp --dump-schemas             print the tool surface as JSON

Settings come from GMAIL_* environment variables; every command also
accepts the matching flags (run one with -h).
`)
}

// fail prints one error line through the redactor and returns exit 1.
func fail(w io.Writer, format string, args ...any) int {
	outf(w, "google-mail-mcp: %s\n", fmt.Sprintf(format, args...))
	return 1
}

// outf is the only way this command writes to a stream. Everything goes
// through the redactor, so an address or client id that arrives inside
// an error another package formatted is masked like one printed here.
func outf(w io.Writer, format string, args ...any) {
	_, _ = io.WriteString(w, redact.Text(fmt.Sprintf(format, args...)))
}

// parseConfig defines the shared settings on fs, lets define add the
// command's own flags, and builds the configuration. A non-nil exit code
// means the caller returns it: 0 after -h, 1 after an error.
func parseConfig(name string, args []string, stderr io.Writer, env func(string) string, define func(*flag.FlagSet)) (config.Config, *int) {
	fs := flag.NewFlagSet("google-mail-mcp "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	settings := config.Define(fs, env)
	if define != nil {
		define(fs)
	}
	if err := fs.Parse(args); err != nil {
		code := 2
		if errors.Is(err, flag.ErrHelp) {
			code = 0
		}
		return config.Config{}, &code
	}
	if fs.NArg() > 0 {
		code := fail(stderr, "%s takes no arguments, got %q", name, fs.Arg(0))
		return config.Config{}, &code
	}
	cfg, err := settings.Build()
	if err != nil {
		code := fail(stderr, "%v", err)
		return config.Config{}, &code
	}
	return cfg, nil
}

func serve(args []string, stdin io.Reader, stdout, stderr io.Writer, env func(string) string) int {
	var showVersion, dumpSchemas bool
	cfg, code := parseConfig("serve", args, stderr, env, func(fs *flag.FlagSet) {
		fs.BoolVar(&showVersion, "version", false, "print the version and exit")
		fs.BoolVar(&dumpSchemas, "dump-schemas", false, "print the full tool surface as JSON and exit")
	})
	if showVersion {
		outf(stdout, "%s\n", version.Info())
		return 0
	}
	if code != nil {
		return *code
	}
	logger := config.NewLogger(cfg, stderr)

	if dumpSchemas {
		// The one time stdout carries something that is not a frame, and
		// the one time no session exists to corrupt.
		err := server.DumpSchemas(context.Background(), stdout, server.Deps{
			Deps: tools.Deps{Config: cfg}, Version: version.String(),
		})
		if err != nil {
			return fail(stderr, "dump schemas: %v", err)
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := app.Assemble(ctx, cfg, app.Options{
		Env: env, Keyring: keyringBackend, Logger: logger, UserAgent: "google-mail-mcp/" + version.String(),
	})
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if rt.CredentialsErr != nil {
		// Not a reason to refuse to start: the client lists tools before
		// anyone logs in, and exiting reads as a crash in every host.
		logger.Warn("no usable credentials; every tool answers [auth] until `google-mail-mcp login` succeeds",
			"reason", redact.Text(rt.CredentialsErr.Error()))
	} else {
		// Warmed off the startup path: the source is safe for concurrent
		// use, and the first tool call reuses the token. serve waits for
		// it on the way out, after canceling ctx, so no log line is
		// written after run returns.
		warmed := make(chan struct{})
		defer func() { stop(); <-warmed }()
		go func() {
			defer close(warmed)
			if _, err := rt.TokenSource.Token(); err != nil {
				logger.Warn("credential check failed; tools answer [auth] until `google-mail-mcp login` succeeds",
					"reason", redact.Text(err.Error()))
				return
			}
			logger.Debug("credential check ok")
		}()
	}
	if w := rt.ScopeWarning(); w != "" {
		logger.Warn(w)
	}

	logger.Info("serving", "version", version.String(), "runtime", rt)
	transport := &mcp.IOTransport{Reader: readCloser(stdin), Writer: writeCloser(stdout)}
	if err := rt.Server(version.String()).Run(ctx, transport); err != nil && !isDisconnect(err) {
		return fail(stderr, "server: %v", err)
	}
	logger.Info("client disconnected")
	return 0
}

// readCloser and writeCloser adapt the streams to the transport, which
// wants closers. The host owns stdin and stdout, so where a Close has to
// be invented it does nothing.
func readCloser(r io.Reader) io.ReadCloser {
	if rc, ok := r.(io.ReadCloser); ok {
		return rc
	}
	return io.NopCloser(r)
}

func writeCloser(w io.Writer) io.WriteCloser {
	if wc, ok := w.(io.WriteCloser); ok {
		return wc
	}
	return nopWriteCloser{w}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// JSON-RPC codes the SDK uses for a closing connection.
const (
	codeServerClosing = -32004
	codeClientClosing = -32003
)

// isDisconnect reports the ordinary end of a stdio session. The SDK
// reports a closed connection as JSON-RPC -32004 or -32003 with the EOF
// only as message text, so errors.Is(err, io.EOF) misses it and the
// process would exit non-zero, which hosts log as a crash. The code is
// matched, never the text (standard §11).
func isDisconnect(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return true
	}
	var je *jsonrpc.Error
	if errors.As(err, &je) {
		return je.Code == codeServerClosing || je.Code == codeClientClosing
	}
	return false
}
