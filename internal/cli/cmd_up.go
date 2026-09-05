package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/paths"
	"github.com/jdmorlan/job-engine/internal/service"
	"github.com/jdmorlan/job-engine/internal/worker"
)

func init() {
	register(&Command{
		Name:  "up",
		Usage: "bring the whole engine up here, the way it is meant to run",
		Long: "One command for a working engine: a control plane, a worker attached to\n" +
			"it, and the web client.\n\n" +
			"The defaults are the deployment, not a demonstration of one. The control\n" +
			"plane runs as a container, because it owns a database and should survive\n" +
			"a reboot without depending on a terminal staying open. The worker runs as\n" +
			"a native service, because a container worker can only run what is in its\n" +
			"image and yours needs the tools on this machine. The web client runs as a\n" +
			"container, because it is stateless and there is nothing to gain by\n" +
			"treating it differently.\n\n" +
			"This replaces the old `je quickstart`, which ran a control plane and a\n" +
			"worker in your terminal. That shape is still here as --foreground, but it\n" +
			"is no longer what you get by default: a quick start that teaches you a\n" +
			"deployment you would never actually use is not a quick start, it is a\n" +
			"detour.\n\n" +
			"Safe to run again. Anything already up is left alone and reported as\n" +
			"such, so this is also how you bring back the piece that died.",
		Local: true,
		Run:   runUp,
	})

	register(&Command{
		Name:  "down",
		Usage: "stop everything running here; data and history are kept",
		Long: "The inverse of `je up`: it stops and unregisters the control plane, the\n" +
			"worker and the web client, in the order that leaves nothing talking to\n" +
			"something that has gone.\n\n" +
			"It removes processes and registrations, never data. Your database, run\n" +
			"history, secrets, certificate authority and this machine's identity are\n" +
			"all still there afterwards, and `je up` brings the engine back to them.\n\n" +
			"For the other thing -- a clean slate, data included -- that is `je reset`,\n" +
			"which says so and asks first.",
		Local: true,
		Run:   runDown,
	})
}

// upComponents is the fixed set this command brings up, in dependency order.
//
// The column they are reported in is padded to the widest of these rather than
// measured at the end, because `je up` can spend a minute pulling an image and
// a progress report that arrives all at once when the work is over is not a
// progress report.
var upComponents = []string{"control plane", "worker", "web"}

