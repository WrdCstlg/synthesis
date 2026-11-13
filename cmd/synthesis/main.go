// Command synthesis is the minimal CLI for the PRO-THESIS public demonstration.
//
// It exposes three subcommands only:
//   - synthesis run
//   - synthesis verify-lock
//   - synthesis explain
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"synthesis.dev/synthesis/internal/demo"
	"synthesis.dev/synthesis/pkg/schema"
)

func main() {
	os.Exit(int(dispatch(os.Args[1:], os.Stdout, os.Stderr)))
}

func dispatch(args []string, stdout, stderr io.Writer) schema.ExitCode {
	if len(args) == 0 {
		printUsage(stderr)
		return schema.ExitConfigError
	}
	switch args[0] {
	case "-h", "--help", "help":
		printUsage(stdout)
		return schema.ExitPass
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "verify-lock":
		return cmdVerifyLock(args[1:], stdout, stderr)
	case "explain":
		return cmdExplain(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "synthesis: unknown command %q (available: run, verify-lock, explain)\n", args[0])
		printUsage(stderr)
		return schema.ExitConfigError
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: synthesis <command> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  run          Execute a locked chaos test profile and emit verdict.json")
	fmt.Fprintln(w, "  verify-lock  Verify .synthesis/lock against synthesis.yaml and oracle definitions")
	fmt.Fprintln(w, "  explain      Render a human-readable summary of a verdict.json file")
}

func cmdRun(args []string, stdout, stderr io.Writer) schema.ExitCode {
	fs := flag.NewFlagSet("synthesis run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.String("project", ".", "path to project directory containing synthesis.yaml")
	profile := fs.String("profile", "buggy", "profile name in synthesis.yaml")
	seed := fs.Uint64("seed", 0, "base PRNG seed")
	worlds := fs.Int("worlds", 0, "override world count (narrowing below lock yields INCONCLUSIVE)")
	budget := fs.Duration("budget", 0, "override wall-clock budget (narrowing below lock yields INCONCLUSIVE)")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return schema.ExitPass
		}
		return schema.ExitConfigError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "synthesis run: unexpected positional arguments: %v\n", fs.Args())
		return schema.ExitConfigError
	}

	seedSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seedSet = true
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sum, err := demo.ExecuteDemoRun(ctx, demo.DemoRunOptions{
		ProjectDir:     *project,
		ProfileName:    *profile,
		Seed:           *seed,
		SeedSet:        seedSet,
		WorldsOverride: *worlds,
		BudgetOverride: time.Duration(*budget),
		Stdout:         stdout,
		Stderr:         stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "synthesis run: %v\n", err)
		if sum != nil {
			return sum.ExitCode
		}
		return schema.ExitConfigError
	}
	return sum.ExitCode
}

func cmdVerifyLock(args []string, stdout, stderr io.Writer) schema.ExitCode {
	fs := flag.NewFlagSet("synthesis verify-lock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.String("project", ".", "path to project directory containing synthesis.yaml")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return schema.ExitPass
		}
		return schema.ExitConfigError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "synthesis verify-lock: unexpected positional arguments: %v\n", fs.Args())
		return schema.ExitConfigError
	}
	return demo.VerifyProjectLock(*project, stdout, stderr)
}

func cmdExplain(args []string, stdout, stderr io.Writer) schema.ExitCode {
	fs := flag.NewFlagSet("synthesis explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return schema.ExitPass
		}
		return schema.ExitConfigError
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: synthesis explain <path/to/verdict.json>")
		return schema.ExitConfigError
	}
	if err := demo.ExplainVerdictFile(fs.Arg(0), stdout); err != nil {
		fmt.Fprintf(stderr, "synthesis explain: %v\n", err)
		return schema.ExitConfigError
	}
	return schema.ExitPass
}
