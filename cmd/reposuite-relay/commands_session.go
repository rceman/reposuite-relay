package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/session"
	"os"
	"strings"
	"time"
)

// cmdServe implements `serve fixture --key <KEY>`.
func cmdServe(args []string, selfExe string) int {
	if len(args) == 0 || (args[0] != "fixture" && args[0] != "codex") {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay serve fixture|codex --key <KEY> [--model M] [--mode S]")
		return 2
	}
	harness := args[0]
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	key := fs.String("key", "", "session key")
	model := fs.String("model", "", "model id (codex)")
	mode := fs.String("mode", "", "service tier / mode (codex)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay serve fixture|codex --key <KEY> [--model M] [--mode S]")
		return 2
	}
	if !session.ValidKey(*key) {
		fmt.Fprintf(os.Stderr, "reposuite-relay: invalid session key %q\n", *key)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var resp api.SessionResponse
	if harness == "codex" {
		resp, err = c.CreateCodex(ctx, *key, cwd, *model, *mode)
	} else {
		resp, err = c.ServeFixture(ctx, *key, cwd)
	}
	if err != nil {
		return failAPI(err)
	}
	printSession(resp.Session)
	fmt.Printf("daemon_pid=%d\n", resp.Daemon.PID)
	return 0
}

// cmdPrompt submits a prompt as a native turn (codex sessions).
func cmdPrompt(args []string, selfExe string) int {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	text := fs.String("text", "", "prompt text")
	model := fs.String("model", "", "per-turn model override")
	effort := fs.String("effort", "", "per-turn reasoning effort override")
	if err := fs.Parse(splitKeyFirst(args)); err != nil || fs.NArg() != 1 || *text == "" {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay prompt <KEY> --text <TEXT> [--model M] [--effort E]")
		return 2
	}
	key := fs.Arg(0)
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.Prompt(ctx, key, *text, *model, *effort)
	if err != nil {
		return failAPI(err)
	}
	fmt.Printf("key=%s turn_id=%s native_thread_id=%s runtime_id=%s\n",
		resp.Key, resp.TurnID, resp.NativeThreadID, resp.RuntimeID)
	return 0
}

// cmdCancel interrupts the in-flight turn of a session.
func cmdCancel(args []string, selfExe string) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	if err := fs.Parse(splitKeyFirst(args)); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay cancel <KEY>")
		return 2
	}
	key := fs.Arg(0)
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.Cancel(ctx, key)
	if err != nil {
		return failAPI(err)
	}
	fmt.Printf("key=%s cancelled=1\n", resp.Key)
	return 0
}

// cmdInput answers a native requested-input request. Answers are supplied
// as QUESTION_ID=ANSWER pairs (repeatable).
func cmdInput(args []string, selfExe string) int {
	fs := flag.NewFlagSet("input", flag.ContinueOnError)
	inputID := fs.String("input", "", "requested input id")
	var pairs multiFlag
	fs.Var(&pairs, "answer", "questionId=answer (repeatable)")
	if err := fs.Parse(splitKeyFirst(args)); err != nil || fs.NArg() != 1 || *inputID == "" || len(pairs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay input <KEY> --input <ID> --answer <QID>=<ANSWER> [--answer ...]")
		return 2
	}
	key := fs.Arg(0)
	answers := make([]api.InputAnswer, 0, len(pairs))
	for _, p := range pairs {
		qid, ans, found := strings.Cut(p, "=")
		if !found || qid == "" || ans == "" {
			fmt.Fprintf(os.Stderr, "reposuite-relay: bad answer %q, want questionId=answer\n", p)
			return 2
		}
		answers = append(answers, api.InputAnswer{QuestionID: qid, Answers: []string{ans}})
	}
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.AnswerInput(ctx, key, *inputID, answers)
	if err != nil {
		return failAPI(err)
	}
	fmt.Printf("key=%s input_id=%s answered=1\n", resp.Key, resp.InputID)
	return 0
}

// cmdConfig changes the accepted model/mode of a session.
func cmdConfig(args []string, selfExe string) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	model := fs.String("model", "", "model id")
	mode := fs.String("mode", "", "service tier / mode")
	if err := fs.Parse(splitKeyFirst(args)); err != nil || fs.NArg() != 1 || (*model == "" && *mode == "") {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay config <KEY> [--model M] [--mode S]")
		return 2
	}
	key := fs.Arg(0)
	var m, md *string
	if *model != "" {
		m = model
	}
	if *mode != "" {
		md = mode
	}
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.SetConfig(ctx, key, m, md)
	if err != nil {
		return failAPI(err)
	}
	printSession(resp.Session)
	return 0
}

// cmdList implements `list` — ensures the daemon, lists durable sessions
// (COLD sessions included).
func cmdList(selfExe string) int {
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.Sessions(ctx)
	if err != nil {
		return failAPI(err)
	}
	for i := range resp.Sessions {
		printSession(resp.Sessions[i])
	}
	fmt.Printf("daemon_pid=%d sessions=%d\n", resp.Daemon.PID, resp.Daemon.SessionCount)
	return 0
}

// cmdStatus implements `status <KEY>`.
func cmdStatus(selfExe, key string) int {
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.Status(ctx, key)
	if err != nil {
		return failAPI(err)
	}
	printSession(resp.Session)
	fmt.Printf("daemon_pid=%d\n", resp.Daemon.PID)
	return 0
}

// cmdStop implements `stop <KEY>` — stop the session runtime and delete
// the durable RelaySession.
func cmdStop(selfExe, key string) int {
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.StopSession(ctx, key); err != nil {
		return failAPI(err)
	}
	fmt.Printf("stopped key=%s\n", key)
	return 0
}

func printSession(s api.SessionInfo) {
	fmt.Printf("key=%s sessionId=%s runtimeId=%s runtimeState=%s harness=%s state=%s generation=%d pid=%d cwd=%s createdAt=%s generationStartedAt=%s\n",
		s.Key, s.SessionID, s.RuntimeID, s.RuntimeState, s.Harness, s.State,
		s.Generation, s.PID, s.Cwd, s.CreatedAt, s.GenerationStartedAt)
}