func runUp(ctx context.Context, env *Env, args []string) error {
	cmd := commands["up"]
	fs := newFlagSet(cmd, env)
	addr := fs.String("addr", daemon.DefaultAddr, "address for the control plane")
	webAddr := fs.String("web-addr", defaultWebAddr, "address for the web client")
	labels := fs.String("labels", jobdef.DefaultRunsOn, "labels for the worker")
	foreground := fs.Bool("foreground", false,
		"run the control plane and a worker in this terminal instead, registering nothing")
	noWeb := fs.Bool("no-web", false, "do not start the web client")
	native := fs.Bool("native", false,
		"run the control plane as a native service rather than a container")
	useDocker := fs.Bool("docker", false, "run the control plane as a container (the default)")
	printOnly := fs.Bool("print", false, "print what would be done, and do nothing")
	verbose := fs.Bool("v", false, "log at debug level (--foreground only)")
	githubAPI := fs.String("github-api", "", "GitHub API base URL, for GitHub Enterprise")
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	if *foreground {
		return upForeground(ctx, env, *addr, *labels, *githubAPI, *verbose)
	}

	st := env.Style
	report := reporter(env, upComponents)

	// The control plane's shape, decided before anything is done, so that a
	// contradiction in the flags is a usage error rather than a half-built
	// deployment.
	mode := installMode{docker: *useDocker, native: *native, printOnly: *printOnly}

	// Asking for a container over a deployment that is already here is the one
	// case worth refusing outright. It is a reasonable thing to want and there
	// is no honest way to do it silently: the container starts from an empty
	// volume, and the history somebody spent months accumulating stays on disk,
	// invisible to the engine they are now talking to.
	if mode.docker && hostedHere(env.Layout) {
		return fmt.Errorf(
			"there is already a control plane's data in %s, and a containerised one\n"+
				"would not use it -- it keeps its database in a named volume, so this\n"+
				"would start an empty engine beside your history rather than moving it.\n\n"+
				"  %-24s keep the deployment you have\n"+
				"  %-24s move that data aside first, and start fresh in a container\n\n"+
				"Nothing has been changed.",
			env.Layout.Data,
			"je up", "je reset")
	}

	if *printOnly {
		return upPrint(ctx, env, *addr, *webAddr, splitLabels(*labels), *noWeb, mode)
	}

	// Control plane first: the worker enrolls against it and the web client
	// talks to it, so neither has anything to attach to until it answers.
	shape, notes, err := bringUpControlPlane(ctx, env, *addr, mode)
	switch {
	case errors.Is(err, errAttention):
		// Registered, and not answering. The installer has already said so and
		// named the log; adding "could not bring up the control plane" on top
		// would be a second, worse account of the same event -- and a wrong
		// one, because it did get set up.
		report("control plane", st.Warn("registered, not answering"), *addr)
		replay(env, notes)
		return errAttention
	case err != nil:
		return err
	}
	report("control plane", shape, *addr)
	replay(env, notes)

	// A worker, native by design. `je control-plane install` deliberately does
	// not attach one (D20/C11) and the result is an engine that looks healthy
	// and runs nothing -- which is the single most common way to end up
	// confused, and the whole reason this command exists.
	attention := false
	shape, notes, err = bringUpWorker(ctx, env, *addr, splitLabels(*labels))
	switch {
	case errors.Is(err, errAttention):
		// Unlike the control plane, this does not stop the bring-up: the
		// engine is up and the web client will work, there is just nothing to
		// execute yet. Worth exiting 3 over, not worth abandoning the rest.
		report("worker", st.Warn("registered, not attached"), *labels)
		attention = true
	case err != nil:
		return err
	default:
		report("worker", shape, *labels)
	}
	replay(env, notes)

	if *noWeb {
		return finishUp(env, *webAddr, false, attention)
	}
	shape, detail, err := bringUpWeb(ctx, env, *webAddr)
	if err != nil {
		// The engine is up. A web client that cannot start is a missing view
		// of a working system, not a failed bring-up, and reporting it as a
		// failure would send somebody looking for a problem they do not have.
		report("web", st.Warn("not started"), "")
		fmt.Fprintf(env.Stderr, "\n%s\n%s\n",
			env.ErrStyle.Warn("the web client did not start:"), indent(err.Error(), "  "))
		return finishUp(env, *webAddr, false, attention)
	}
	report("web", shape, detail)
	return finishUp(env, *webAddr, true, attention)
}

// upPrint writes out everything `je up` would do, and does none of it.
//
// A separate path rather than a flag threaded through the bring-up, because
// the two have nothing in common past the plan: there is no "already running"
// to detect, no component to report the state of, and no control plane to ask
// where the web client should point. Sharing the code meant printing status
// lines about containers that do not exist, which is worse than not having the
// flag.
func upPrint(ctx context.Context, env *Env, addr, webAddr string, labels []string, noWeb bool, mode installMode) error {
	mode = defaultedMode(env, mode)
	if _, err := chooseMode(env, mode); err != nil {
		return err
	}
	if err := installControlPlane(ctx, env, addr, mode); err != nil {
		return err
	}
	if err := joinWorker(ctx, env, workerJoin{
		name:   defaultWorkerName(),
		addr:   dialable(addr),
		labels: labels,
		mode:   installMode{native: true, printOnly: true},
	}); err != nil {
		return err
	}
	if noWeb {
		return nil
	}
	// Told where the control plane will be rather than asking where one is:
	// printing a plan must not require the thing the plan would create.
	base, err := webTarget(env, addr)
	if err != nil {
		return err
	}
	return startWebContainer(ctx, env, webAddr, base, true)
}

// bringUpControlPlane installs one if none is answering, and says which shape
// it ended up in.
func bringUpControlPlane(ctx context.Context, env *Env, addr string, mode installMode) (string, string, error) {
	st := env.Style
	if controlPlaneAnswers(ctx, env) {
		// Before the shape is decided, deliberately: there is nothing to
		// install, so an explanation of how it would have been installed is a
		// paragraph about a decision that was never taken.
		return st.Good("already running"), "", nil
	}

	mode = defaultedMode(env, mode)
	kind, err := chooseMode(env, mode)
	if err != nil {
		return "", "", err
	}
	c, err := quietly(env, func(sub *Env) error {
		return installControlPlane(ctx, sub, addr, mode)
	})
	if err != nil {
		return "", c.notes, stepFailed(env, "the control plane", c.out, err)
	}
	if kind == modeDocker {
		return st.Good("running as a container"), c.notes, nil
	}
	return st.Good("registered as a service"), c.notes, nil
}

