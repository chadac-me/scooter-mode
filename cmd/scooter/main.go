// Command scooter connects local tools to a Scooter server.
//
//	scooter acp --server URL [flags]   ACP agent on stdio (agent-shell)
//	scooter tunnel [flags]             a conversation's sshd on stdio (ssh ProxyCommand)
//	scooter version                    print the release version
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is set at release build time with -ldflags -X.
var version = "dev"

func main() {
	log.SetFlags(0)
	log.SetPrefix("scooter: ")
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "tunnel" {
		tunnelMain(os.Args[2:])
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "acp" {
		fmt.Fprintln(os.Stderr, "usage: scooter acp --server URL [--conversation ID] [--model M] [--project] [--replay-turns N] [--headers-command CMD | --headers-file PATH]\n       scooter acp --self   (inside a Scooter sandbox: chat with its own conversation)")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("acp", flag.ExitOnError)
	server := fs.String("server", "", "Scooter base URL")
	conversation := fs.String("conversation", "", "attach the first new session to this conversation id")
	model := fs.String("model", "", "model for new conversations (default: the server's)")
	project := fs.Bool("project", false, "put the working directory's git remote and branch in the first prompt")
	headersCmd := fs.String("headers-command", "", "shell command printing the auth headers as a JSON object; {retry} becomes t after an auth wall")
	headersFile := fs.String("headers-file", "", "file holding the auth headers as a JSON object")
	replayTurns := fs.Int("replay-turns", 0, "on load, replay only the last N user turns (0: all)")
	requestTimeout := fs.Duration("request-timeout", defaultTimeout, "give up on a request that isn't a stream after this long (0: never)")
	self := fs.Bool("self", false, "use the sandbox's self-access to its own conversation (SCOOTER_SELF_URL, SCOOTER_SELF_TOKEN_PATH, CONVERSATION_ID)")
	_ = fs.Parse(os.Args[2:]) // ExitOnError: it exits rather than returning one.
	src := headerSource{command: *headersCmd, file: *headersFile}
	opts := agentOptions{conversation: *conversation, model: *model, project: *project, replayTurns: *replayTurns}
	if *self {
		*server = os.Getenv("SCOOTER_SELF_URL")
		src = headerSource{bearer: os.Getenv("SCOOTER_SELF_TOKEN_PATH")}
		opts = agentOptions{conversation: os.Getenv("CONVERSATION_ID"), self: true, approveURL: os.Getenv("CONVERSATION_URL"), replayTurns: *replayTurns}
		if *server == "" || src.bearer == "" || opts.conversation == "" {
			log.Fatal("--self needs SCOOTER_SELF_URL, SCOOTER_SELF_TOKEN_PATH and CONVERSATION_ID")
		}
	}
	if *server == "" {
		log.Fatal("--server is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := newClient(*server, src)
	c.timeout = *requestTimeout
	out := newRPC(os.Stdout)
	a := newAgent(ctx, c, out, opts)
	if err := out.serve(ctx, os.Stdin, a.handle, a.notify); err != nil {
		log.Fatal(err)
	}
}

func tunnelMain(args []string) {
	fs := flag.NewFlagSet("tunnel", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: scooter tunnel --server URL --conversation ID [--headers-command CMD | --headers-file PATH]\n       scooter tunnel --spec-command CMD | --spec-file PATH   ({\"url\", \"conversation\", \"headers\"})\n\nAn ssh ProxyCommand: ssh -o ProxyCommand='scooter tunnel ...' root@sandbox")
		fs.PrintDefaults()
	}
	server := fs.String("server", "", "Scooter base URL")
	conversation := fs.String("conversation", "", "conversation id")
	headersCmd := fs.String("headers-command", "", "shell command printing the auth headers as a JSON object; {retry} becomes t after an auth wall")
	headersFile := fs.String("headers-file", "", "file holding the auth headers as a JSON object")
	specCmd := fs.String("spec-command", "", "shell command printing the server, conversation and headers as JSON; {retry} as for --headers-command")
	specFile := fs.String("spec-file", "", "file holding the JSON --spec-command prints")
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for the sandbox and its ssh service")
	_ = fs.Parse(args) // ExitOnError: it exits rather than returning one.

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	src := headerSource{command: *headersCmd, file: *headersFile}
	if *specCmd != "" || *specFile != "" {
		src = headerSource{spec: *specCmd, specFile: *specFile}
		spec, err := src.loadSpec(ctx, false)
		if err != nil {
			log.Fatal(err)
		}
		*server, *conversation = spec.URL, spec.Conversation
	}
	if *server == "" || *conversation == "" {
		fs.Usage()
		os.Exit(2)
	}
	err := runTunnel(ctx, tunnelOptions{
		server: *server, conversation: *conversation, source: src,
		wait: *wait, poll: 1500 * time.Millisecond,
		stdin: os.Stdin, stdout: os.Stdout,
		log: func(s string) { log.Print(s) },
	})
	if err != nil {
		log.Fatal(err)
	}
}
