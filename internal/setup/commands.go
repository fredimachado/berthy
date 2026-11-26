package setup

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// CommandLog is what a run sends to the server, step by step. With a writer it prints each script as it goes: the script before
// it is sent, and its exit status after. It keeps the scripts of the step that is running, which the step's result reports as
// its commands, so the log and the JSON report cannot disagree. It holds scripts only: never the stdin a script reads, and never
// what a script prints. A nil CommandLog records nothing, which is what a run that is not verbose has.
type CommandLog struct {
	w      io.Writer
	style  CommandLogStyle
	step   string   // the step whose scripts are recorded
	headed bool     // whether the step's header has been printed
	cmds   []string // the scripts of step, in order
}

// CommandLogStyle decorates the prompt and exit status without changing recorded scripts.
type CommandLogStyle struct {
	Prompt func(string) string
	Exit   func(int, string) string
}

// NewStyledCommandLog prints command markers using the optional callbacks.
func NewStyledCommandLog(w io.Writer, s CommandLogStyle) *CommandLog {
	return &CommandLog{w: w, style: s}
}

// NewCommandLog is a log that prints to w.
func NewCommandLog(w io.Writer) *CommandLog { return &CommandLog{w: w} }

// begin makes step the one the scripts that follow belong to.
func (l *CommandLog) begin(step string) {
	if l == nil {
		return
	}
	l.step, l.headed, l.cmds = step, false, nil
}

// end returns the scripts of the step that just ran, and starts the next step with none. It is nil only for a nil log; a step
// that sent nothing gets an empty list.
func (l *CommandLog) end() *[]string {
	if l == nil {
		return nil
	}
	cmds := append([]string{}, l.cmds...)
	l.cmds = nil
	return &cmds
}

// Would records a script that a dry run does not send. It is listed among the step's commands and marked as not run.
func (l *CommandLog) Would(script string) {
	if l == nil {
		return
	}
	l.show(script)
	fmt.Fprintln(l.w, "→ not run: dry run")
}

// sending records script and prints it, before it is sent.
func (l *CommandLog) sending(script string) {
	if l == nil {
		return
	}
	l.show(script)
}

// exited prints how the script that was sent ended: its exit code, or why it could not run at all.
func (l *CommandLog) exited(res Result, err error) {
	if l == nil {
		return
	}
	if err != nil {
		if l.step == "cli_key" {
			fmt.Fprintln(l.w, "→ could not run")
			return
		}
		fmt.Fprintf(l.w, "→ could not run: %s\n", clean(err.Error()))
		return
	}
	line := fmt.Sprintf("→ exit %d", res.Code)
	if l.style.Exit != nil {
		line = l.style.Exit(res.Code, line)
	}
	fmt.Fprintln(l.w, line)
}

// show records script as a command of the current step and prints it, under the step's header when it is the step's first.
// Every line of the script is printed behind "$ ", so the log's own lines stay apart from the script's.
func (l *CommandLog) show(script string) {
	l.cmds = append(l.cmds, script)
	if !l.headed {
		fmt.Fprintf(l.w, "# %s\n", l.step)
		l.headed = true
	}
	prompt := "$"
	if l.style.Prompt != nil {
		prompt = l.style.Prompt(prompt)
	}
	for _, line := range strings.Split(strings.TrimSuffix(script, "\n"), "\n") {
		if line == "" {
			fmt.Fprintln(l.w, prompt)
		} else {
			fmt.Fprintf(l.w, "%s %s\n", prompt, line)
		}
	}
}

// Recording returns a Runner that runs scripts through r and logs each one in log: before it is sent, and its exit status after.
// Everything else passes through, SetSudo included, so Connect and the steps see the same runner they would without it.
func Recording(r Runner, log *CommandLog) Runner { return recording{Runner: r, log: log} }

type recording struct {
	Runner
	log *CommandLog
}

func (r recording) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	r.log.sending(script)
	res, err := r.Runner.Run(ctx, script, stdin)
	r.log.exited(res, err)
	return res, err
}

func (r recording) SetSudo(v bool) {
	if s, ok := r.Runner.(SudoSetter); ok {
		s.SetSudo(v)
	}
}