// bringUpWorker attaches one to the control plane, as a native service.
func bringUpWorker(ctx context.Context, env *Env, addr string, labels []string) (string, string, error) {
	st := env.Style
	if mgr, err := service.New(service.Worker); err == nil {
		if state, err := mgr.Status(); err == nil && state.Installed {
			return st.Good("already attached"), "", nil
		}
	}

	c, err := quietly(env, func(sub *Env) error {
		return joinWorker(ctx, sub, workerJoin{
			name:   defaultWorkerName(),
			addr:   dialable(addr),
			labels: labels,
			// Native, and not from chooseMode's default: the reason is
			// specific to a worker rather than a matter of taste. The
			// published image is FROM scratch, so a container worker can only
			// run jobs that need nothing but themselves -- which is not the
			// worker somebody brings up to run their own jobs.
			mode: installMode{native: true},
		})
	})
	if err != nil {
		return "", c.notes, stepFailed(env, "the worker", c.out, err)
	}
	return st.Good("registered as a service"), c.notes, nil
}

// bringUpWeb starts the web client's container, and returns where it is.
func bringUpWeb(ctx context.Context, env *Env, addr string) (string, string, error) {
	st := env.Style
	host, port := splitHostPort(addr)
	if host == "" {
		host = "127.0.0.1"
	}
	where := fmt.Sprintf("http://%s:%s", host, port)

	if containerExists(ctx, "web") {
		return st.Good("already running"), where, nil
	}

	base, err := webTarget(env, "")
	if err != nil {
		return "", "", err
	}
	if _, err := quietly(env, func(sub *Env) error {
		return startWebContainer(ctx, sub, addr, base, false)
	}); err != nil {
		return "", "", err
	}
	return st.Good("running as a container"), where, nil
}

// finishUp prints what to do next and settles the exit code.
//
// Attention (exit 3) rather than failure: everything that could be brought up
// was, and something registered is not answering. P1 asks that "does anything
// need my attention?" be a question with an exit code, and a bring-up that
// half-worked is exactly that question.
func finishUp(env *Env, webAddr string, web, attention bool) error {
	if err := upNextSteps(env, webAddr, web); err != nil {
		return err
	}
	if attention {
		return errAttention
	}
	return nil
}

// upNextSteps is the two or three lines worth reading after a bring-up.
func upNextSteps(env *Env, webAddr string, web bool) error {
	st := env.Style
	fmt.Fprintln(env.Stdout)
	tw := env.table()
	if web {
		host, port := splitHostPort(webAddr)
		if host == "" {
			host = "127.0.0.1"
		}
		fmt.Fprintf(tw, "  %s\t%s\n", st.Cmd(fmt.Sprintf("open http://%s:%s", host, port)),
			st.Muted("history, chains and schedules in a browser"))
	}
	fmt.Fprintf(tw, "  %s\t%s\n", st.Cmd("je status"), st.Muted("is it up, and is anything attached"))
	fmt.Fprintf(tw, "  %s\t%s\n", st.Cmd("je source add <name> <owner/repo>"),
		st.Muted("point it at your jobs"))
	fmt.Fprintf(tw, "  %s\t%s\n", st.Cmd("je down"), st.Muted("stop all of it; data and history stay"))
	return tw.Flush()
}

func runDown(ctx context.Context, env *Env, args []string) error {
	cmd := commands["down"]
	fs := newFlagSet(cmd, env)
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	st := env.Style
	report := reporter(env, upComponents)
	stopped := 0

	// The reverse of the order `je up` uses. Taking the control plane first
	// would leave a worker claiming against nothing and a web client rendering
	// connection errors, which looks like a broken teardown rather than a
	// finished one.
	if removed, err := stopContainer(ctx, "web"); err != nil {
		return err
	} else if removed {
		report("web", st.Muted("stopped"), "container removed")
		stopped++
	} else {
		report("web", st.Muted("not running"), "")
	}

	for _, c := range []service.Component{service.Worker, service.ControlPlane} {
		label := "worker"
		if c == service.ControlPlane {
			label = "control plane"
		}
		res, err := quietly(env, func(sub *Env) error { return removeComponent(ctx, sub, c) })
		if err != nil {
			return stepFailed(env, label, res.out, err)
		}
		if strings.Contains(stripANSI(res.out), "nothing to remove") {
			report(label, st.Muted("not running"), "")
			continue
		}
		report(label, st.Muted("stopped"), "unregistered")
		stopped++
	}

	if stopped == 0 {
		fmt.Fprintf(env.Stdout, "\n%s\n", st.Muted("nothing was running here."))
		return nil
	}

	// Said once, at the end, rather than by each component as it goes. It is
	// the sentence somebody tearing down a deployment actually wants, and
	// three copies of it reads as protesting too much.
	fmt.Fprintf(env.Stdout, "\n%s\n  %s\n\n  %s   %s\n",
		st.Muted("Left alone: your data directory, job definitions, run history and secrets."),
		st.Muted(env.Layout.Data),
		st.Cmd("je up"), st.Muted("brings it back"))
	return nil
}

// upForeground is the whole engine in this terminal: a control plane, and a
// worker that talks to it over the same API a worker anywhere else would use.
//
// This was `je quickstart`, and it is a flag now rather than a command because
// of what having it as a command implied. It is genuinely useful -- it is the
// fastest way to watch the two halves interact, and it is what the tests and a
// GitHub Enterprise smoke test reach for -- but it is not how anybody should
// run this, and a command called "quickstart" told every new reader that it
// was. Nothing here is a third mode: the transport is the same HTTPS, the
// worker enrolls against the control plane's own authority, and nothing works
// here that would not work split across two machines.
func upForeground(ctx context.Context, env *Env, addr, labels, githubAPI string, verbose bool) error {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ready := make(chan struct{})
	planeDone := make(chan error, 1)
	go func() {
		planeDone <- daemon.Run(ctx, daemon.Config{
			Layout:    env.Layout,
			Addr:      addr,
			Version:   env.Version,
			Logger:    logger,
			Ready:     ready,
			GitHubAPI: githubAPI,
		})
	}()

	// Wait for the listener before dialling. Racing it would mean the worker's
	// first few claims fail with connection refused, which looks like a broken
	// install on the very first command somebody runs.
	select {
	case <-ready:
	case err := <-planeDone:
		if err != nil {
			return err
		}
		return fmt.Errorf("the control plane stopped before it was ready")
	case <-ctx.Done():
		return <-planeDone
	}

	// Resolved from the runtime file rather than from --addr, so that a port of
	// 0 (which the OS picks) reaches the port it actually chose.
	info, err := daemon.ReadRuntime(env.Layout.Runtime())
	if err != nil {
		cancel()
		<-planeDone
		return err
	}

	// The local case, so the worker enrolls itself from the token the control
	// plane just wrote into the data directory they share -- no token to paste
	// and no step to explain (D25).
	//
	// Fatal now, where it used to be a warning. It could be a warning while a
	// worker without an identity still had a plaintext socket to fall back to;
	// with the flip there is nothing behind it, so carrying on would print
	// "one worker attached" and then fail every claim.
	if err := autoEnroll(ctx, env, dialable(info.Address), defaultWorkerName(), splitLabels(labels), nil); err != nil {
		cancel()
		<-planeDone
		return fmt.Errorf("enrolling the worker this command starts: %w", err)
	}

	client, err := dialControlPlane(env, dialable(info.Address))
	if err != nil {
		cancel()
		<-planeDone
		return err
	}
	w, err := worker.New(worker.Options{
		Name:   defaultWorkerName(),
		Labels: splitLabels(labels),
		// The same two paths `je worker run` passes. Leaving them to the
		// worker's own defaults put its age key under <data>/cache while
		// `je worker keygen` wrote <data>/identity, so a secret encrypted to
		// the key you had just created could not be read here (D25).
		DataDir:      env.Layout.Data,
		CacheDir:     env.Layout.Data,
		ToolchainBin: env.Layout.ToolchainBin(),
		IdentityFile: env.Layout.AgeIdentity(),
		Version:      env.Version,
		Client:       client,
		Logger:       logger,
	})
	if err != nil {
		cancel()
		<-planeDone
		return err
	}

	est := env.ErrStyle
	fmt.Fprintf(env.Stderr, "\n%s %s, one worker attached %s\n%s\n\n%s\n\n",
		est.Muted("je: control plane on"), info.Address, est.Muted("("+labels+")"),
		est.Muted("    Nothing is registered and this stops when you do."+
			" `je up` deploys it properly."),
		"    try:  "+est.Cmd("je jobs")+est.Muted("   in another terminal"))

	workerDone := make(chan error, 1)
	go func() { workerDone <- w.Run(ctx) }()

	// Either half stopping takes the other down. A control plane with no worker
	// runs nothing (C11) and a worker with no control plane has nothing to do,
	// so leaving one of them alive would only produce a system that looks up
	// and is not.
	select {
	case err := <-workerDone:
		cancel()
		<-planeDone
		return err
	case err := <-planeDone:
		cancel()
		<-workerDone
		return err
	}
}

// hostedHere reports whether a control plane has already run against this data
// directory as an ordinary process -- natively, or in the foreground.
//
// The CA directory is the tell. A control plane generates it beside the
// database it is using, so its presence means one has run with *this* directory
// as its data directory. A containerised control plane generates the same thing
// inside its own volume, where this cannot see it, which is exactly the
// distinction being drawn.
func hostedHere(l paths.Layout) bool {
	for _, path := range []string{l.CACert(), l.StateDB()} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// defaultedMode fills in `je up`'s own default -- a containerised control
// plane -- and says so out loud when it cannot have one.
//
// The default is stated here rather than inherited from chooseMode, which
// defaults the other way: installing one component on purpose is a different
// question from bringing a whole engine up, and the right answer differs.
func defaultedMode(env *Env, mode installMode) installMode {
	if mode.docker || mode.native {
		return mode
	}

	// An engine that is already here keeps the shape it has.
	//
	// This is the more important half of "safe to run again". A containerised
	// control plane keeps its database in a named volume, so moving an existing
	// native deployment into a container does not migrate it -- it starts an
	// empty one beside it. Every run, every cursor and every secret is still on
	// disk and none of it is in the engine that is now running, which reads as
	// data loss and is the worst thing this command could do quietly.
	//
	// So the default applies to a machine with no engine on it. A machine that
	// already has one is not asking a question about defaults.
	if hostedHere(env.Layout) {
		mode.native = true
		return mode
	}

	ok, why := canRunContainers(env)
	mode.docker, mode.native = ok, !ok
	if why != "" {
		// Not a failure, and not silent either. A machine that cannot run this
		// build in a container still has a service manager, and refusing to
		// bring the engine up over a preference would be choosing the shape
		// over the point of it. But a default that quietly becomes its
		// opposite is worse than one you disagree with, so it says which it
		// took and why.
		fmt.Fprintf(env.Stderr, "%s\n\n", env.ErrStyle.Muted(
			"je: "+why+", so the control plane goes in as a native service.\n"+
				"    `je up --docker` moves it once that is sorted."))
	}
	return mode
}

// canRunContainers reports whether this build can actually be run as one, and
// says what is missing when it cannot.
//
// Two separate ways to fail and both end in the same place. Docker may not be
// installed; or Docker is fine and this is a development build, which has no
// published image to run -- the case every contributor hits the first time
// they try their own binary, and the one that would otherwise make `je up`
// look broken on the machine it was built on.
func canRunContainers(env *Env) (bool, string) {
	if err := dockerAvailable(); err != nil {
		return false, "no Docker here"
	}
	if imageRef(env.Version) == "" {
		return false, "this is a " + env.Version + " build, which has no published image"
	}
	return true, ""
}

// controlPlaneAnswers reports whether one is already up and reachable.
//
// "Answers", not "is registered": a container that exists and a service that is
// enabled both prove somebody once asked for a control plane, and neither
// proves there is one to talk to now. Bringing the engine up on top of a
// registration whose process died is exactly the case this command is for.
func controlPlaneAnswers(ctx context.Context, env *Env) bool {
	client, err := Connect(env.Layout)
	if err != nil {
		return false
	}
	return reachable(ctx, client)
}

// upStates is every state these two commands can report.
//
// Declared rather than discovered so the columns can be padded before the
// first line is printed. Add a state here when you add one below; the cost of
// forgetting is a ragged column, not a wrong answer.
var upStates = []string{
	"already running", "already attached", "running as a container",
	"registered as a service", "not started", "stopped", "not running",
}

// reporter returns the line-per-component printer both commands use.
//
// Progressive rather than a table at the end, because bringing an engine up
// can spend a minute pulling an image, and a report that arrives all at once
// when the work is already over tells you nothing while you are waiting. Both
// columns are padded to the widest value these commands know how to print,
// which is what lets the lines align without having seen them all first --
// the one thing an ordinary table gets for free and this cannot.
func reporter(env *Env, labels []string) func(label, state, detail string) {
	pad := func(s string, width int) string {
		if n := width - displayWidth(s); n > 0 {
			return s + strings.Repeat(" ", n)
		}
		return s
	}
	labelWidth := widest(labels)
	stateWidth := widest(upStates)

	st := env.Style
	return func(label, state, detail string) {
		line := pad(st.Header(label), labelWidth) + "  "
		if detail == "" {
			// Nothing to its right, so nothing to line up with -- and padding
			// it would leave trailing spaces on the line.
			fmt.Fprintln(env.Stdout, line+state)
			return
		}
		fmt.Fprintln(env.Stdout, line+pad(state, stateWidth)+"  "+st.Muted(detail))
	}
}

func widest(values []string) int {
	width := 0
	for _, v := range values {
		if n := displayWidth(v); n > width {
			width = n
		}
	}
	return width
}

// captured is what one step printed while `je up` was holding its output.
type captured struct {
	out   string // its own report, which the status line replaces
	notes string // what it said on stderr, which is worth keeping
}

// quietly runs one step with its output captured.
//
// The per-component installers each print a small report of their own, which
// is right when you ran one of them on purpose and is three stacked reports
// when `je up` ran all three. Captured rather than discarded: a step that
// fails has usually said something useful on the way, and throwing that away
// to keep the happy path tidy is how a clean-looking command becomes an
// undebuggable one.
//
// stderr is captured for a different reason: to reorder it. An installer that
// registers something and then finds it is not answering says so immediately,
// which lands before `je up` has printed the line saying which component it
// was -- so the explanation arrives ahead of the thing it explains. Holding it
// for a moment costs nothing, because none of it streams: no step here writes
// progress to stderr, only conclusions.
func quietly(env *Env, step func(*Env) error) (captured, error) {
	var out, notes bytes.Buffer
	sub := *env
	sub.Stdout = &out
	sub.Stderr = &notes
	err := step(&sub)
	return captured{out: out.String(), notes: notes.String()}, err
}

// replay prints what a step said, after its status line rather than before.
func replay(env *Env, notes string) {
	if s := strings.TrimRight(notes, "\n"); s != "" {
		fmt.Fprintf(env.Stderr, "%s\n", s)
	}
}

// stepFailed decides what a step's error deserves on the way out.
//
// errAttention is not a failure and must not be dressed as one: the installer
// returns it when the component was set up and then did not answer, having
// already printed that and named its log. It goes back untouched, for the
// caller to report in the component's own status line.
//
// A real error gets the banner and whatever the step managed to print before
// it failed -- which `je up` captured to keep the happy path quiet, and which
// would otherwise be the useful half of the diagnosis, thrown away for tidiness.
func stepFailed(env *Env, what, out string, err error) error {
	if errors.Is(err, errAttention) {
		return err
	}
	fmt.Fprintf(env.Stderr, "\n%s\n", env.ErrStyle.Bad("could not bring up "+what+":"))
	if s := strings.TrimSpace(out); s != "" {
		fmt.Fprintf(env.Stderr, "%s\n", indent(s, "  "))
	}
	return err
}

// indent prefixes every line, including the ones after the first.
//
// Multi-line errors are the norm here -- most of them end in a command to run
// -- and indenting only the first line leaves the rest hanging at the margin,
// where it reads as a separate message rather than as part of this one.
func indent(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
